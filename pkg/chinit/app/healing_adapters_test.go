// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// This file pins the force-restore flagger's KEEPER PATH contract —
// the 2026-11-17 live A/B on backup-0: the flag lives at
// /clickhouse/tables/01/<table>/replicas/<SELF>/flags/force_restore_data
// (the attach thread reads getFlagsPath() — the REPLICA's own flags
// znode). The table-level path (.../<table>/flags/...) fails "No
// node" and heals nothing; the replica-level path healed the
// readonly table on the next RESTART (is_readonly 1 -> 0). The
// adapter is transport-only, so the specs pin the argv it shells out
// plus the no-guess guard on the replica name.

var _ = Describe("cliForceRestoreFlagger", func() {
	var recorded [][]string

	seed := func(replicaHostname string) (*nodeState, string) {
		dir := filepath.Join(os.TempDir(), "chinit-force-restore-test")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())

		identity := model.NodeIdentity{
			Role:        model.RoleBackup,
			Hostname:    replicaHostname,
			InstanceID:  "i-0test",
			Region:      "us-west-2",
			ClusterName: "example",
			DNSZone:     "example.internal",
		}
		statePath := filepath.Join(dir, "state.json")
		data, err := json.Marshal(nodeState{Identity: identity})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(statePath, data, 0644)).To(Succeed())

		// The inventory spec the flagger reads for the keeper
		// endpoint (the same shape the bundle renders).
		specPath := filepath.Join(dir, "chinit.yaml")
		spec := "keeper_nodes: [\"keeper-0\", \"keeper-1\"]\ndns_zone_name: \"example.internal\"\n"
		Expect(os.WriteFile(specPath, []byte(spec), 0644)).To(Succeed())
		origSpec := inventorySpecPath
		inventorySpecPath = func() string { return specPath }
		DeferCleanup(func() { inventorySpecPath = origSpec })

		// The exec seam: record argv, answer success.
		origRun := runCommand
		recorded = nil
		runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
			recorded = append(recorded, append([]string{name}, args...))
			return []byte("ok"), nil
		}
		DeferCleanup(func() { runCommand = origRun })

		st, err := loadNodeState(statePath)
		Expect(err).NotTo(HaveOccurred())
		return st, statePath
	}

	It("touches the REPLICA-scoped flags path (the live A/B's healing form)", func() {
		st, _ := seed("backup-0")
		flagger := newForceRestoreFlagger(context.Background(), st)
		Expect(flagger).NotTo(BeNil(), "the spec carries keepers and the identity is settled")

		Expect(flagger.SetForceRestoreFlag("default.hourly_log_summary")).To(Succeed())
		Expect(recorded).To(HaveLen(1))
		argv := recorded[0]
		Expect(argv[0]).To(Equal("clickhouse-keeper-client"))
		// The path is REPLICA-scoped: .../<table>/replicas/<self>/flags/
		// force_restore_data — the attach thread's own read location.
		// The table-level form (.../<table>/flags/...) answered
		// "Coordination error: No node" live and healed nothing.
		Expect(strings.Join(argv, " ")).To(ContainSubstring(
			"touch /clickhouse/tables/01/hourly_log_summary/replicas/backup-0/flags/force_restore_data"))
		Expect(strings.Join(argv, " ")).NotTo(ContainSubstring(
			"/clickhouse/tables/01/hourly_log_summary/flags/force_restore_data"),
			"the table-level path is the proven no-op form")
	})

	It("targets the keeper-0 endpoint from the spec with the zone suffix", func() {
		st, _ := seed("backup-0")
		flagger := newForceRestoreFlagger(context.Background(), st)
		Expect(flagger).NotTo(BeNil())

		Expect(flagger.SetForceRestoreFlag("default.events")).To(Succeed())
		Expect(strings.Join(recorded[0], " ")).To(ContainSubstring("--host keeper-0.example.internal"))
		Expect(strings.Join(recorded[0], " ")).To(ContainSubstring("--port 9181"))
	})

	It("refuses to build the flagger without a settled identity (never guess a path)", func() {
		// An empty replica name must never produce a flagger that
		// writes blind paths. The loadNodeState gate is the outer
		// guard (an empty-hostname state file answers Fatal — no
		// subcommand ever sees one); the adapter keeps its own guard
		// so a future caller that constructs it directly cannot
		// regress into guessing. Pin the adapter-level guard
		// directly: a zero-value flagger answers an error, not a
		// blind touch.
		flagger := &cliForceRestoreFlagger{execContext: context.Background(), keeperHost: "keeper-0.example.internal", keeperPort: 9181}
		err := flagger.SetForceRestoreFlag("default.hourly_log_summary")
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("replica name is empty"))
	})

	It("answers nil on a keeper-less stack (no znodes to flag)", func() {
		st, _ := seed("backup-0")
		// A spec with no keeper_nodes: the queue-heal pass reports the
		// init-failed tables unhealed rather than touching anything.
		specPath := filepath.Join(os.TempDir(), "chinit-force-restore-test", "chinit.yaml")
		Expect(os.WriteFile(specPath, []byte("dns_zone_name: \"example.internal\"\n"), 0644)).To(Succeed())
		Expect(newForceRestoreFlagger(context.Background(), st)).To(BeNil())
	})
})
