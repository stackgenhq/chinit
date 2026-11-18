// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// This file pins `chinit spec-sync` — the config-refresher Lambda's
// replacement. The job PULLS the node's role spec from the bundle
// prefix and converges on drift via the in-process boot --refresh;
// the per-role lock serializes the restart-bearing converge.

// fakeRoleSpecSource serves the canned role spec.
type fakeRoleSpecSource struct {
	spec     []byte
	exists   bool
	err      error
	requests []chinitport.RoleSpecRequest
}

func (f *fakeRoleSpecSource) FetchRoleSpec(ctx context.Context, req chinitport.RoleSpecRequest) ([]byte, bool, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, false, f.err
	}
	return f.spec, f.exists, nil
}

// fakeRefreshLock records lock traffic.
type fakeRefreshLock struct {
	acquired []chinitport.RefreshLockRequest
	released []chinitport.RefreshLockRequest
	take     bool
}

func (f *fakeRefreshLock) AcquireRefreshLock(ctx context.Context, req chinitport.RefreshLockRequest) (bool, error) {
	f.acquired = append(f.acquired, req)
	return f.take, nil
}

func (f *fakeRefreshLock) ReleaseRefreshLock(ctx context.Context, req chinitport.RefreshLockRequest) error {
	f.released = append(f.released, req)
	return nil
}

// specSyncProvider composes the capability fakes (the disk-autoscale
// suite's provider shape: the mandatory surface stubbed, the
// capability under test real).
type specSyncProvider struct {
	name  string
	specs chinitport.RoleSpecSource
	lock  chinitport.RefreshLock
}

func (p *specSyncProvider) Name() string { return p.name }
func (p *specSyncProvider) Metadata(context.Context) (chinitport.InstanceMetadata, error) {
	return chinitport.InstanceMetadata{}, nil
}
func (p *specSyncProvider) GetSecretValue(context.Context, chinitport.SecretRequest) (string, error) {
	return "", nil
}
func (p *specSyncProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}
func (p *specSyncProvider) FetchRoleSpec(ctx context.Context, req chinitport.RoleSpecRequest) ([]byte, bool, error) {
	return p.specs.FetchRoleSpec(ctx, req)
}
func (p *specSyncProvider) AcquireRefreshLock(ctx context.Context, req chinitport.RefreshLockRequest) (bool, error) {
	return p.lock.AcquireRefreshLock(ctx, req)
}
func (p *specSyncProvider) ReleaseRefreshLock(ctx context.Context, req chinitport.RefreshLockRequest) error {
	return p.lock.ReleaseRefreshLock(ctx, req)
}

// noSpecSourceProvider is the capability-absent provider: AsRoleSpecSource
// must answer nil for it.
type noSpecSourceProvider struct{ name string }

func (p *noSpecSourceProvider) Name() string { return p.name }
func (p *noSpecSourceProvider) Metadata(context.Context) (chinitport.InstanceMetadata, error) {
	return chinitport.InstanceMetadata{}, nil
}
func (p *noSpecSourceProvider) GetSecretValue(context.Context, chinitport.SecretRequest) (string, error) {
	return "", nil
}
func (p *noSpecSourceProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}

var _ = Describe("runSpecSync", func() {
	var dir string
	var statePath string
	var fakes *struct {
		specs *fakeRoleSpecSource
		lock  *fakeRefreshLock
	}
	var convergeCalled bool

	seed := func() {
		dir = filepath.Join(os.TempDir(), "chinit-app-spec-sync-test")
		Expect(os.MkdirAll(dir, 0755)).To(Succeed())
		identity := model.NodeIdentity{
			Role:        model.RoleWriter,
			Hostname:    "writer-0",
			InstanceID:  "i-0specsync0000000001",
			Region:      "us-west-2",
			ClusterName: "example",
			LeaseTable:  "example-claims",
		}
		data, err := json.Marshal(nodeState{Identity: identity})
		Expect(err).NotTo(HaveOccurred())
		statePath = filepath.Join(dir, "state.json")
		Expect(os.WriteFile(statePath, data, 0644)).To(Succeed())

		// The on-disk spec the job compares against: a valid chinit.yaml
		// carrying the bundle location (the job's fetch reads it).
		specYAML := "cluster_name: example\nconfig_bundle:\n  bucket: data-bucket\n  prefix: bootstrap/chinit\n"
		specPath := filepath.Join(dir, "chinit.yaml")
		Expect(os.WriteFile(specPath, []byte(specYAML), 0644)).To(Succeed())

		origSpecPath := defaultSpecSyncPath
		defaultSpecSyncPath = func() string { return specPath }
		origInventoryPath := inventorySpecPath
		inventorySpecPath = func() string { return specPath }
		DeferCleanup(func() {
			defaultSpecSyncPath = origSpecPath
			inventorySpecPath = origInventoryPath
		})

		fakes = &struct {
			specs *fakeRoleSpecSource
			lock  *fakeRefreshLock
		}{specs: &fakeRoleSpecSource{}, lock: &fakeRefreshLock{take: true}}
		convergeCalled = false

		origBuild := buildProvider
		origConverge := specSyncConvergeFn
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &specSyncProvider{name: "aws", specs: fakes.specs, lock: fakes.lock}, nil
		}
		specSyncConvergeFn = func(ctx context.Context, st *nodeState, path string, spec []byte) error {
			convergeCalled = true
			Expect(os.WriteFile(path, spec, 0644)).To(Succeed())
			return nil
		}
		DeferCleanup(func() {
			buildProvider = origBuild
			specSyncConvergeFn = origConverge
		})
	}

	It("is a no-op when the spec is unchanged", func() {
		seed()
		current, err := os.ReadFile(defaultSpecSyncPath())
		Expect(err).NotTo(HaveOccurred())
		fakes.specs.spec = current
		fakes.specs.exists = true

		Expect(runSpecSync([]string{"-state", statePath})).To(Succeed())
		Expect(convergeCalled).To(BeFalse(), "an unchanged spec must never restart the service")
		// The fetch addressed the bundle the node's own spec names —
		// never an apply-time env snapshot (the Lambda's stale-env
		// failure shape).
		Expect(fakes.specs.requests).To(HaveLen(1))
		Expect(fakes.specs.requests[0].Bucket).To(Equal("data-bucket"))
		Expect(fakes.specs.requests[0].Prefix).To(Equal("bootstrap/chinit"))
		Expect(fakes.specs.requests[0].Role).To(Equal("writer"))
		// No lock traffic: the lock belongs to the converge, not the
		// (skipped) compare.
		Expect(fakes.lock.acquired).To(BeEmpty())
	})

	It("converges on drift under the per-role lock", func() {
		seed()
		fakes.specs.spec = []byte("cluster_name: example\n# drifted\n")
		fakes.specs.exists = true

		Expect(runSpecSync([]string{"-state", statePath})).To(Succeed())
		Expect(convergeCalled).To(BeTrue(), "a drifted spec must converge")
		// The lock is per-ROLE, keyed on the claims table the state
		// carries, and released after the converge — one writer
		// restarts at a time.
		Expect(fakes.lock.acquired).To(HaveLen(1))
		Expect(fakes.lock.acquired[0].Table).To(Equal("example-claims"))
		Expect(fakes.lock.acquired[0].Key).To(Equal("refresh-writer"))
		Expect(fakes.lock.acquired[0].Holder).To(Equal("writer-0"))
		Expect(fakes.lock.released).To(HaveLen(1))
	})

	It("skips the converge when a peer holds the lock", func() {
		seed()
		fakes.specs.spec = []byte("cluster_name: example\n# drifted\n")
		fakes.specs.exists = true
		fakes.lock.take = false

		Expect(runSpecSync([]string{"-state", statePath})).To(Succeed())
		Expect(convergeCalled).To(BeFalse(), "the lock holder owns the converge; the peer retries next tick")
		// A failed acquire must NOT release — the release would yank
		// the lock from the peer that holds it.
		Expect(fakes.lock.released).To(BeEmpty())
	})

	It("skips benignly when the bundle carries no spec for the role", func() {
		seed()
		fakes.specs.exists = false

		Expect(runSpecSync([]string{"-state", statePath})).To(Succeed())
		Expect(convergeCalled).To(BeFalse())
		Expect(fakes.lock.acquired).To(BeEmpty())
	})

	It("skips loudly when the provider lacks the capability", func() {
		seed()
		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &noSpecSourceProvider{name: "azure"}, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })

		Expect(runSpecSync([]string{"-state", statePath})).To(Succeed(),
			"a capability gap is a logged no-op, never a timer-failing error")
		Expect(convergeCalled).To(BeFalse())
	})

	It("fails the tick on a fetch error (the timer retries)", func() {
		seed()
		fakes.specs.err = errFakeFetch

		err := runSpecSync([]string{"-state", statePath})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec-sync:"))
		Expect(convergeCalled).To(BeFalse())
	})
})

var errFakeFetch = &fakeFetchError{}

type fakeFetchError struct{}

func (*fakeFetchError) Error() string { return "transport exploded" }
