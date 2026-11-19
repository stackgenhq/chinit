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

var _ = Describe("ddlQueueReplicas & cliDDLTaskQueueCleaner", func() {
	It("selects chi-*, localhost, and dead writer/reader entries (both :port, %3Aport, and %2D/%2E escaped) while sparing live claims and backup-0", func() {
		entries := ddlQueueReplicas{
			"writer-0.example.internal:9010",
			"writer%2D0%2Eexample%2Einternal:9440",
			"writer-1.example.internal%3A9010",
			"writer%2D4%2Eexample%2Einternal:9440",
			"reader-0:9440",
			"reader%2D1%2Eexample%2Einternal:9010",
			"localhost:9000",
			"chi-demo-demo-01-2:9000",
			"backup%2D0%2Eexample%2Einternal:9010",
		}
		live := map[string]bool{
			"writer-0": true,
			"reader-1": true,
		}
		stale := entries.staleEntries(live, "writer-", "reader-")
		Expect([]string(stale)).To(Equal([]string{
			"writer-1.example.internal%3A9010",
			"writer%2D4%2Eexample%2Einternal:9440",
			"reader-0:9440",
			"localhost:9000",
			"chi-demo-demo-01-2:9000",
		}))
	})

	It("prunes stale DDL task_queue replica znodes via clickhouse-keeper-client", func() {
		dir := GinkgoT().TempDir()
		specPath := filepath.Join(dir, "chinit.yaml")
		Expect(os.WriteFile(specPath, []byte("keeper_nodes: [\"keeper-0\"]\ndns_zone_name: \"example.internal\"\n"), 0644)).To(Succeed())
		origSpec := inventorySpecPath
		inventorySpecPath = func() string { return specPath }
		DeferCleanup(func() { inventorySpecPath = origSpec })

		var cmds []string
		origRun := runCommand
		runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
			line := name + " " + strings.Join(args, " ")
			cmds = append(cmds, line)
			if strings.Contains(line, "ls '/clickhouse/demo/task_queue/replicas'") {
				return []byte("writer-0.example.internal:9010 writer-1:9010 chi-demo-demo-01-4:9000 backup-0.example.internal:9010\n"), nil
			}
			if strings.Contains(line, "ls '/clickhouse/task_queue/replicas'") {
				return []byte("localhost%3A9000\n"), nil
			}
			return []byte("ok"), nil
		}
		DeferCleanup(func() { runCommand = origRun })

		st := &nodeState{Identity: model.NodeIdentity{
			Role:        model.RoleWriter,
			Hostname:    "writer-0",
			ClusterName: "demo",
			DNSZone:     "example.internal",
		}}
		cleaner := newDDLTaskQueueCleaner(context.Background(), st)
		Expect(cleaner).NotTo(BeNil())

		pruned := cleaner.PruneStaleDDLReplicas(activeClaims{{Hostname: "writer-0"}})
		Expect(pruned).To(Equal([]string{
			"/clickhouse/demo/task_queue/replicas/writer-1:9010",
			"/clickhouse/demo/task_queue/replicas/chi-demo-demo-01-4:9000",
			"/clickhouse/task_queue/replicas/localhost%3A9000",
		}))
	})
})

var _ = Describe("cliCasMountCleaner", func() {
	It("drops retired ephemeral i-* and chi-* fenced mounts while preserving live claims, deterministic UUIDs, and backup-0", func() {
		var queries []string
		origRunEnv := runCommandWithEnv
		runCommandWithEnv = func(_ context.Context, _ []string, _ string, args ...string) ([]byte, error) {
			q := strings.Join(args, " ")
			queries = append(queries, q)
			if strings.Contains(q, "FROM system.cas_mounts") {
				return []byte(strings.Join([]string{
					"cas\ti-0livewriter",
					"cas\ti-0deadreader",
					"cas_default\tchi-old-pod-0",
					"cas\tbackup-0",
					"cas\t550e8400-e29b-51d4-a716-446655440000",
				}, "\n")), nil
			}
			return []byte("ok"), nil
		}
		DeferCleanup(func() { runCommandWithEnv = origRunEnv })

		st := &nodeState{Identity: model.NodeIdentity{
			Role:       model.RoleWriter,
			Hostname:   "writer-0",
			InstanceID: "i-0self",
		}}
		cleaner := newCasMountCleaner(context.Background(), st)
		dropped := cleaner.PruneRetiredCasMounts(activeClaims{{Hostname: "writer-1", InstanceID: "i-0livewriter"}})
		Expect(dropped).To(Equal([]string{
			"cas:i-0deadreader",
			"cas_default:chi-old-pod-0",
		}))
		Expect(strings.Join(queries, "\n")).To(ContainSubstring("SYSTEM CAS DROP POOL MEMBER 'i-0deadreader' FROM DISK 'cas'"))
		Expect(strings.Join(queries, "\n")).To(ContainSubstring("SYSTEM CAS DROP POOL MEMBER 'chi-old-pod-0' FROM DISK 'cas_default'"))
	})
})
