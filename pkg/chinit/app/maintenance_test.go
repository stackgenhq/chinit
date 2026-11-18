// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
)

// This suite pins the writer-ASG maintenance contract (A5):
//
// - the ordinal-0 gate: the maintenance timers install on EVERY
// writer, but with ASG writers "writer-0" is a lease ordinal, not
// a machine — the subcommand must no-op exit 0 on any node that
// does not hold the settled identity writer-{prefix}0 (readers
// and non-0 writers take the same skip path, so a manual SSM run
// cannot fire the sweep from the wrong node);
// - the two-pass dead-replica GC with its safety contract: the
// reader pass ALWAYS runs; the writer pass runs only with
// liveness data (active lease claims) and passes every live
// writer as -exclude so a mid-boot re-join never loses its
// registration.

// maintSpec is the bundle-spec fixture the gate and the writer pass
// read (the writer prefix and, for lease-less nodes, the claimer
// table). Carries a custom-ish default shape plus the claimer table.
const maintSpec = `role: writer
identity_mode: lease
cluster_name: demo
writer_max: 4
writer_hostname_prefix: writer-
reader_max: 10
reader_hostname_prefix: reader-
dns_zone_name: ch.internal
claimer_table_name: node-claims
`

// maintDir is this suite's scratch dir — never /var/lib.
var maintDir = filepath.Join(os.TempDir(), "chinit-app-maintenance-test")

// maintFixtures installs the spec + password seams and returns the
// restore func (DeferCleanup).
func maintFixtures() func() {
	if err := os.MkdirAll(maintDir, 0755); err != nil {
		Fail(err.Error())
	}
	specPath := filepath.Join(maintDir, "chinit.yaml")
	if err := os.WriteFile(specPath, []byte(maintSpec), 0644); err != nil {
		Fail(err.Error())
	}
	passwordPath := filepath.Join(maintDir, "admin-password")
	if err := os.WriteFile(passwordPath, []byte("sekrit\n"), 0644); err != nil {
		Fail(err.Error())
	}
	origSpec, origPassword := inventorySpecPath, adminPasswordPath
	inventorySpecPath = func() string { return specPath }
	adminPasswordPath = func() string { return passwordPath }
	return func() {
		inventorySpecPath = origSpec
		adminPasswordPath = origPassword
	}
}

// maintState builds a settled-identity nodeState.
func maintState(role model.Role, hostname string) *nodeState {
	return &nodeState{Identity: model.NodeIdentity{
		Role:        role,
		Hostname:    hostname,
		ClusterName: "demo",
		DNSZone:     "ch.internal",
		CRName:      "01-0",
		LeaseTable:  "node-claims",
	}}
}

// captureStderr redirects os.Stderr into a file for the duration of
// run and returns what was written — the WARNING lines the specs
// assert are the operator's journal contract (fmt.Fprintf resolves
// os.Stderr at call time, so the swap is honored).
//
// Self-sufficient on the scratch dir: Ginkgo shuffles top-level
// container order per run, so a Describe outside the maintenance
// suite (the logging specs) may run BEFORE the maintenance
// BeforeEach has created maintDir — OpenFile would fail on a fresh
// runner (the chinit-v0.1.8 release-gate flake). MkdirAll is
// idempotent and keeps the helper order-independent.
func captureStderr(run func()) string {
	if err := os.MkdirAll(maintDir, 0755); err != nil {
		Fail(err.Error())
	}
	logPath := filepath.Join(maintDir, "stderr.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		Fail(err.Error())
	}
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()
	run()
	if err := f.Close(); err != nil {
		Fail(err.Error())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		Fail(err.Error())
	}
	return string(data)
}

var _ = Describe("gateMaintenanceJob", func() {
	BeforeEach(func() {
		DeferCleanup(maintFixtures())
	})

	DescribeTable("the ordinal-0 designation",
		func(role model.Role, hostname string, wantSkip bool) {
			Expect(gateMaintenanceJob(context.Background(), maintState(role, hostname))).To(Equal(wantSkip))
		},
		Entry("the writer holding ordinal 0 runs the jobs", model.RoleWriter, "writer-0", false),
		Entry("a non-0 writer skips (the ordinal is a lease, not a machine)", model.RoleWriter, "writer-1", true),
		Entry("a high-ordinal writer skips", model.RoleWriter, "writer-3", true),
		Entry("a reader skips even at ordinal 0 (SSM manual run)", model.RoleReader, "reader-0", true),
		Entry("a keeper-shaped node skips", model.RoleKeeper, "keeper-0", true),
	)

	It("honors the spec's custom writer prefix", func() {
		// The designation is prefix+0 of the prefix the BOOT used
		// "writer-0" must not match when the stack leased "ingest-N".
		specPath := filepath.Join(maintDir, "chinit-prefix.yaml")
		if err := os.WriteFile(specPath, []byte(`role: writer
identity_mode: lease
cluster_name: demo
writer_max: 2
writer_hostname_prefix: ingest-
dns_zone_name: ch.internal
`), 0644); err != nil {
			Fail(err.Error())
		}
		orig := inventorySpecPath
		inventorySpecPath = func() string { return specPath }
		defer func() { inventorySpecPath = orig }()

		Expect(gateMaintenanceJob(context.Background(), maintState(model.RoleWriter, "ingest-0"))).To(BeFalse(),
			"the ingest-0 holder runs the jobs")
		Expect(gateMaintenanceJob(context.Background(), maintState(model.RoleWriter, "writer-0"))).To(BeTrue(),
			"the default-prefixed name does not match a custom-prefix stack")
	})

	It("defaults to the model prefix when the bundle spec is unreadable", func() {
		orig := inventorySpecPath
		inventorySpecPath = func() string { return "/nonexistent/chinit.yaml" }
		defer func() { inventorySpecPath = orig }()

		Expect(gateMaintenanceJob(context.Background(), maintState(model.RoleWriter, "writer-0"))).To(BeFalse(),
			"the default writer- prefix holds the designation")
	})

	DescribeTable("the vacancy election (no live ordinal-0 lease)",
		// The 2026-11-16 finding: writer-0's instance terminated
		// mid-churn and no node re-claimed ordinal 0 — every writer
		// skipped and the fleet's GC silently stopped. The gate now
		// elects the lowest-ordinal LIVE writer when the ordinal-0
		// lease is vacant.
		func(hostname string, claims []chinitport.Claim, wantSkip bool) {
			Expect(gateMaintenanceJobWithClaims(context.Background(), maintState(model.RoleWriter, hostname), claims)).To(Equal(wantSkip))
		},
		Entry("the lowest-ordinal live writer runs when ordinal 0 is vacant",
			"writer-1",
			[]chinitport.Claim{{Hostname: "writer-2"}, {Hostname: "writer-1"}},
			false),
		Entry("a higher-ordinal live writer still skips (only the lowest runs)",
			"writer-2",
			[]chinitport.Claim{{Hostname: "writer-2"}, {Hostname: "writer-1"}},
			true),
		Entry("no election when ordinal 0 is live (the holder runs; others skip)",
			"writer-1",
			[]chinitport.Claim{{Hostname: "writer-0"}, {Hostname: "writer-1"}},
			true),
		Entry("no election without claims data (fail-safe: the historic skip)",
			"writer-1",
			nil,
			true),
		Entry("no election for a node whose own lease is absent from the claims",
			"writer-1",
			[]chinitport.Claim{{Hostname: "writer-2"}},
			true),
		Entry("reader claims never elect (prefix match only)",
			"writer-1",
			[]chinitport.Claim{{Hostname: "reader-0"}, {Hostname: "reader-1"}},
			true),
	)

	It("elects under a custom writer prefix", func() {
		// The election reads the prefix from the same spec seam as the
		// hard match: an ingest-N stack elects ingest-1, and the
		// default-prefixed name must not win.
		specPath := filepath.Join(maintDir, "chinit-prefix-election.yaml")
		if err := os.WriteFile(specPath, []byte(`role: writer
identity_mode: lease
cluster_name: demo
writer_max: 2
writer_hostname_prefix: ingest-
dns_zone_name: ch.internal
`), 0644); err != nil {
			Fail(err.Error())
		}
		orig := inventorySpecPath
		inventorySpecPath = func() string { return specPath }
		defer func() { inventorySpecPath = orig }()

		claims := []chinitport.Claim{{Hostname: "ingest-1"}, {Hostname: "ingest-2"}}
		Expect(gateMaintenanceJobWithClaims(context.Background(), maintState(model.RoleWriter, "ingest-1"), claims)).To(BeFalse(),
			"the lowest-ordinal live ingest writer runs")
		Expect(gateMaintenanceJobWithClaims(context.Background(), maintState(model.RoleWriter, "writer-1"), claims)).To(BeTrue(),
			"the default-prefixed name does not match a custom-prefix stack")
	})
})

var _ = Describe("deadReplicaGc", func() {
	var ops *fakeClusterOps
	var lister *fakeJobLister

	BeforeEach(func() {
		DeferCleanup(maintFixtures())
		origOps := clusterOps
		ops = &fakeClusterOps{}
		clusterOps = ops
		DeferCleanup(func() { clusterOps = origOps })
		lister = &fakeJobLister{}
	})

	Context("the ordinal-0 holder with liveness data", func() {
		It("runs the reader pass then the guarded writer pass", func() {
			lister.claims = []chinitport.Claim{
				{Hostname: "writer-0"},
				{Hostname: "writer-1", InstanceID: "i-w1"}, // mid-boot re-join, live lease
				{Hostname: "reader-2"},
			}
			st := maintState(model.RoleWriter, "writer-0")

			Expect(deadReplicaGc(context.Background(), st, lister)).To(Succeed())

			// Reader pass first (self as the detection peer, no
			// pattern = the reader-* default); writer pass second
			// with the writer pattern and one exclude per LIVE
			// writer claim (sorted).
			calls := ops.gcCalls()
			Expect(calls).To(HaveLen(2))
			Expect(calls[0].RunOn).To(Equal("writer-0"))
			Expect(calls[0].NamePattern).To(Equal(""))
			Expect(calls[0].ExcludeHosts).To(BeEmpty())
			Expect(calls[1].RunOn).To(Equal("writer-0"))
			Expect(calls[1].NamePattern).To(Equal("writer-*"))
			Expect(calls[1].ExcludeHosts).To(Equal([]string{"writer-0", "writer-1"}))
			// The lease table listed once, from the identity.
			Expect(lister.tables).To(Equal([]string{"node-claims"}))
		})

		// NOTE: the reader-exclusion contract (a live reader claim
		// never pads the writer pass's excludes) is pinned by the
		// exact-argv spec above — its Equal asserts the full writer
		// pass argv with reader-2's claim live but ABSENT from the
		// excludes, so a reader-padding regression fails it without a
		// dedicated spec re-asserting the same value.
	})

	Context("no liveness data", func() {
		It("skips the writer pass with a warning; the reader pass still runs", func() {
			// The GC safety contract's fail-safe: without liveness data
			// the writer pass is too dangerous to guess at.
			st := maintState(model.RoleWriter, "writer-0")

			Expect(deadReplicaGc(context.Background(), st, nil)).To(Succeed())
			calls := ops.gcCalls()
			Expect(calls).To(HaveLen(1), "exactly one gc call — the reader pass")
			Expect(calls[0].NamePattern).To(BeEmpty())
		})

		It("skips the writer pass when the claims scan fails", func() {
			lister.err = fmt.Errorf("lease table down")
			st := maintState(model.RoleWriter, "writer-0")

			Expect(deadReplicaGc(context.Background(), st, lister)).To(Succeed())
			calls := ops.gcCalls()
			Expect(calls).To(HaveLen(1))
			Expect(calls[0].NamePattern).To(BeEmpty())
		})

		It("skips the writer pass when no lease table is known", func() {
			// A lease-less identity (static writer) with a spec that
			// names no claimer table: no table to list, no writer pass.
			specPath := filepath.Join(maintDir, "chinit-notable.yaml")
			if err := os.WriteFile(specPath, []byte(`role: writer
identity_mode: static
cluster_name: demo
dns_zone_name: ch.internal
`), 0644); err != nil {
				Fail(err.Error())
			}
			orig := inventorySpecPath
			inventorySpecPath = func() string { return specPath }
			defer func() { inventorySpecPath = orig }()

			st := maintState(model.RoleWriter, "writer-0")
			st.Identity.LeaseTable = "" // static node holds no lease

			Expect(deadReplicaGc(context.Background(), st, lister)).To(Succeed())
			Expect(lister.tables).To(BeEmpty(), "no table known — the lister is never consulted")
			calls := ops.gcCalls()
			Expect(calls).To(HaveLen(1))
			Expect(calls[0].NamePattern).To(BeEmpty())
		})
	})

	Context("pass failures", func() {
		It("fails the run when the reader pass fails", func() {
			ops.gcErrs = []error{fmt.Errorf("connection refused")}

			st := maintState(model.RoleWriter, "writer-0")
			err := deadReplicaGc(context.Background(), st, lister)
			Expect(err).To(MatchError(ContainSubstring("reader GC pass failed")))
		})

		It("fails the run when the writer pass fails but the reader pass ran", func() {
			// First call (reader pass) succeeds; second (writer pass) fails.
			ops.gcErrs = []error{nil, fmt.Errorf("peer unreachable")}

			lister.claims = []chinitport.Claim{{Hostname: "writer-0"}}
			st := maintState(model.RoleWriter, "writer-0")
			err := deadReplicaGc(context.Background(), st, lister)
			Expect(err).To(MatchError(ContainSubstring("writer GC pass failed")))
		})
	})
})

var _ = Describe("runDeadReplicaGc / runOrphanedGc (the CLI entries)", func() {
	var ops *fakeClusterOps
	var lister *fakeJobLister
	var statePath string

	writeMaintState := func(role model.Role, hostname string) {
		state := fmt.Sprintf(`{"identity":{"role":%q,"hostname":%q,"cluster_name":"demo","dns_zone":"ch.internal","cr_name":"01-0","lease_table":"node-claims"}}`, role, hostname)
		if err := os.WriteFile(statePath, []byte(state), 0644); err != nil {
			Fail(err.Error())
		}
	}

	BeforeEach(func() {
		DeferCleanup(maintFixtures())
		ops = &fakeClusterOps{}
		lister = &fakeJobLister{}
		// The cluster-op seam: the entry calls the in-process port,
		// not a binary — the fake records the typed request.
		origOps := clusterOps
		clusterOps = ops
		// The provider seam (drain suite's pattern): the entry builds
		// its lister through buildProvider, and the fake keeps the
		// registry's real AWS provider (IMDS credential retries) out
		// of the test clock.
		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &drainProvider{name: "aws", lister: lister}, nil
		}
		DeferCleanup(func() {
			clusterOps = origOps
			buildProvider = origBuild
		})

		statePath = filepath.Join(maintDir, "state.json")
	})

	It("no-ops exit 0 on a node that is not the ordinal-0 holder", func() {
		// The entries' only gate is gateMaintenanceJob — the same
		// function the gate DescribeTable above pins across the role
		// matrix (writer-1/3, reader-0, keeper-0), so one entry-level
		// witness per CLI entry suffices here; a per-role entry spec
		// would re-assert the gate the table already proves.
		writeMaintState(model.RoleWriter, "writer-1")
		Expect(runDeadReplicaGc([]string{"-state", statePath})).To(Succeed())
		Expect(runOrphanedGc([]string{"-state", statePath})).To(Succeed())
		Expect(ops.calls).To(BeEmpty(), "neither maintenance job reaches the cluster port")
	})

	It("runs both passes on the ordinal-0 holder with live claims", func() {
		// The entry contract: state file → ordinal-0 gate →
		// gcLivelinessFrom through the buildProvider seam → the
		// two-pass sweep. The exact argv is pinned by the
		// deadReplicaGc specs above; here the assertion is the entry
		// WIRING — two chctl passes, the second one the guarded
		// writer sweep with the live-writer excludes.
		lister.claims = []chinitport.Claim{
			{Hostname: "writer-0"},
			{Hostname: "writer-1"},
		}
		writeMaintState(model.RoleWriter, "writer-0")
		Expect(runDeadReplicaGc([]string{"-state", statePath})).To(Succeed())
		calls := ops.gcCalls()
		Expect(calls).To(HaveLen(2), "reader pass then writer pass")
		Expect(calls[1].NamePattern).To(Equal("writer-*"))
		Expect(calls[1].ExcludeHosts).To(Equal([]string{"writer-0", "writer-1"}))
		// The claims came from the entry's own provider build, not a
		// direct lister handoff.
		Expect(lister.tables).To(Equal([]string{"node-claims"}))
	})

	It("runs the reader pass only when the entry's provider has no lister (WARNING in the log)", func() {
		// The ordinal-0 holder whose cloud cannot list leases (the
		// capability-absent shape): gcLivelinessFrom answers nil, the
		// writer pass is skipped WITH the WARNING the operator reads
		// in the journal, and the reader pass still ran — the GC
		// safety contract's fail-safe at the ENTRY level (the core
		// deadReplicaGc specs pin the same pass with a nil lister
		// handed in directly).
		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &bareProvider{providerName: "azure"}, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })

		writeMaintState(model.RoleWriter, "writer-0")
		log := captureStderr(func() {
			Expect(runDeadReplicaGc([]string{"-state", statePath})).To(Succeed())
		})
		calls := ops.gcCalls()
		Expect(calls).To(HaveLen(1), "exactly one gc call — the reader pass")
		Expect(calls[0].NamePattern).To(BeEmpty())
		Expect(log).To(ContainSubstring("WARNING"))
		Expect(log).To(ContainSubstring("skipping the writer GC pass"))
		Expect(log).To(ContainSubstring("no lease-listing capability"))
	})

	It("runs the orphaned census on the ordinal-0 holder", func() {
		writeMaintState(model.RoleWriter, "writer-0")
		Expect(runOrphanedGc([]string{"-state", statePath})).To(Succeed())
		Expect(ops.calls).To(BeEmpty(), "orphaned-gc is a documented no-op; it reaches no cluster port")
	})

	It("sweeps retired high-water DNS records across writer_max and reader_max on the coordinator without extra lease scans", func() {
		// maintSpec sets writer_max: 4, reader_max: 10. With only writer-0
		// and reader-1 active, the coordinator must sweep writer-1..3 and
		// reader-0, reader-2..9 while keeping lister.tables at a single scan.
		lister.claims = []chinitport.Claim{
			{Hostname: "writer-0", IP: "10.0.1.254"},
			{Hostname: "reader-1", IP: "10.0.1.233"},
		}
		dns := &drainDNS{}
		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &drainProvider{name: "aws", lister: lister, dns: dns}, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })

		writeMaintState(model.RoleWriter, "writer-0")
		Expect(runDeadReplicaGc([]string{"-state", statePath})).To(Succeed())
		Expect(lister.tables).To(Equal([]string{"node-claims"}), "must reuse the single entry lease scan")

		deleted := map[string]bool{}
		for _, req := range dns.deletes {
			deleted[req.RecordName] = true
		}
		Expect(deleted["writer-1"]).To(BeTrue(), "retired high-water writer-1 record must be deleted")
		Expect(deleted["reader-0"]).To(BeTrue(), "vacant reader-0 record must be deleted")
		Expect(deleted["writer-0"]).To(BeFalse(), "live writer-0 record must never be deleted")
		Expect(deleted["reader-1"]).To(BeFalse(), "live reader-1 record must never be deleted")
	})
})

var _ = Describe("runQueueHeal (the CLI entry)", func() {
	var ops *fakeClusterOps
	var statePath string

	writeQueueHealState := func() {
		state := `{"identity":{"role":"writer","hostname":"writer-1","cluster_name":"demo","dns_zone":"ch.internal","cr_name":"01-0","lease_table":"node-claims"}}`
		if err := os.WriteFile(statePath, []byte(state), 0644); err != nil {
			Fail(err.Error())
		}
	}

	BeforeEach(func() {
		DeferCleanup(maintFixtures())
		ops = &fakeClusterOps{}
		origOps := clusterOps
		clusterOps = ops
		DeferCleanup(func() { clusterOps = origOps })
		statePath = filepath.Join(maintDir, "queue-heal-state.json")
	})

	It("heals its OWN queue — no ordinal-0 gate on a writer-1 node", func() {
		// Self-healing, not a cluster sweep: every writer's timer heals
		// its own queue, so the ordinal gate must NOT apply (the
		// incident's stuck queue sat on writer-1 while writer-0 held
		// the designation).
		writeQueueHealState()
		ops.queueHeal = &clusterops.QueueHealResult{
			StuckTables: []string{"default.events"},
			Restarted:   []string{"default.events"},
		}
		Expect(runQueueHeal([]string{"-state", statePath})).To(Succeed())
		Expect(ops.calls).To(HaveLen(1), "the queue-heal port is reached from a non-0 writer")
		Expect(ops.calls[0].Op).To(Equal("QueueHeal"))
	})

	It("threads the flags: -stuck-tries and the busy-skip default", func() {
		writeQueueHealState()
		Expect(runQueueHeal([]string{"-state", statePath, "-stuck-tries", "500"})).To(Succeed())
		req := ops.calls[0].Req.(clusterops.QueueHealRequest)
		Expect(req.StuckTries).To(Equal(500))
		Expect(req.BusyMergesSkip).To(BeTrue(), "the busy-skip gate defaults ON")
		Expect(req.RunOn).To(Equal("writer-1"), "self-heal: the node's own queue")
	})

	It("-no-busy-skip disables the busy gate", func() {
		writeQueueHealState()
		Expect(runQueueHeal([]string{"-state", statePath, "-no-busy-skip"})).To(Succeed())
		req := ops.calls[0].Req.(clusterops.QueueHealRequest)
		Expect(req.BusyMergesSkip).To(BeFalse())
	})

	It("surfaces a port error on stderr", func() {
		writeQueueHealState()
		ops.queueHealErr = fmt.Errorf("keeper unreachable")
		Expect(runQueueHeal([]string{"-state", statePath})).NotTo(Succeed())
	})
})
