// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// runSpotWatch polls the cloud spot/preemption endpoint and executes a
// graceful drain when an interruption notice is received.
func runSpotWatch(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	watcher := port.AsSpotSignals(provider)
	if watcher == nil {
		return fail.Fatal(fmt.Errorf("cloud %s has no spot-watch capability", provider.Name()))
	}

	ctx := context.Background()
	logger.GetLogger(ctx).Info("spot-watch: polling for interruption notices", "cloud", provider.Name())
	notice, ok := <-watcher.Watch(ctx)
	if !ok {
		return nil // context ended without a notice
	}
	logger.GetLogger(ctx).Warn("spot-watch: INTERRUPTION DETECTED — draining",
		"deadline_seconds", notice.DeadlineSeconds,
		"raw", notice.Raw)
	return drain(ctx, st, provider, drainOptions{DeadlineSeconds: notice.DeadlineSeconds, Reason: drainReasonSpot})
}

// drainReason values — the invocation context a drain runs under.
// Why named: the termination lifecycle hook (SSM → `chinit drain`)
// and the spot watcher share the drain; the reason lands in the log
// so post-termination forensics can tell a scale-in from a spot
// reclaim apart.
const (
	drainReasonSpot            = "spot"
	drainReasonTerminationHook = "termination-hook"
	drainReasonRollout         = "rollout"
)

// defaultDrainDeadlineSeconds is the CLI drain's budget — 240s,
// matching the Terraform termination-hook's drain allowance so the
// SSM invocation and the hook heartbeat run out together (the hook
// Lambda CONTINUEs on our non-zero exit; exceeding its budget has the
// same outcome, so a local overage must never be the failure mode).
const defaultDrainDeadlineSeconds = 240

// drainOptions is the drain's tunables: the deadline the drain runs
// inside and the reason (log context).
type drainOptions struct {
	// DeadlineSeconds bounds the whole drain (spot: the notice's
	// reclaim window; termination hook: 240s — the hook budget).
	DeadlineSeconds int
	// Reason names the trigger: drainReasonSpot, drainReasonShutdown
	// or drainReasonTerminationHook.
	Reason string
	// SkipServerShutdown leaves the local server RUNNING after the
	// drain's replica/DNS/lease work: the shutdown-drain unit runs as
	// ExecStop inside systemd's shutdown transaction (ordered before
	// clickhouse-server stops), so the OS teardown owns the stop
	// issuing SYSTEM SHUTDOWN from inside would fight the very
	// transaction that invoked the drain.
	SkipServerShutdown bool
}

// runDrain is `chinit drain` (the ASG termination lifecycle hook's
// SSM invocation): reloads the settled identity, then performs the
// graceful drain — the same sequence the spot watcher runs, with the
// hook's generous budget instead of a spot notice's reclaim window.
// Writers are on-demand only (no spot notices), so the termination
// hook is their drain trigger; the SSM agent is the entry point and
// no systemd unit is needed.
//
// Exit contract: a node whose state cannot be read exits 1 — a node
// that never settled identity cannot drain safely, and the SSM
// failure makes the hook Lambda CONTINUE (the instance dies on
// schedule, the weekly GC reaps the registration).
func runDrain(args []string) error {
	fs := flag.NewFlagSet("chinit drain", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	reason := fs.String("reason", drainReasonTerminationHook, "drain trigger for the log: spot|termination-hook")
	timeout := fs.Int("timeout", defaultDrainDeadlineSeconds, "drain budget in seconds (the termination hook's drain allowance)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *reason {
	case drainReasonSpot, drainReasonShutdown, drainReasonTerminationHook:
	default:
		return fmt.Errorf("invalid -reason %q: want %q, %q or %q", *reason, drainReasonSpot, drainReasonShutdown, drainReasonTerminationHook)
	}
	if *timeout <= 0 {
		return fmt.Errorf("invalid -timeout %d: must be positive", *timeout)
	}
	st, err := loadNodeState(*statePath)
	if err != nil {
		// Deliberately a PLAIN error (exit 1, not the boot-fatal 2):
		// SSM needs any non-zero exit, and the text tells the
		// operator reading the SSM log why the drain declined plus
		// what happens next (hook CONTINUEs; weekly GC reaps).
		logger.GetLogger(context.TODO()).Warn("drain: cannot load node state; the termination hook will CONTINUE and the weekly dead-replica GC will clean up",
			"state_path", *statePath,
			"err", err)
		return fmt.Errorf("drain: cannot load node state from %s: %s\n"+
			"a node that never settled identity cannot drain safely; the ASG termination hook will CONTINUE and the weekly dead-replica GC will clean up (SSM sees this failure)",
			*statePath, err)
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	logger.GetLogger(context.TODO()).Info("drain: invoked",
		"hostname", st.Identity.Hostname,
		"reason", *reason,
		"budget_seconds", *timeout)
	return drain(context.Background(), st, provider, drainOptions{
		DeadlineSeconds: *timeout,
		Reason:          *reason,
	})
}

// drainReasonShutdown is the systemd-shutdown drain's reason (the
// log context that distinguishes an OS-shutdown drain from the
// hook's SSM invocation and the spot watcher's notice).
const drainReasonShutdown = "shutdown"

// shutdownDrainDeadlineSeconds is the shutdown drain's budget: 150s,
// bounded well below EC2's shutdown window (default 180s for the
// API-driven stop/scale-in path, with headroom for the OS's own
// teardown) — the drain MUST NOT be the reason a termination times
// out and the instance is killed mid-drain.
const shutdownDrainDeadlineSeconds = 150

// runDrainShutdown is `chinit drain-shutdown` — the in-node shutdown
// drain the chinit-shutdown-drain.service unit's ExecStop invokes.
//
// Why a daemon-adjacent unit instead of (only) the ASG termination
// hook's SSM dispatch: the OS shutdown path is the ONE signal every
// termination mode goes through — ASG scale-in, EC2 health-check
// replacement, spot reclaim at the deadline, operator stop. The
// hook's SSM dispatch has a known race (SSM cannot reliably deliver
// to a stopping instance — the agent is mid-teardown), and the
// health-check termination NEVER fires the hook at all. Running the
// drain from INSIDE the node at systemd stop time closes every gap
// with no external infra: EC2 ACPI-shuts the node → systemd stops
// units in reverse start order → this unit's ExecStop runs (ordered
// After=clickhouse-server, so ClickHouse is still UP to serve the
// drain's peer drop and SYSTEM STOP queries) → clickhouse-server
// stops.
//
// The reboot guard: a reboot RELOADS the same node with the same
// identity — its lease, DNS record and replica registration must all
// survive. systemd exposes the pending shutdown target in
// `systemctl list-jobs`; reboot.target in the job list means a
// scheduled reboot, and the drain declines (exit 0, loud log) so the
// node rejoins exactly as it was. poweroff.target / shutdown.target
// / halt.target (and the ASG-driven default target on instance
// termination) DO drain.
func runDrainShutdown(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	rebooting, err := shutdownIsReboot()
	if err != nil {
		// The check is best-effort BY DESIGN: a systemd IPC failure
		// (container, missing binary) must not turn into a skipped
		// drain on a real termination — the drain's failure modes are
		// already all best-effort. Log loudly and proceed.
		logger.GetLogger(context.TODO()).Warn("drain-shutdown: reboot check failed; assuming termination",
			"err", err)
	} else if rebooting {
		logger.GetLogger(context.TODO()).Info("drain-shutdown: reboot detected — skipping the drain (identity survives a reboot)",
			"hostname_path", statePath)
		return nil
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		// Plain error (exit 1): a node that never settled identity
		// cannot drain safely — the lease TTL (15 min) plus the
		// weekly dead-replica GC are the safety nets, same contract
		// as runDrain's SSM failure.
		logger.GetLogger(context.TODO()).Warn("drain-shutdown: cannot load node state; the lease TTL and the weekly dead-replica GC will clean up",
			"state_path", statePath,
			"err", err)
		return fmt.Errorf("drain-shutdown: cannot load node state from %s: %w", statePath, err)
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	logger.GetLogger(context.TODO()).Info("drain-shutdown: invoked",
		"hostname", st.Identity.Hostname,
		"budget_seconds", shutdownDrainDeadlineSeconds)
	if err := drain(context.Background(), st, provider, drainOptions{
		DeadlineSeconds: shutdownDrainDeadlineSeconds,
		Reason:          drainReasonShutdown,
		// SkipServerShutdown: systemd is already tearing the node
		// down — the drain runs as a unit's ExecStop ordered BEFORE
		// clickhouse-server stops. Issuing SYSTEM SHUTDOWN here (or
		// worse, `systemctl stop clickhouse-server`) would fight the
		// shutdown transaction that invoked us and can deadlock it
		// (a stop job waiting on a stop job).
		SkipServerShutdown: true,
	}); err != nil {
		return err
	}
	// The parting sweep: release every OTHER expired-slot record the
	// dying node leaves behind — an undrained predecessor's stale
	// record (its drain could not dispatch) would otherwise wedge
	// peers' schema-sync/fan-outs until the next renewal tick. This
	// node holds the provider handle and the zone already; the sweep
	// is the cheapest point on the fleet to run it (best-effort — a
	// failure never fails the shutdown).
	sweepArgs := service.ConvergeDNSArgs{
		LeaseTable:  st.Identity.LeaseTable,
		DNSZoneName: st.dnsZone(),
		// Hostname is the converger's guard input (the sweep never
		// self-heals — see SweepExpiredSlotRecords).
		Hostname: st.Identity.Hostname,
	}
	if spec, err := loadInventorySpec(); err == nil && spec.DNSZoneName != "" {
		sweepArgs.DNSZoneName = spec.DNSZoneName
	}
	if err := service.SweepExpiredSlotRecords(context.Background(), provider, sweepArgs); err != nil {
		logger.GetLogger(context.TODO()).Warn("drain-shutdown: expired-slot sweep failed; the renewal ticks own the retry",
			"err", err)
	}
	return nil
}

// shutdownIsReboot reports whether the pending systemd shutdown is a
// reboot (the job list carries reboot.target). The unit invokes this
// from ExecStop, mid-transaction: list-jobs is the one query that
// answers it without a second control-channel handshake.
var shutdownIsReboot = func() (bool, error) {
	out, err := runCommandWithEnv(context.Background(), nil, "systemctl", "list-jobs", "--no-legend", "--plain")
	if err != nil {
		return false, fmt.Errorf("systemctl list-jobs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.Contains(string(out), "reboot.target"), nil
}

// selectDrainPeer picks the LIVE peer the replica drop runs against,
// from the lease table's active claims.
//
// Why a live peer and not a hardcoded writer-0: the ASG termination
// hook fires for ANY writer, including the node holding ordinal 0
// (draining writer-0 against itself is a no-op drop that strands the
// registration) and including the LAST writer during a scale-in to
// min (writer-0 may already be gone). Preference order:
// 1. writer-0 (the ordinal the maintenance jobs and the weekly GC
// run on — the node most likely to be healthy);
// 2. the lowest-ordinal OTHER live writer;
// 3. any live reader (readers are full replicas — the peer-side
// chctl drop against a reader carries the same registration
// authority; the note in the log says so);
// 4. no capability/no claims: the writer-0 DNS-name fallback
// (static/interim clusters), degraded loudly — the weekly GC
// catches what a dead peer missed.
//
// Self is excluded (dropping a replica via itself is the no-op the
// local fallback already covers).
func selectDrainPeer(ctx context.Context, st *nodeState, spec *inventorySpec, lister port.LeaseLister, cloudName string) *port.Claim {
	self := st.Identity.Hostname
	// The writer-0 fallback answers when live discovery cannot (no
	// capability, no lease table, an unreadable spec, a scan outage):
	// a possibly-vacant peer beats NO peer for a best-effort drop,
	// degraded loudly — the weekly GC is the safety net.
	fallback := func(why string) *port.Claim {
		logger.GetLogger(ctx).Warn("WARNING: drain peer fell back to writer-0 without live-claims discovery — the peer may be vacant; the weekly dead-replica GC is the safety net",
			"dns_zone", st.dnsZone(),
			"why", why)
		return &port.Claim{Hostname: "writer-0"}
	}
	switch {
	case spec == nil:
		return fallback("bundle spec unreadable — no slot arithmetic")
	case lister == nil:
		return fallback("cloud " + cloudName + " has no lease-listing capability")
	case st.Identity.LeaseTable == "":
		return fallback("static node — identity holds no lease table")
	}
	claims, err := lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: st.Identity.LeaseTable})
	if err != nil {
		return fallback("cloud " + cloudName + " claims scan failed: " + err.Error())
	}
	// One pass, two buckets: writers and readers by ordinal, self
	// skipped. Ordinals, not scan order, pick the peer. A claim at or
	// beyond the writer ceiling is a stale row from a shrunk max — it
	// reserves no CR slot (the same rule claimsHostFlags applies).
	var writers, readers []*port.Claim
	for i := range claims {
		if claims[i].Hostname == self {
			continue
		}
		role, ordinal, ok := claimRoleOrdinal(claims[i].Hostname, spec.writerPrefix(), spec.readerPrefix())
		if !ok || claimCRSlot(role, ordinal, spec.writerCeiling()) < 0 {
			continue
		}
		if role == model.RoleWriter {
			writers = append(writers, &claims[i])
		} else {
			readers = append(readers, &claims[i])
		}
	}
	// Writers sort numerically (writer-10 > writer-2 — lexically 10
	// sorts first, which would pick the wrong "lowest" peer).
	ordinalOf := func(c *port.Claim) int {
		_, ordinal, _ := claimRoleOrdinal(c.Hostname, spec.writerPrefix(), spec.readerPrefix())
		return ordinal
	}
	sortPeers := func(peers []*port.Claim) {
		sort.SliceStable(peers, func(i, j int) bool { return ordinalOf(peers[i]) < ordinalOf(peers[j]) })
	}
	if len(writers) > 0 {
		sortPeers(writers)
		// writer-0 first when present; otherwise the lowest live
		// ordinal. Sorting already puts 0 first when present — the
		// explicit bucket is the documentation, not the arithmetic.
		return writers[0]
	}
	if len(readers) > 0 {
		sortPeers(readers)
		logger.GetLogger(ctx).Info("NOTE: no live writer peer; draining via a reader (a reader is a full replica — the peer-side drop carries the same registration authority)",
			"peer", readers[0].Hostname)
		return readers[0]
	}
	return fallback("no live claims besides self")
}

// drain performs the graceful shutdown, fastest-failure-first inside
// the drain budget (spot notice: the reclaim window; termination hook:
// 240s). The sequence is the hook Lambda's contract:
// 1. SYSTEM STOP REPLICATION QUEUES + STOP DISTRIBUTED SENDS (no
// fetch/merge/send starts mid-shutdown);
// 2. chctl replica drop on a LIVE peer (removes the table AND the
// database replica registration the local statement misses),
// falling back to the local SYSTEM DROP REPLICA;
// 3. DNS record delete + lease release (best-effort);
// 4. SYSTEM SHUTDOWN (graceful), systemctl stop as the last resort.
//
// Every step is best-effort by design: the instance is dying, and the
// weekly GC reaps whatever this pass could not reach.
func drain(ctx context.Context, st *nodeState, provider port.Provider, opts drainOptions) error {
	deadline := opts.DeadlineSeconds
	if deadline <= 0 {
		deadline = defaultDrainDeadlineSeconds
	}
	drainCtx, cancel := context.WithTimeout(ctx, time.Duration(deadline)*time.Second)
	defer cancel()
	log := logger.GetLogger(drainCtx)
	log.Info("drain: draining node",
		"hostname", st.Identity.Hostname,
		"reason", opts.Reason,
		"budget_seconds", deadline)

	// 0. Pre-drain lifecycle hooks (the operator's HostShutdown
	// moment). The drain boundary NEVER aborts on a hook failure — the
	// node is already going down; a CRITICAL hook's failure is logged
	// and the drain continues (RunHooks' drainBoundary rule).
	specHooks := drainHookSpecs()
	if len(specHooks.HooksFor("pre_drain")) > 0 || len(specHooks.HooksFor("post_drain")) > 0 {
		_ = service.RunHooks(drainCtx, service.ShellHookRunner{}, specHooks, model.HookPreDrain, drainHookIdentity(st), true)
	}

	// 1. ClickHouse-level graceful drain FIRST (parity with the
	// operator's HostShutdown hooks): stop replication queues so no
	// fetch/merge starts mid-shutdown; stop distributed sends so no
	// in-flight send is cut. Admin credentials via env (the `default`
	// user the bare client falls back to is readonly — its profile
	// refuses the drain statements).
	clientEnv := clickhouseClientEnv()
	for _, query := range []string{
		"SYSTEM STOP REPLICATION QUEUES",
		"SYSTEM STOP DISTRIBUTED SENDS",
	} {
		out, err := runCommandWithEnv(drainCtx, clientEnv, "clickhouse-client", "--query", query)
		if err != nil {
			log.Warn("WARNING: drain query failed",
				"query", query,
				"err", err,
				"out", strings.TrimSpace(string(out)))
		} else {
			log.Info("drain: query issued", "query", query)
		}
	}

	// 2. Peer-side replica drop (the operator's HostDropReplica form):
	// chctl replica drop on a LIVE peer removes both the table and the
	// DATABASE replica registration the local statement misses. If no
	// peer is reachable, the weekly GC is the safety net.
	spec, _ := loadInventorySpec()
	peer := selectDrainPeer(drainCtx, st, spec, port.AsLeaseLister(provider), provider.Name())
	if peer != nil {
		log.Info("drain: dropping replica on peer", "peer", peer.Hostname)
		err := dropOnPeer(drainCtx, st, spec, peer)
		if err != nil {
			log.Warn("NOTE: peer replica drop failed; the weekly dead-replica GC will clean up",
				"peer", peer.Hostname,
				"err", err)
			// Fallback: local drop.
			_, _ = runCommandWithEnv(drainCtx, clientEnv, "clickhouse-client", "--query",
				"SYSTEM DROP REPLICA '"+st.Identity.Hostname+"'")
		}
	}

	// 3. Release DNS + lease (best-effort — the instance is dying).
	if dns := port.AsDNSRegistrar(provider); dns != nil && st.Identity.ZoneID != "" {
		// Ownership gate: the exact-only delete is safe only when
		// the claim is still OURS. If a replacement already
		// re-claimed the ordinal (the instance-refresh overlap), the
		// replacement owns BOTH the lease row and the DNS record —
		// this drain releases neither (the lease's instance_id guard
		// already makes the release a no-op; the DNS gate is the
		// belt to the ExactOnly delete's suspenders).
		gate := service.NewReplacementGate(st.Identity.LeaseTable, port.AsLeaseLister(provider))
		replacement, replaced := gate.ReplacedBy(drainCtx, st.Identity.Hostname, st.Identity.InstanceID)
		if !replaced {
			if err := dns.DeleteARecord(drainCtx, port.ARecordRequest{
				ZoneID:     st.Identity.ZoneID,
				ZoneName:   st.dnsZone(),
				RecordName: st.Identity.Hostname,
				// IP is REQUIRED on Route53 DELETE (the exact value match
				// the API demands); empty falls back to a live-value list
				// in the provider, but the settled identity knows the IP.
				IP: st.Identity.PrivateIP,
				// ExactOnly: a failed requested-IP delete means the
				// record belongs to a live replacement now — never
				// sweep the live value set from a dying node.
				ExactOnly: true,
				TTL:       60,
			}); err != nil {
				log.Warn("WARNING: drain DNS release failed (a replacement likely re-registered the record; the expired-slot sweep owns stale cleanup)",
					"record", st.Identity.Hostname,
					"err", err)
			} else {
				log.Info("drain: DNS A record released",
					"record", st.Identity.Hostname,
					"zone_id", st.Identity.ZoneID)
			}
		} else {
			log.Warn("drain: DNS release skipped — a replacement already holds this ordinal's claim",
				"record", st.Identity.Hostname,
				"replacement", replacement)
		}
	}
	if claimer := port.AsOrdinalClaimer(provider); claimer != nil && st.Identity.LeaseTable != "" {
		if err := claimer.ReleaseLease(drainCtx, port.ReleaseRequest{
			Table:    st.Identity.LeaseTable,
			Hostname: st.Identity.Hostname,
			// Ownership guard input: the drain of the OLD instance is
			// a no-op on a claim its replacement already re-claimed
			// (the instance-refresh overlap race).
			InstanceID: st.Identity.InstanceID,
		}); err != nil {
			log.Warn("WARNING: drain lease release failed",
				"hostname", st.Identity.Hostname,
				"lease_table", st.Identity.LeaseTable,
				"err", err)
		} else {
			log.Info("drain: lease released",
				"hostname", st.Identity.Hostname,
				"lease_table", st.Identity.LeaseTable)
		}
	}

	// 4. SYSTEM SHUTDOWN: stop accepting new queries gracefully.
	// The shutdown drain skips this leg entirely (SkipServerShutdown):
	// it runs as an ExecStop inside systemd's shutdown transaction,
	// and the OS teardown that follows stops the server on its own.
	if opts.SkipServerShutdown {
		log.Info("drain: server shutdown skipped (the systemd teardown owns it)",
			"hostname", st.Identity.Hostname)
	} else if _, err := runCommandWithEnv(drainCtx, clientEnv, "clickhouse-client", "--query", "SYSTEM SHUTDOWN"); err != nil {
		log.Warn("WARNING: graceful shutdown failed; falling back to systemctl stop", "err", err)
		_, _ = runCommand(drainCtx, "systemctl", "stop", "clickhouse-server")
	} else {
		log.Info("drain: server shut down gracefully")
	}

	// 5. Post-drain lifecycle hooks (the operator's HostDelete hook):
	// the identity is released and the server is down; the hook runs
	// against the terminal state. Same drain rule — never blocking.
	_ = service.RunHooks(drainCtx, service.ShellHookRunner{}, drainHookSpecs(), model.HookPostDrain, drainHookIdentity(st), true)

	log.Info("drain: complete", "hostname", st.Identity.Hostname)
	return nil
}

// dropOnPeer removes this node's replica registration via one live
// peer (the operator's HostDropReplica form): the peer executes the
// SYSTEM DROP REPLICA that reaches both the table and the DATABASE
// registration the local statement misses. The request's inventory
// carries the peer's CR slot (writers 01-{N}; readers
// 01-{writerCeiling+N} — the claimCRSlot arithmetic) plus self, so
// the service resolves both replica addresses; the in-process port
// carries credentials in the typed request, never on an argv.
func dropOnPeer(ctx context.Context, st *nodeState, spec *inventorySpec, peer *port.Claim) error {
	peerFQDN := peer.Hostname + "." + st.dnsZone()
	hosts := []clusterops.Host{
		{Name: st.Identity.CRName, Address: st.Identity.Hostname + "." + st.dnsZone()},
	}
	if role, ordinal, ok := claimRoleOrdinal(peer.Hostname, spec.writerPrefix(), spec.readerPrefix()); ok {
		if slot := claimCRSlot(role, ordinal, spec.writerCeiling()); slot >= 0 {
			hosts = append(hosts, clusterops.Host{Name: fmt.Sprintf("01-%d", slot), Address: peerFQDN})
		}
	}
	return clusterOps.DropReplica(ctx, clusterops.DropReplicaRequest{
		Cluster:  clusterRequestWithHosts(st, hosts),
		HostName: st.Identity.Hostname,
		RunOn:    peerFQDN,
	})
}

// drainHookSpecs loads the spec's hooks section (the drain reads the
// same chinit.yaml the boot did; a missing spec answers an empty
// section — the boundaries are no-ops).
func drainHookSpecs() *model.Hooks {
	spec, err := loadSpec(inventorySpecPath())
	if err != nil || spec == nil {
		return &model.Hooks{}
	}
	return &spec.Hooks
}

// drainHookIdentity answers the hook environment for the drain.
func drainHookIdentity(st *nodeState) service.HookIdentity {
	return service.HookIdentity{
		Role:       string(st.Identity.Role),
		Hostname:   st.Identity.Hostname,
		Cluster:    st.Identity.ClusterName,
		InstanceID: st.Identity.InstanceID,
	}
}
