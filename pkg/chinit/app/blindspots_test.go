// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// This file pins the logging layer the blind-spot fixes ride on:
//
// - the central singleton's handler (the logger package's own specs
// pin the level-split routing: INFO to stdout, WARN/ERROR to stderr);
// - the drain's peer-fallback WARNING arrives as ONE structured
// record through the singleton — the "why" and the zone are
// attributes, not buried in the message;
// - runLeaseRenew emits its attempt + outcome records.

// captureSlog swaps the central logger's handler for a buffer-backed
// one and returns the buffer plus the restore func. The seam is
// logger.SetLogHandler — the one configuration point the singleton
// has (no slog.SetDefault here: the singleton IS the default).
func captureSlog() (*bytes.Buffer, func()) {
	buf := &bytes.Buffer{}
	logger.SetLogHandler(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf, logger.Init
}

var _ = Describe("drain fallback logging", func() {
	It("emits the peer-fallback warning through slog with the why as an attribute", func() {
		// The drain's degraded path: live discovery cannot answer (the
		// bare provider has no lister), the fallback peer is
		// writer-0, and BOTH contracts hold — the raw WARNING line the
		// journal greps and the structured record the log pipeline
		// consumes.
		buf, restore := captureSlog()
		DeferCleanup(restore)

		st := drainer()
		spec := &inventorySpec{
			WriterMax:        4,
			ReaderMax:        10,
			ReaderHostPrefix: "reader-",
		}
		peer := selectDrainPeer(context.Background(), st, spec, nil, "azure")
		Expect(peer).NotTo(BeNil())
		Expect(peer.Hostname).To(Equal("writer-0"))

		log := buf.String()
		Expect(log).To(ContainSubstring("WARNING: drain peer fell back to writer-0"))
		Expect(log).To(ContainSubstring("weekly dead-replica GC is the safety net"))
		// The structured "why": the no-capability cause is an
		// attribute, queryable — never only embedded in the message.
		Expect(log).To(ContainSubstring("why="))
		Expect(log).To(ContainSubstring("no lease-listing capability"))
	})

	It("logs the drain's phases on the critical path", func() {
		// The full drain against the fixture fakes: the start record
		// carries the identity, reason and budget; each phase lands as
		// the drain walks the hook's sequence.
		buf, restore := captureSlog()
		DeferCleanup(restore)

		f, fixtures := drainFixtures()
		DeferCleanup(fixtures)
		f.lister.claims = []chinitport.Claim{{Hostname: "writer-0"}}

		st := drainer()
		provider := &drainProvider{name: "aws", lister: f.lister, claimer: f.claimer, dns: f.dns}
		Expect(drain(context.Background(), st, provider, drainOptions{
			DeadlineSeconds: 240,
			Reason:          drainReasonTerminationHook,
		})).To(Succeed())

		log := buf.String()
		Expect(log).To(ContainSubstring("drain: draining node"))
		Expect(log).To(ContainSubstring("hostname=writer-1"))
		Expect(log).To(ContainSubstring("reason=termination-hook"))
		Expect(log).To(ContainSubstring("budget_seconds=240"))
		Expect(log).To(ContainSubstring("drain: query issued"))
		Expect(log).To(ContainSubstring("drain: dropping replica on peer"))
		Expect(log).To(ContainSubstring("drain: DNS A record released"))
		Expect(log).To(ContainSubstring("drain: lease released"))
		Expect(log).To(ContainSubstring("drain: complete"))
	})
})

var _ = Describe("lease-renew logging", func() {
	It("logs the renewal attempt and its success", func() {
		buf, restore := captureSlog()
		DeferCleanup(restore)

		claimer := &drainClaimer{}
		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &drainProvider{name: "aws", claimer: claimer}, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })

		dir := filepath.Join(os.TempDir(), "chinit-app-blindspot-test")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())
		statePath := filepath.Join(dir, "state.json")
		state := `{"identity":{"role":"writer","hostname":"writer-1","instance_id":"i-w1","cluster_name":"demo","dns_zone":"ch.internal","lease_table":"node-claims"}}`
		Expect(os.WriteFile(statePath, []byte(state), 0644)).To(Succeed())

		Expect(runLeaseRenew([]string{"-state", statePath})).To(Succeed())
		log := buf.String()
		Expect(log).To(ContainSubstring("lease-renew: extending lease"))
		Expect(log).To(ContainSubstring("hostname=writer-1"))
		Expect(log).To(ContainSubstring("lease_table=node-claims"))
		Expect(log).To(ContainSubstring("lease-renew: lease extended"))
	})

	It("logs the renewal failure with the cause as an attribute", func() {
		buf, restore := captureSlog()
		DeferCleanup(restore)

		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &drainProvider{name: "aws", claimer: &failingRenewClaimer{}}, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })

		dir := filepath.Join(os.TempDir(), "chinit-app-blindspot-test")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())
		statePath := filepath.Join(dir, "state.json")
		state := `{"identity":{"role":"writer","hostname":"writer-1","instance_id":"i-w1","lease_table":"node-claims"}}`
		Expect(os.WriteFile(statePath, []byte(state), 0644)).To(Succeed())

		Expect(runLeaseRenew([]string{"-state", statePath})).NotTo(Succeed())
		Expect(buf.String()).To(ContainSubstring("lease-renew: renewal failed"))
		Expect(buf.String()).To(ContainSubstring("err="))
	})
})

// failingRenewClaimer answers a renewal error.
type failingRenewClaimer struct{}

func (c *failingRenewClaimer) ClaimOrdinal(context.Context, chinitport.ClaimRequest) (int, error) {
	return 0, nil
}

func (c *failingRenewClaimer) RenewLease(context.Context, chinitport.RenewRequest) error {
	return errors.New("dynamodb throttled")
}

func (c *failingRenewClaimer) ReleaseLease(context.Context, chinitport.ReleaseRequest) error {
	return nil
}
