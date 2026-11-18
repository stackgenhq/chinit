// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// ErrNotSafeToDisrupt is returned when disrupting the target keeper would
// drop the ensemble below Raft quorum. Callers (lifecycle hooks) must hold
// the node.
var ErrNotSafeToDisrupt = errors.New("disrupting this keeper would drop the ensemble below Raft quorum")

// FaultTolerantMinMembers mirrors raftFaultTolerantMinMembers from the
// CHK controller's raft-safety gate: below this ensemble size quorum
// gating is meaningless — a 1- or 2-member ensemble cannot survive any
// planned disrupt, so the gate must not pretend otherwise and must allow
// maintenance.
const FaultTolerantMinMembers = 3

// DefaultQuorumPollInterval and DefaultQuorumWaitTimeout mirror the
// controller's defaultQuorumDisruptPollInterval / WaitTimeout.
const (
	DefaultQuorumPollInterval = 5 * time.Second
	DefaultQuorumWaitTimeout  = 2 * time.Minute
)

// QuorumSize is Raft majority for an ensemble of n members (n/2 + 1).
// Extracted from the controller's raftQuorumSize.
func QuorumSize(members int) int {
	if members <= 0 {
		return 0
	}
	return members/2 + 1
}

// HasQuorumHeadroom mirrors the controller's ensembleHasQuorumHeadroom.
func HasQuorumHeadroom(members int) bool {
	return members >= FaultTolerantMinMembers
}

// EnsembleSnapshot captures live Raft state before a disrupt decision.
// Frozen on purpose (the controller does the same): never re-derive after
// the disrupt starts.
type EnsembleSnapshot struct {
	// Members is the configured ensemble size.
	Members int
	// Synced is the count of live Raft participants per Keeper mntr.
	Synced int
}

// SafeAfterDisrupt reports whether the remaining synced members would still
// meet quorum if this host were disrupted. Re-extracted from the
// controller's ensembleQuorumSafeAfterDisrupt, with mntr synced-followers
// replacing STS ReadyReplicas as the live-count source.
func (s EnsembleSnapshot) SafeAfterDisrupt(targetSynced bool) bool {
	if !HasQuorumHeadroom(s.Members) {
		return true
	}
	remaining := s.Synced
	if targetSynced {
		remaining--
	}
	return remaining >= QuorumSize(s.Members)
}

// KeeperSafetyService decides whether a keeper node may be stopped without
// dropping the ensemble below Raft quorum, using a KeeperProbe for live
// state. It is the off-cluster counterpart of the controller's
// ensureQuorumSafeToDisruptHost.
type KeeperSafetyService struct {
	// probe reads live ensemble state.
	probe port.KeeperProbe
}

// NewKeeperSafetyService builds the service over a probe.
func NewKeeperSafetyService(probe port.KeeperProbe) *KeeperSafetyService {
	return &KeeperSafetyService{probe: probe}
}

// SafeToStopRequest is the input to SafeToStop.
type SafeToStopRequest struct {
	// KeeperAddress is the mntr endpoint of the leader or any voting
	// member.
	KeeperAddress string
	// Members is the configured ensemble size (defaults to the mntr
	// voting membership when zero).
	Members int
	// TargetIsSynced marks whether the node being disrupted is currently a
	// synced participant (removing an unsynced node cannot break quorum).
	TargetIsSynced bool
	// PollInterval bounds the wait loop (default 5s).
	PollInterval time.Duration
	// WaitTimeout bounds the total wait (default 2m).
	WaitTimeout time.Duration
}

// SafeToStop polls the ensemble until disrupting the target is safe, the
// wait budget is spent, or the context ends — mirroring the controller's
// ensureQuorumSafeToDisruptHost flow, refusals included: a disrupt that
// would break quorum is never allowed, only deferred.
func (s *KeeperSafetyService) SafeToStop(ctx context.Context, request SafeToStopRequest) error {
	if request.KeeperAddress == "" {
		return fmt.Errorf("keeper address is empty")
	}
	pollInterval := request.PollInterval
	if pollInterval == 0 {
		pollInterval = DefaultQuorumPollInterval
	}
	waitTimeout := request.WaitTimeout
	if waitTimeout == 0 {
		waitTimeout = DefaultQuorumWaitTimeout
	}

	deadline := time.Now().Add(waitTimeout)
	for {
		mntr, err := s.probe.Mntr(ctx, port.MntrRequest{Address: request.KeeperAddress})
		if err != nil {
			return fmt.Errorf("mntr: %w", err)
		}
		members := request.Members
		if members == 0 {
			members = mntr.VotingMembers
		}
		snapshot := EnsembleSnapshot{Members: members, Synced: mntr.SyncedCount()}
		if !HasQuorumHeadroom(members) {
			// Small ensemble: the controller skips gating entirely.
			return nil
		}
		if !request.TargetIsSynced || snapshot.SafeAfterDisrupt(request.TargetIsSynced) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (synced=%d quorum=%d members=%d)",
				ErrNotSafeToDisrupt, snapshot.Synced, QuorumSize(members), members)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
			// Re-check: a flapping follower may have re-synced.
		}
	}
}

// SchemaService orchestrates schema/replica operations via a SchemaRunner
// factory.
type SchemaService struct {
	// factory builds runners bound to a cluster inventory.
	factory port.SchemaClientFactory
	// flagger touches the keeper force_restore_data znode for the
	// init-FAILED readonly heal (nil = the capability is absent;
	// init-failed tables are reported but not healed — the loud
	// degradation, never a guessed heal).
	flagger port.ForceRestoreFlagger
}

// NewSchemaService builds the service over a runner factory.
func NewSchemaService(factory port.SchemaClientFactory) *SchemaService {
	return &SchemaService{factory: factory}
}

// WithForceRestoreFlagger binds the keeper znode capability (the
// init-FAILED readonly heal's transport). Optional: without it the
// init-failed pass reports but cannot heal.
func (s *SchemaService) WithForceRestoreFlagger(f port.ForceRestoreFlagger) *SchemaService {
	s.flagger = f
	return s
}

// SchemaRequest binds a schema operation to one cluster context.
type SchemaRequest struct {
	// ClusterName is the ClickHouse cluster name (remote_servers naming).
	ClusterName string
	// Credentials connect to the target hosts.
	Credentials domain.Credentials
	// Hosts is the cluster inventory (operations address hosts by Name).
	Hosts []domain.HostIdentity
	// HTTPPort is the HTTP interface port (default 8123).
	HTTPPort int
	// Secure marks whether the cluster requires TLS connections.
	Secure bool
	// NormalizedCR is the SpecLoader's CHI output. Required: the operator
	// schemer names replicas by walking the CR's cluster/shard tree, and
	// hand-built Host values panic in its namer.
	NormalizedCR any
}

// runner builds a client for one operation.
func (s *SchemaService) runner(ctx context.Context, request SchemaRequest) (port.SchemaRunner, error) {
	return s.factory.NewClient(ctx, port.NewSchemaClientRequest{
		ClusterName:  request.ClusterName,
		Credentials:  request.Credentials,
		Hosts:        request.Hosts,
		HTTPPort:     request.HTTPPort,
		Secure:       request.Secure,
		NormalizedCR: request.NormalizedCR,
	})
}

// DefaultSchemaSyncRetries is the default -retries for schema sync: two
// full create passes. One pass re-runs the operator's per-SQL retry, but a
// materialized view whose TO-target table lands later in the same batch
// stays uncreated until a fresh pass re-orders the batch against the now
// existing targets .
const DefaultSchemaSyncRetries = 2

// SyncSchema creates all schema objects on a target host from cluster
// peers — the new-reader boot case (HostCreateTables).
//
// Retries: up to sync.Retries full create passes (0 = DefaultSchemaSyncRetries),
// stopping early on the first clean pass. The operator's ExecHost retry
// re-runs failed SQLs, but ordering across a fresh pass is not guaranteed
// the retry loop makes the second pass deterministic instead of relying on
// a manual re-run.
//
// PruneStaleSelf: when a pass fails with Code 253 (the target's replica
// registration already exists in Keeper — an ASG/VMSS replacement node
// re-claiming an ordinal whose dead predecessor left a PERSISTENT znode),
// the service runs the DropReplica pair for the failed registration and
// continues the loop. The drop name comes from the error the pass
// reported; the runner's DropReplica refuses any name not in the
// inventory, and the flag is opt-in (default false). Unflagged, the 253
// error returns immediately with the flag named — every retry would hit
// the same znode, so a retry would only burn a pass.
//
// Non-253 failures retry through every pass: the MV whose TO-target
// lands later in the batch is what the NEXT pass fixes.
//
// Without the retry loop: a single-pass failure leaves the node schemaless
// until a human re-runs the command . Without
// PruneStaleSelf: a recycled ordinal cannot recover its own registration
// at all — CREATE and the auto-ATTACH fallback both fail on a fresh disk,
// and only a manual `replica drop` on a peer unblocks it.
func (s *SchemaService) SyncSchema(ctx context.Context, request SchemaRequest, sync domain.SchemaSyncRequest) error {
	runner, err := s.runner(ctx, request)
	if err != nil {
		return err
	}
	retries := sync.Retries
	if retries <= 0 {
		retries = DefaultSchemaSyncRetries
	}
	runOn := sync.RunOn
	if runOn == "" {
		runOn = sync.HostName
	}

	var lastErr error
	for pass := 0; pass < retries; pass++ {
		lastErr = runner.SyncSchema(ctx, sync)
		if lastErr == nil {
			return nil
		}
		staleName, stale := staleSelfRegistration(lastErr)
		if !stale {
			// A non-253 failure (e.g. an MV whose TO-target table lands
			// later in the batch) is what the NEXT pass fixes — the loop
			// continues; only the last pass's error is returned.
			continue
		}
		if !sync.PruneStaleSelf {
			// The registration already exists and the caller did not
			// opt into reclaiming it: retrying without the drop is
			// pointless (every pass hits the same znode), so the error
			// surfaces immediately with the recovery flag named.
			return fmt.Errorf("%w (pass %d/%d — reclaim it with -prune-stale-self)", lastErr, pass+1, retries)
		}
		// The failed pass named the registration the target could not
		// claim. Drop exactly that one — the runner's DropReplica
		// validates it against the inventory before any SQL runs.
		if err := runner.DropReplica(ctx, domain.ReplicaDropRequest{HostName: staleName, RunOn: runOn}); err != nil {
			return fmt.Errorf("prune stale self: %w (after %w)", err, lastErr)
		}
	}
	return lastErr
}

// staleSelfRegistration extracts the replica name a failed create pass
// reported as already registered (Code 253) — error shape is
// Replica /clickhouse/tables/.../replicas/<name> already exists (the
// znode path form), or the quoted Replica '<name>' already exists form
// some drivers wrap it in.
func staleSelfRegistration(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	if !strings.Contains(msg, "Code: 253") && !strings.Contains(msg, "already exists") {
		return "", false
	}
	for _, marker := range []string{"/replicas/", "'"} {
		if !strings.Contains(msg, marker) {
			continue
		}
		if name := extractReplicaName(msg, marker); name != "" {
			return name, true
		}
	}
	return "", false
}

// extractReplicaName returns the replica name named by the marker's
// segment in msg: for "/replicas/" it is the token after the LAST
// occurrence (the znode path's final component — the path prefix can
// itself contain the marker on deeper trees, while the replica segment
// is always last); for "'" the quoted name between the LAST-but-one
// and the LAST quote (the closing quote of Replica '<name>').
func extractReplicaName(msg, marker string) string {
	if marker == "'" {
		// Quoted form: the name sits between the closing pair of quotes.
		second := strings.LastIndexByte(msg, '\'')
		if second <= 0 {
			return ""
		}
		first := strings.LastIndexByte(msg[:second], '\'')
		if first < 0 {
			return ""
		}
		return msg[first+1 : second]
	}
	idx := strings.LastIndex(msg, marker)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(marker):]
	// The replica name ends at the first character that cannot appear
	// in a znode name (the path ends at whitespace or the next path
	// boundary).
	end := strings.IndexAny(rest, " \t\r\n:\"<>")
	if end < 0 {
		end = len(rest)
	}
	name := rest[:end]
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}

// DropReplica removes a dead replica's Keeper registration via a live peer
// (HostDropReplica — SYSTEM DROP REPLICA).
func (s *SchemaService) DropReplica(ctx context.Context, request SchemaRequest, drop domain.ReplicaDropRequest) error {
	runner, err := s.runner(ctx, request)
	if err != nil {
		return err
	}
	return runner.DropReplica(ctx, drop)
}

// SyncReplica re-queues replication on a restarted node
// (HostSyncTables — SYSTEM SYNC REPLICA).
func (s *SchemaService) SyncReplica(ctx context.Context, request SchemaRequest, sync domain.ReplicaSyncRequest) error {
	runner, err := s.runner(ctx, request)
	if err != nil {
		return err
	}
	return runner.SyncReplica(ctx, sync)
}

// QueueHealResult reports the queue-heal outcome: what was stuck, what
// was restarted, and why nothing happened when nothing happened.
type QueueHealResult struct {
	// StuckTables lists the tables the detection found past the
	// retry threshold (before any busy-skip gate).
	StuckTables []string
	// OrphanTables lists the tables with NO_REPLICA_HAS_PART queue
	// entries (the orphan-merge class — healed with SYNC only).
	OrphanTables []string
	// OrphanSynced lists the tables that received the orphan SYNC
	// heal.
	OrphanSynced []string
	// Restarted lists the tables that received SYSTEM RESTART REPLICA.
	Restarted []string
	// MergesRunning is the node's in-flight merge count at detection
	// time (the busy-skip gate's evidence).
	MergesRunning int
	// SkippedReason, when non-empty, names why no restart ran: the
	// busy-skip gate ("merges running") or an empty detection ("no
	// stuck entries").
	SkippedReason string
	// ReadonlyTables lists the read-only tables found (Keeper session
	// loss — the separate detection pass).
	ReadonlyTables []string
	// InitFailedTables lists the read-only tables whose queue never
	// started (the attach thread refused them — the
	// force_restore_data heal class, a different pass from the
	// session-loss readonly).
	InitFailedTables []string
	// Restored lists the tables that received the readonly heal
	// (RESTART REPLICA when the Keeper replica path is present,
	// RESTORE REPLICA when absent).
	Restored []string
	// SkippedHeals lists readonly tables whose Keeper-path probe
	// failed — the heal that would have been a guess is deferred to
	// the next pass.
	SkippedHeals []string
	// FailedTables lists tables whose restart or sync failed during this
	// pass; errors are contained per table so remaining tables still heal.
	FailedTables []string
}

// QueueHeal detects and recovers a node's blocked replication queue:
// entries past the retry threshold with the merge pool idle receive
// SYSTEM RESTART REPLICA per stuck table followed by SyncReplica.
func (s *SchemaService) QueueHeal(ctx context.Context, request SchemaRequest, heal domain.QueueHealRequest) (*QueueHealResult, error) {
	runner, err := s.runner(ctx, request)
	if err != nil {
		return nil, err
	}
	stuck, err := runner.FindStuckQueueTables(ctx, heal)
	if err != nil {
		return nil, err
	}
	result := &QueueHealResult{StuckTables: stuck}
	if len(stuck) > 0 {
		if heal.BusyMergesSkip {
			merges, err := runner.CountRunningMerges(ctx, heal)
			if err != nil {
				return nil, err
			}
			result.MergesRunning = merges
			if merges > 0 {
				result.SkippedReason = "merges running"
			}
		}
		if result.SkippedReason == "" {
			for _, table := range stuck {
				healTable := heal
				healTable.Table = table
				if err := runner.RestartReplica(ctx, healTable); err != nil {
					// The 2026-11-17 live finding: SYNC REPLICA after a
					// restart re-processes the ENTIRE replication log —
					// on a 14k-entry queue it exceeds the command
					// timeout, and the pass ABORTED on the first
					// table, leaving every later table unhealed for the
					// rest of the day. A failed restart is per-table
					// state: log it, record the failure, and CONTINUE —
					// the next hourly pass retries the failed one.
					result.FailedTables = append(result.FailedTables, table)
					continue
				}
				// SYNC REPLICA after the restart: the restart re-inits
				// in-memory queue state from Keeper, and the sync
				// re-processes the log — re-enqueueing the part checks
				// whose PartCheckThread resolves or deletes the
				// orphan-merge entries (the NO_REPLICA_HAS_PART class:
				// the graceful path's enqueuePartForCheck only runs on
				// the non-throw branch, so poisoned entries wait for
				// the check thread's own slow timer without this).
				if err := runner.SyncReplica(ctx, domain.ReplicaSyncRequest{HostName: heal.RunOn}); err != nil {
					// Same per-table containment: a sync that times out
					// on one loaded table must not strand the rest of
					// the heal list (the restart already succeeded; the
					// sync is the accelerant, not the heal itself).
					result.FailedTables = append(result.FailedTables, table)
					continue
				}
				result.Restarted = append(result.Restarted, table)
			}
		}
	} else {
		result.SkippedReason = "no stuck entries"
	}

	// FindStuckQueueTables covers the retry-threshold class; the
	// ORPHAN class (NO_REPLICA_HAS_PART — a queued merge whose source
	// part no live replica holds) is detected separately: its heal is
	// SYNC REPLICA (re-enqueue the part checks whose PartCheckThread
	// resolves or deletes the entry), NOT a restart — safe while
	// merges run.
	orphan, err := runner.FindOrphanMergeTables(ctx, heal)
	if err != nil {
		return nil, err
	}
	result.OrphanTables = orphan
	for _, table := range orphan {
		healTable := heal
		healTable.Table = table
		if err := runner.SyncReplica(ctx, domain.ReplicaSyncRequest{HostName: heal.RunOn}); err != nil {
			// Per-table containment (the restart-loop rationale above):
			// the orphan sync is the heal itself, so a failure IS
			// table-scoped loss — record it, continue with the rest,
			// let the next pass retry.
			result.FailedTables = append(result.FailedTables, table)
			continue
		}
		result.OrphanSynced = append(result.OrphanSynced, table)
	}

	// The init-FAILED readonly pass (a different failure class from
	// the session-loss readonly below): the attach thread refused the
	// table (TOO_MANY_UNEXPECTED_DATA_PARTS et al.) and its queue
	// never started — last_queue_update at the epoch. A plain RESTART
	// re-runs the same failed attach; the heal is the source's own
	// escape hatch: flags/force_restore_data on the replica path
	// makes the next RESTART skip the parts sanity check, detach the
	// unexpected parts and sync from the log
	// (StorageReplicatedMergeTree.cpp:558). Verified live on the
	// 2026-11-17 backup-0: three summary tables readonly for hours
	// healed in one flag+restart pass.
	//
	// Evaluated BEFORE the plain readonly pass so a table whose
	// last_queue_update is at the epoch is never hit with a flagless
	// RESTART first.
	initFailed, err := runner.FindInitFailedReadonlyTables(ctx, heal)
	if err != nil {
		return result, fmt.Errorf("init-failed readonly detection: %w", err)
	}
	result.InitFailedTables = initFailed
	initFailedSet := make(map[string]bool, len(initFailed))
	for _, table := range initFailed {
		initFailedSet[table] = true
		healed, err := s.healInitFailedTable(ctx, runner, heal, table)
		if healed {
			result.Restored = append(result.Restored, table)
		}
		if err != nil {
			return result, err
		}
		if !healed && err == nil {
			result.SkippedHeals = append(result.SkippedHeals, table)
		}
	}

	// The readonly heal: a table flipped read-only by a lost Keeper
	// session stays read-only until the right heal runs — the branch
	// lives in healReadonlyTable (Keeper-path present ⇒ RESTART,
	// absent ⇒ RESTORE).
	readonly, err := runner.FindReadonlyTables(ctx, heal)
	if err != nil {
		return result, fmt.Errorf("readonly detection: %w", err)
	}
	result.ReadonlyTables = readonly
	for _, table := range readonly {
		if initFailedSet[table] {
			continue
		}
		healed, err := s.healReadonlyTable(ctx, runner, heal, table)
		if healed {
			result.Restored = append(result.Restored, table)
		}
		if err != nil {
			return result, err
		}
		if !healed && err == nil {
			result.SkippedHeals = append(result.SkippedHeals, table)
		}
	}
	return result, nil
}

// healReadonlyTable runs ONE readonly table's heal, branching on the
// Keeper registration (the ClickHouse contract,
// StorageReplicatedMergeTree.cpp:7629: restoreMetadataInZooKeeper
// REFUSES when the replica path exists — "nothing to restore").
//
// Path PRESENT ⇒ the readonly is local session/queue state: heal with
// RESTART REPLICA (re-reads the keeper log pointer; the 2026-11-17
// backup-0 finding: 3 summary tables sat readonly for hours with
// registered keeper paths — RESTORE would error, only the restart
// heals that shape). Path ABSENT ⇒ the metadata must be re-created:
// heal with RESTORE REPLICA (the operator's ClickHouseReadonlyReplica
// analog — its pod recreation is the absent-path shape).
//
// healed=false + nil error is the deferred verdict: the Keeper-path
// probe failed (keeper unreachable, query timeout) and a guessed heal
// is worse than none — a RESTORE against a registered path errors and
// a RESTART against an absent one is a no-op; the next hourly pass
// retries.
func (s *SchemaService) healReadonlyTable(ctx context.Context, runner port.SchemaRunner, heal domain.QueueHealRequest, table string) (healed bool, err error) {
	healTable := heal
	healTable.Table = table
	registered, err := runner.ReplicaKeeperPathExists(ctx, healTable)
	if err != nil {
		return false, nil // deferred: never guess the heal
	}
	if registered {
		if err := runner.RestartReplica(ctx, healTable); err != nil {
			return false, fmt.Errorf("restart replica %s (readonly, keeper path present): %w", table, err)
		}
		return true, nil
	}
	if err := runner.RestoreReplica(ctx, healTable); err != nil {
		return false, fmt.Errorf("restore replica %s (readonly, keeper path absent): %w", table, err)
	}
	return true, nil
}

// healInitFailedTable runs ONE init-failed table's heal: the
// force_restore_data flag + RESTART REPLICA (the attach thread
// refused the table — TOO_MANY_UNEXPECTED_DATA_PARTS and its class —
// so the restart alone re-runs the same failed attach; the flag is
// the source's own skip-the-sanity-check switch,
// StorageReplicatedMergeTree.cpp:558).
//
// healed=false + nil error is the deferred verdict: the keeper
// capability is absent (the flag cannot be touched through SQL
// without allow_zookeeper_write; the app layer binds the
// keeper-client CLI). The next pass with the capability heals.
func (s *SchemaService) healInitFailedTable(ctx context.Context, runner port.SchemaRunner, heal domain.QueueHealRequest, table string) (healed bool, err error) {
	if s.flagger == nil {
		return false, nil // capability absent: report, never guess
	}
	healTable := heal
	healTable.Table = table
	if err := s.flagger.SetForceRestoreFlag(table); err != nil {
		return false, fmt.Errorf("force-restore flag %s: %w", table, err)
	}
	if err := runner.RestartReplica(ctx, healTable); err != nil {
		return false, fmt.Errorf("restart replica %s (init-failed, after force-restore flag): %w", table, err)
	}
	return true, nil
}

// ReplicaGcResult reports the outcome of a GC sweep so the CLI can print
// the TSV list and the human summary from one pass.
type ReplicaGcResult struct {
	// Stale is every stale host name found (before pattern filtering).
	Stale []string
	// Dropped is every host whose registration was removed successfully.
	Dropped []string
	// Failed is every host whose drop errored (already removed from
	// Dropped).
	Failed []string
}

// DefaultReplicaGcPattern is the eligibility glob for a GC sweep. The
// AWS stack's dead-replica-gc.sh gated on reader-%: only readers are
// disposable; a stale writer or keeper registration must survive a GC
// pass untouched, so the default keeps that gate.
const DefaultReplicaGcPattern = "reader-*"

// GcReplica detects stale replica registrations on a peer and drops them
// in one pass — the dead-replica GC that replaced the weekly bash timer
// on the AWS stack.
//
// The pattern gate is applied here, not in SQL: detection stays a simple
// query (stays easy to reason about), and the eligibility rule lives in
// one place the CLI documents. A host that does not match is reported as
// stale but never dropped. The exclude list is the caller's second gate
// (chinit's writer pass excludes hosts with live ordinal leases): an
// excluded host is reported as stale but skipped at drop time — the
// sweep stays visible while the liveness override wins.
//
// Drop failures are logged per-host and do not abort the sweep — one
// flaky replica must not shield the rest from cleanup. The caller is
// expected to exit non-zero when Failed is non-empty.
func (s *SchemaService) GcReplica(ctx context.Context, request SchemaRequest, gc domain.ReplicaGcRequest) (*ReplicaGcResult, error) {
	if gc.RunOn == "" {
		return nil, fmt.Errorf("gc: -on peer is empty")
	}
	pattern := gc.NamePattern
	if pattern == "" {
		pattern = DefaultReplicaGcPattern
	}
	// Validate the glob BEFORE any SQL: an invalid pattern (e.g. "[a-")
	// must fail fast as a usage error, not surface as a silent no-op.
	if _, err := path.Match(pattern, ""); err != nil {
		return nil, fmt.Errorf("gc: invalid name pattern %q: %w", pattern, err)
	}
	gc.NamePattern = pattern
	excluded := gc.NewExcludeSet()

	runner, err := s.runner(ctx, request)
	if err != nil {
		return nil, err
	}
	stale, err := runner.FindStaleReplicas(ctx, gc)
	if err != nil {
		return nil, err
	}

	result := &ReplicaGcResult{Stale: stale}
	for _, host := range stale {
		matched, err := path.Match(pattern, host)
		// The pattern was validated above; a per-host match error cannot
		// happen. Being defensive costs one line and never reaps on
		// ambiguity.
		if err != nil || !matched {
			continue
		}
		if excluded[host] {
			// The caller's liveness guard outranks staleness: the host
			// stays in Stale (the sweep's visibility contract) but its
			// registration must survive the pass.
			continue
		}
		if err := runner.DropReplica(ctx, domain.ReplicaDropRequest{HostName: host, RunOn: gc.RunOn}); err != nil {
			result.Failed = append(result.Failed, host)
			continue
		}
		result.Dropped = append(result.Dropped, host)
	}
	return result, nil
}
