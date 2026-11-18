// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// runSchemaSync executes `chinit schema-sync` to propagate DDL objects
// from live peers onto this node.
func runSchemaSync(args []string) error {
	fs := flag.NewFlagSet("chinit schema-sync", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	retries := fs.Int("retries", 3, "full-pass retries (late materialized-view targets)")
	pruneStaleSelf := fs.Bool("prune-stale-self", false, "drop this node's stale replica registration before create")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadNodeState(*statePath)
	if err != nil {
		return err
	}
	ctx := context.Background()

	if err := clusterOps.SyncSchema(ctx, clusterops.SyncSchemaRequest{
		Cluster:        clusterRequest(st),
		HostName:       st.Identity.Hostname,
		Retries:        *retries,
		PruneStaleSelf: *pruneStaleSelf,
	}); err != nil {
		logger.GetLogger(ctx).Error("schema sync failed (retry on timer)", "err", err)
		return fmt.Errorf("schema sync failed (retry on timer): %w", err)
	}
	return nil
}

// runSchemaBootstrap is `chinit schema-bootstrap` (the 15-minute
// convergence unit): one idempotent pass of the bundle DDL — the same
// ApplyBootstrapDDL the boot's bootstrap_schema step runs. The
// reader-0 exhausted the boot's retries while the keeper rolled
// in the same instance refresh, and NOTHING re-ran the DDL afterward
// the node served reads schemaless for hours. This unit is the
// after-the-boot heal path; the timer retries it until the pass is clean.
func runSchemaBootstrap(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	result, err := service.ApplyBootstrapDDL(ctx, st.Identity.Hostname, string(st.Identity.Role))
	if err != nil {
		return fmt.Errorf("schema bootstrap failed (retry on timer): %w", err)
	}
	logger.GetLogger(ctx).Info("schema bootstrap: statements applied",
		"applied", result.Created,
		"ddl_path", result.DDLPath,
		"failed", result.Failed)
	if result.Failed > 0 {
		// Non-zero exit: the timer unit's OnFailure semantics and the
		// journal contract surface a failed pass; the next tick retries.
		return fmt.Errorf("schema bootstrap: %d statement(s) failed (retry on timer)", result.Failed)
	}
	return nil
}

// runReplicaSync is `chinit replica-sync` (the per-boot unit):
// SYSTEM SYNC REPLICA for every replicated table on this host.
func runReplicaSync(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := clusterOps.SyncReplica(ctx, clusterops.SyncReplicaRequest{
		Cluster:  clusterRequest(st),
		HostName: st.Identity.Hostname,
	}); err != nil {
		logger.GetLogger(ctx).Error("replica sync failed", "err", err)
		return err
	}
	return nil
}

// runQueueHeal is `chinit queue-heal` (the hourly timer on every
// writer; each node heals its OWN queue — no ordinal-0 gate, this is
// self-healing not a cluster sweep): detect replication-queue entries
// past the retry threshold with the merge pool idle, and restart the
// affected tables' replica queues.
//
// The 2026-11-16 incident: a NO_REPLICA_HAS_PART entry (a merge
// citing parts no live replica held — churn residue) retried 1.1M
// times with system.merges at ZERO. The queue blocked every merge
// behind it; parts piled to 365 toward the insert cliff; the parts
// alarm paged scale-outs that could not help (more replicas do not
// merge writer data). The manual remedy — SYSTEM RESTART REPLICA —
// is this timer, automated.
//
// Safety: the busy-skip gate (default on) never restarts a node with
// running merges — a BUSY merge pool is healthy; restarting it would
// kill in-flight merges for nothing. The stuck threshold defaults to
// 1000 tries; a progressing queue cycles individual exception retries
// orders of magnitude below it.
func runQueueHeal(args []string) error {
	fs := flag.NewFlagSet("chinit queue-heal", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	stuckTries := fs.Int("stuck-tries", 0, "num_tries threshold above which a replication-queue entry counts as stuck (0 = default 1000)")
	noBusySkip := fs.Bool("no-busy-skip", false, "restart stuck queues even when merges are running")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("queue-heal flags: %w", err)
	}
	st, err := loadNodeState(*statePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := clusterOps.QueueHeal(ctx, clusterops.QueueHealRequest{
		Cluster:        clusterRequest(st),
		RunOn:          st.Identity.Hostname,
		StuckTries:     *stuckTries,
		BusyMergesSkip: !*noBusySkip,
		// The init-FAILED readonly heal's keeper capability (nil on a
		// keeper-less stack: the pass reports the class unhealed).
		ForceRestore: newForceRestoreFlagger(ctx, st),
	})
	if err != nil {
		logger.GetLogger(ctx).Error("queue-heal failed", "err", err)
		return err
	}
	log := logger.GetLogger(ctx)
	switch {
	case res.SkippedReason != "":
		log.Info("queue-heal: nothing to do",
			"reason", res.SkippedReason,
			"stuck", len(res.StuckTables),
			"merges_running", res.MergesRunning)
	case len(res.Restarted) == 0:
		log.Info("queue-heal: no stuck entries")
	default:
		log.Warn("queue-heal: restarted stuck replica queues",
			"restarted", strings.Join(res.Restarted, ","),
			"stuck", len(res.StuckTables),
			"merges_running", res.MergesRunning)
	}
	if len(res.OrphanSynced) > 0 {
		log.Warn("queue-heal: synced orphan-merge queues (part-check re-enqueued)",
			"synced", strings.Join(res.OrphanSynced, ","),
			"orphans", len(res.OrphanTables))
	}
	if len(res.FailedTables) > 0 {
		// Per-table containment: the pass CONTINUED past these —
		// surfaced so the retry shows in the log (the 2026-11-17
		// finding: a timeout on one table used to abort the whole
		// pass silently-by-error).
		log.Warn("queue-heal: some tables failed this pass (contained; next pass retries)",
			"failed", strings.Join(res.FailedTables, ","))
	}
	if len(res.Restored) > 0 {
		log.Warn("queue-heal: restored read-only replica metadata",
			"restored", strings.Join(res.Restored, ","),
			"readonly", len(res.ReadonlyTables),
			"init_failed", len(res.InitFailedTables))
	}
	if len(res.InitFailedTables) > 0 && len(res.Restored) == 0 {
		log.Warn("queue-heal: init-failed read-only tables need the force-restore capability",
			"init_failed", strings.Join(res.InitFailedTables, ","))
	}
	return nil
}
