// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

func runDeadReplicaGc(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	// The ordinal-0 gate: every writer installs this timer, only the
	// holder of writer-{prefix}0 runs the sweep (with ASG writers the
	// ordinal is a lease, not a machine). The gate may ELECT a
	// lowest-ordinal live writer when the ordinal-0 lease is vacant —
	// see gateMaintenanceJob's vacancy note. The live claims are
	// listed ONCE here and threaded through both the gate and the
	// writer pass (one DynamoDB scan per run, not two).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	claims, err := listLiveClaims(ctx, st, gcLivelinessFrom(st))
	if err != nil {
		logger.GetLogger(ctx).Warn("dead-replica-gc: live-claims listing failed; treating as no claims",
			"error", err.Error())
		claims = nil
	}
	if gateMaintenanceJobWithClaims(ctx, st, claims) {
		return nil
	}
	return deadReplicaGc(ctx, st, claimsLimiter(claims))
}

// claimsLimiter adapts a claims list into the writer pass's liveness
// source: the sweep takes a LeaseLister, and with the claims already
// listed (once, at the entry) the adapter answers them without a
// second scan. A nil claims slice adapts as a lister-less pass (the
// writer pass skips with its WARNING; the reader pass still ran).
type claimsLister struct {
	claims []port.Claim
	table  string
}

func claimsLimiter(claims []port.Claim) port.LeaseLister {
	if claims == nil {
		return nil
	}
	table := ""
	if spec, err := loadInventorySpec(); err == nil && spec != nil {
		table = spec.ClaimerTableName
	}
	return &claimsLister{claims: claims, table: table}
}

func (c *claimsLister) ListActiveClaims(ctx context.Context, req port.ListClaimsRequest) ([]port.Claim, error) {
	return c.claims, nil
}

// listLiveClaims resolves the cluster's live lease claims through the
// provider's LeaseLister. Table resolution mirrors writerGcExcludes:
// the identity's lease table, falling back to the bundle spec's claimer
// table (the static-writer SSM shape). A nil lister answers (nil, nil):
// the caller treats no-claims as the fail-safe skip, and the writer
// pass's own nil-lister warning path reports the reason.
func listLiveClaims(ctx context.Context, st *nodeState, lister port.LeaseLister) ([]port.Claim, error) {
	if lister == nil {
		return nil, nil
	}
	table := st.Identity.LeaseTable
	if table == "" {
		if spec, specErr := loadInventorySpec(); specErr == nil && spec != nil {
			table = spec.ClaimerTableName
		}
	}
	if table == "" {
		return nil, nil
	}
	return lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: table})
}

// gcLivelinessFrom builds the writer pass's liveness source the way
// every subcommand reaches a capability: the provider's LeaseLister
// (via buildProvider — the boot's provider instance died with its
// process; the var is the specs' injection seam). The two-pass sweep
// itself takes the lister as a parameter (deadReplicaGc), so the
// specs drive the passes without the registry.
func gcLivelinessFrom(st *nodeState) port.LeaseLister {
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return nil
	}
	return port.AsLeaseLister(provider)
}

// writerGcExcludes builds the writer pass's liveness guard: the
// ACTIVE writer claims (never dropped, always reported) plus the
// skip reason when the pass cannot run safely. A nil lister or an
// unlistable table skips the writer pass — the reader pass still
// swept — because dropping writer registrations without liveness
// data risks a mid-boot re-join.
func writerGcExcludes(ctx context.Context, st *nodeState, lister port.LeaseLister) (excludes []string, skipNote string, err error) {
	if lister == nil {
		return nil, "cloud has no lease-listing capability", nil
	}
	table := st.Identity.LeaseTable
	if table == "" {
		// The maintenance holder need not hold a lease itself
		// (theoretically it always does in this stack, but a manual
		// SSM run on a static writer must still find the table the
		// boot's spec names).
		if spec, specErr := loadInventorySpec(); specErr == nil && spec != nil {
			table = spec.ClaimerTableName
		}
	}
	if table == "" {
		return nil, "", fmt.Errorf("no lease table to list (identity and bundle spec both carry none)")
	}
	claims, err := lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: table})
	if err != nil {
		return nil, "", fmt.Errorf("live-claims listing failed: %w", err)
	}
	prefix := model.DefaultWriterHostPrefix
	if spec, specErr := loadInventorySpec(); specErr == nil && spec != nil {
		prefix = spec.writerPrefix()
	}
	for _, claim := range claims {
		if strings.HasPrefix(claim.Hostname, prefix) {
			excludes = append(excludes, claim.Hostname)
		}
	}
	sort.Strings(excludes)
	logger.GetLogger(ctx).Info("writer GC pass: live writer leases excluded from the sweep",
		"excluded", len(excludes),
		"name_pattern", prefix+"*")
	return excludes, "", nil
}

// deadReplicaGc is the two-pass sweep over a settled identity and a
// liveness source (nil lister = skip the writer pass; the reader pass
// still runs). Extracted from runDeadReplicaGc so the specs can drive
// the passes without the provider registry. The maintenance holder IS
// the detection peer: detection runs against this node's own
// system.replicas and the clusterAllReplicas fan-out rides the
// node's rendered remote_servers config — self is the only host the
// request must reach.
func deadReplicaGc(ctx context.Context, st *nodeState, lister port.LeaseLister) error {
	// Pass 1: readers, unconditionally (the empty pattern keeps the
	// service's reader-* default).
	logger.GetLogger(ctx).Info("dead-replica-gc: reader pass starting", "on", st.Identity.Hostname)
	res, err := clusterOps.GcReplica(ctx, clusterops.GcReplicaRequest{
		Cluster: clusterRequest(st),
		RunOn:   st.Identity.Hostname,
	})
	if err != nil {
		return fmt.Errorf("reader GC pass failed: %w", err)
	}
	logGcResult(ctx, "reader", res)
	logger.GetLogger(ctx).Info("dead-replica-gc: reader pass complete")

	// Pass 2: writers, only with liveness data. The active claims are
	// the exclude set — stale-but-live writers are reported and left
	// alone.
	excludes, skip, err := writerGcExcludes(ctx, st, lister)
	if err != nil || skip != "" {
		logger.GetLogger(ctx).Warn("WARNING: skipping the writer GC pass; the reader pass ran",
			"reason", reasonOf(err, skip))
		return nil
	}
	res, err = clusterOps.GcReplica(ctx, clusterops.GcReplicaRequest{
		Cluster:      clusterRequest(st),
		RunOn:        st.Identity.Hostname,
		NamePattern:  model.DefaultWriterHostPrefix + "*",
		ExcludeHosts: excludes,
	})
	if err != nil {
		return fmt.Errorf("writer GC pass failed: %w", err)
	}
	logGcResult(ctx, "writer", res)
	logger.GetLogger(ctx).Info("dead-replica-gc: writer pass complete")
	return nil
}

// logGcResult records a pass outcome in the operator's triage shape
// (stale/dropped/failed counts; the attributes the weekly timer's log
// queries).
func logGcResult(ctx context.Context, pass string, res *clusterops.GcResult) {
	if res == nil {
		return
	}
	log := logger.GetLogger(ctx)
	log.Info("dead-replica-gc: pass result",
		"pass", pass, "stale", len(res.Stale), "dropped", len(res.Dropped), "failed", len(res.Failed))
	for _, host := range res.Dropped {
		log.Info("dead-replica-gc: dropped", "host", host)
	}
	for _, host := range res.Failed {
		log.Warn("dead-replica-gc: FAILED", "host", host)
	}
}

// reasonOf renders the skip reason from either the error path (the
// listing failed) or the note path (no capability).
func reasonOf(err error, note string) string {
	if err != nil {
		return err.Error()
	}
	return note
}

// runOrphanedGc is `chinit orphaned-gc` (the daily timer on every
// writer; the ordinal-0 holder does the work): the account-hygiene
// census — untagged unattached volumes older than the age threshold
// are DELETED (untagged = unowned; every resource this stack creates
// carries tags), and unassociated EIPs / untagged snapshots are
// REPORTED (never deleted: NAT EIPs legitimately show
// AssociationId==null, and snapshots could be packer bases).
//
// Why the volume category deletes and nothing else does: an
// unattached untagged volume is debris by definition (the incident:
// inline ebs_block_device volumes with
// delete_on_termination=false piled up tens of TB in days of
// rebuilds); the report-only categories all have legitimate
// unowned-looking states a human must adjudicate.
//
// The S3-parts census (parts no replica's metadata references) stays
// with the bucket's lifecycle rules: the parts catalog compare is a
// schemer follow-up, and lifecycle expiry already bounds the same
// cost without a false-positive risk.
func runOrphanedGc(args []string) error {
	// The observation-mode switch: report and alarm without deleting.
	orphanDryRun = false
	for _, a := range args {
		if a == "-dry-run" {
			orphanDryRun = true
		}
	}
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	// Same ordinal-0 gate (with the vacancy election) as the
	// dead-replica GC: the two maintenance jobs travel together.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	claims, err := listLiveClaims(ctx, st, gcLivelinessFrom(st))
	if err != nil {
		logger.GetLogger(ctx).Warn("orphaned-gc: live-claims listing failed; treating as no claims",
			"error", err.Error())
		claims = nil
	}
	if gateMaintenanceJobWithClaims(ctx, st, claims) {
		return nil
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return fmt.Errorf("orphaned-gc: build provider: %w", err)
	}
	sweeper := port.AsOrphanSweeper(provider)
	if sweeper == nil {
		logger.GetLogger(ctx).Warn("orphaned-gc: provider has no orphan-sweep capability; skipping")
		return nil
	}
	result, err := sweeper.SweepOrphans(ctx, port.OrphanSweepRequest{
		Region:       st.Identity.Region,
		AgeThreshold: 24 * time.Hour,
		DryRun:       orphanDryRun,
	})
	if err != nil {
		return fmt.Errorf("orphaned-gc: %w", err)
	}
	log := logger.GetLogger(ctx)
	log.Info("orphaned-gc: sweep complete",
		"volumes_found", len(result.VolumesFound),
		"volumes_deleted", len(result.VolumesDeleted),
		"eips_reported", len(result.EIPsUnassociated),
		"snapshots_reported", len(result.SnapshotsUntagged))
	for _, v := range result.VolumesDeleted {
		log.Info("orphaned-gc: deleted volume", "volume", v)
	}
	for _, v := range result.VolumesFound {
		log.Info("orphaned-gc: candidate volume (dry-run or delete-failed)", "volume", v)
	}
	pushOrphanFindings(ctx, st, result)
	return nil
}

// gateMaintenanceJob answers whether this node should skip the
// maintenance jobs (true = skip and exit 0). With ASG writers
// "writer-0" is a LEASE ORDINAL — whichever instance currently holds
// it — so the gate compares the settled hostname (the state's
// authoritative identity) against the boot's writer prefix, exact
// match: `reader-0` must not match, and prefix tricks must never win.
// The prefix comes from the bundle spec the same way the inventory
// paths read it (the state carries identity, not topology); an
// unreadable spec falls back to the model default — the fail-safe
// direction (a custom-prefix holder that cannot read its spec skips
// rather than sweeping from the wrong designation).
//
// Non-writers (readers; SSM could invoke this manually) take the same
// skip path — maintenance is a writer job.
//
// The vacancy election (the 2026-11-16 finding): when no instance
// holds the ordinal-0 lease (writer-0 terminated mid-churn; the ASG
// re-claimed ordinals 1 and 2), the hard ordinal-0 match left EVERY
// writer skipping and the fleet's GC silently stopped — stale
// registrations accumulated until quorum math broke (inserts rejected
// over dead replicas). The lease table is the only component that
// knows the designation is vacant, and the maintenance timer already
// builds a provider to reach it (gcLivelinessFrom). When the lister
// names NO live ordinal-0 holder, the LOWEST-ORDINAL live writer
// elects itself: the sweep is idempotent (dropping a stale
// registration from any live writer is safe — the GC's own exclude
// guard still protects every live lease), and the reader pass's
// detection peer is only required to be a live writer with the
// rendered remote_servers, which any holder is. Fail-safe direction
// unchanged: no lister, an unreachable lease table, or a listing
// error means no election data — every non-0 writer skips (the
// historic behavior) rather than a wrong guess.
func gateMaintenanceJob(ctx context.Context, st *nodeState) bool {
	return gateMaintenanceJobWithClaims(ctx, st, nil)
}

// gateMaintenanceJobWithClaims is the gate over an explicit claims
// list (the specs' injection seam — no provider registry).
func gateMaintenanceJobWithClaims(ctx context.Context, st *nodeState, claims []port.Claim) bool {
	prefix := model.DefaultWriterHostPrefix
	if spec, err := loadInventorySpec(); err == nil && spec != nil {
		prefix = spec.writerPrefix()
	}
	ordinalZero := prefix + "0"
	if st.Identity.Role == model.RoleWriter && st.Identity.Hostname == ordinalZero {
		return false
	}
	// Vacancy election: with no live ordinal-0 holder, the
	// lowest-ordinal live writer takes the designation.
	if st.Identity.Role == model.RoleWriter && len(claims) > 0 {
		ordinalZeroHeld := false
		selfLive := false
		selfOrdinal := -1
		lowestOrdinal := -1
		lowestName := ""
		for _, claim := range claims {
			if !strings.HasPrefix(claim.Hostname, prefix) {
				continue
			}
			ordinal, err := strconv.Atoi(strings.TrimPrefix(claim.Hostname, prefix))
			if err != nil {
				continue
			}
			if claim.Hostname == ordinalZero {
				ordinalZeroHeld = true
			}
			if claim.Hostname == st.Identity.Hostname {
				selfLive = true
				selfOrdinal = ordinal
			}
			if lowestOrdinal == -1 || ordinal < lowestOrdinal {
				lowestOrdinal = ordinal
				lowestName = claim.Hostname
			}
		}
		if !ordinalZeroHeld && selfLive && lowestName == st.Identity.Hostname {
			logger.GetLogger(ctx).Info("maintenance: ordinal-0 lease is vacant; electing the lowest-ordinal live writer",
				"ordinal_zero", ordinalZero,
				"elected", lowestName,
				"self_ordinal", selfOrdinal)
			return false
		}
	}
	logger.GetLogger(ctx).Info("maintenance jobs run on ordinal 0; this node holds a different ordinal; skipping",
		"ordinal_zero", ordinalZero,
		"this_node", st.Identity.Hostname)
	return true
}

// orphanDryRun is the orphaned-gc observation-mode switch (the unit
// pins it via the timer's ExecStart arguments during a new pattern's
// observation window).
var orphanDryRun bool

// pushOrphanFindings publishes the sweep's outcome (the LeakGuard
// metric shape the orphan alarm evaluates — findings page through
// the normal alarm topic instead of silently accumulating).
func pushOrphanFindings(ctx context.Context, st *nodeState, result *port.OrphanSweepResult) {
	if result == nil {
		return
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return
	}
	pusher := port.AsMetricPusher(provider)
	if pusher == nil {
		return
	}
	_ = pusher.PutMetric(ctx, port.MetricPushRequest{
		Namespace:  "ClickHouse/LeakGuard",
		Name:       "OrphanVolumesFound",
		Value:      float64(len(result.VolumesFound) + len(result.EIPsUnassociated) + len(result.SnapshotsUntagged)),
		Unit:       "Count",
		Dimensions: map[string]string{"ClusterName": st.clusterName()},
	})
}
