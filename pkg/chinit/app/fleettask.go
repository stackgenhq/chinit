// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

// fleetTaskTable resolves the task table (the claims table — one table,
// one liveness view) from the identity, falling back to the spec the
// way listLiveClaims does (keepers and the backup hold no lease).
func fleetTaskTable(st *nodeState) string {
	if st.Identity.LeaseTable != "" {
		return st.Identity.LeaseTable
	}
	if spec, err := loadInventorySpec(); err == nil && spec != nil {
		return spec.ClaimerTableName
	}
	return ""
}

// runFleetConverge is `chinit fleet-converge` (the coordinator timer on
// writers): the sweep over the fleet's task rows. The ordinal-0
// maintenance gate + vacancy election is the coordinator designation —
// the same fail-safe dead-replica-gc and autoscale use (a dead holder's
// designation is re-elected by the lowest-ordinal live writer on its
// next tick).
func runFleetConverge(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	if st.Identity.Role != model.RoleWriter {
		logger.GetLogger(context.TODO()).Info("fleet-converge: not a writer; the sweep is the ordinal-0 writer's job", "role", string(st.Identity.Role))
		return nil
	}
	// The sweep owns a bounded context from the entry: every record
	// below (the listing warning, the gate's skip, the outcome) logs
	// through it.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	table := fleetTaskTable(st)
	if table == "" {
		// No lease table = no task store rows either; the timer logs
		// and exits 0 (degradation is the designed behavior — the
		// same rule as the maintenance gate's no-claims answer).
		logger.GetLogger(ctx).Warn("fleet-converge: no lease table in identity or spec; nothing to sweep")
		return nil
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	store := port.AsFleetTaskStore(provider)
	if store == nil {
		logger.GetLogger(ctx).Warn("fleet-converge: cloud lacks the fleet-task capability", "cloud", provider.Name())
		return nil
	}
	lister := port.AsLeaseLister(provider)
	claims, cerr := listLiveClaims(ctx, st, lister)
	if cerr != nil {
		// The sweep gates on liveness like dead-replica-gc's writer
		// pass: no claims data means NO sweep this tick — re-
		// dispatching against an invisible fleet is guessing.
		logger.GetLogger(ctx).Warn("fleet-converge: live-claims listing failed; skipping this tick", "error", cerr.Error())
		return nil
	}
	if gateMaintenanceJobWithClaims(ctx, st, claims) {
		// Not the coordinator this tick (the vacancy election may
		// make another writer the holder — its timer wins).
		return nil
	}
	svc, err := service.NewFleetTaskService(store)
	if err != nil {
		return err
	}
	res, err := svc.Sweep(ctx, table)
	if err != nil {
		return fmt.Errorf("fleet-converge: %w", err)
	}
	logger.GetLogger(ctx).Info("fleet-converge: sweep complete",
		"rows", res.Rows,
		"redispatched", res.Redispatched,
		"failed", res.Failed,
		"left_pending", res.LeftPending,
		"live_executing", res.LiveExecuting)
	// The sweep tick's inventory and transitions reach the OTLP
	// endpoint beside the journal line (disabled = no-op): a fleet
	// re-dispatching stale tasks, or failing them, is exactly the
	// pattern an alert keys on.
	telemetry.FleetSweep(ctx, telemetry.FleetSweepRequest{
		Rows:          res.Rows,
		Redispatched:  res.Redispatched,
		Failed:        res.Failed,
		LeftPending:   res.LeftPending,
		LiveExecuting: res.LiveExecuting,
	})
	return nil
}

// runTaskPoll is `chinit task-poll` (the timer on every server node):
// claim this node's OWN pending task row and execute its motion. The
// orchestration is the domain executor; this shell wires the ports
// (the store from the provider, the refresh runner) and reports.
func runTaskPoll(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	switch st.Identity.Role {
	case model.RoleWriter, model.RoleReader, model.RoleBackup:
	default:
		logger.GetLogger(context.TODO()).Info("task-poll: role executes no tasks", "role", string(st.Identity.Role))
		return nil
	}
	table := fleetTaskTable(st)
	if table == "" {
		return nil
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	// Cordon: a cordoned node takes no voluntary motions (the operator
	// owns it mid-maintenance); its rows stay pending until uncordoned.
	// The lease still renews and the status still publishes — identity
	// and observability survive maintenance.
	if publisher := port.AsStatusPublisher(provider); publisher != nil {
		rows, serr := publisher.ListStatus(context.TODO(), port.NodeStatusList{Table: table, Hostname: st.Identity.Hostname})
		if serr == nil && len(rows) == 1 && rows[0].Cordoned {
			logger.GetLogger(context.TODO()).Info("task-poll: node is cordoned; skipping motions", "hostname", st.Identity.Hostname)
			return nil
		}
	}
	store := port.AsFleetTaskStore(provider)
	if store == nil {
		logger.GetLogger(context.TODO()).Warn("task-poll: cloud lacks the fleet-task capability", "cloud", provider.Name())
		return nil
	}
	executor, err := service.NewFleetTaskExecutor(store, &taskMotionRunner{provider: provider, state: st}, st.Identity.Hostname)
	if err != nil {
		return err
	}
	// The disruption budget gates the voluntary motions (refreshes,
	// rollouts): the claim waits when the fleet's healthy floors would
	// be breached. A missing status capability degrades loudly and
	// defers (never allows on no data).
	if publisher := port.AsStatusPublisher(provider); publisher != nil && table != "" {
		executor = executor.WithDisruptionGate(&statusDisruptionGate{provider: provider, table: table}, string(st.Identity.Role))
	} else {
		logger.GetLogger(context.TODO()).Warn("task-poll: no status capability; motions run without the disruption budget", "cloud", provider.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	res, err := executor.Poll(ctx, table)
	if err != nil {
		return fmt.Errorf("task-poll: %w", err)
	}
	if res.Claimed == 0 {
		return nil
	}
	logger.GetLogger(ctx).Info("task-poll: tick complete",
		"claimed", res.Claimed,
		"completed", res.Completed,
		"failed", res.Failed)
	// Same dual report as the sweep: the poll's claims and verdicts
	// ship beside the journal line (disabled = no-op).
	telemetry.FleetPoll(ctx, telemetry.FleetPollRequest{
		Claimed:   res.Claimed,
		Completed: res.Completed,
		Failed:    res.Failed,
	})
	return nil
}

// taskMotionRunner dispatches the task types to their motions: the
// refresh runs in-process (the drain-aware sequence below); the
// rollout delegates to the drain path (the node exits and the ASG's
// replacement boots the new AMI — the motion's "completion" is the
// drain's success; the task row's verdict lands on the node's next
// boot by the claim of a task the replacement... does not hold. The
// rollout row is instead closed by the COORDINATOR: it sees the
// replacement report the target version and submits the next node;
// the stale row is failed by the sweep (the dead executor's verdict
// never comes — by design).
type taskMotionRunner struct {
	provider port.Provider
	state    *nodeState
}

// RunMotion implements port.MotionRunner.
func (r *taskMotionRunner) RunMotion(ctx context.Context, task port.FleetTask) error {
	switch task.Type {
	case port.TaskRefresh:
		return (&refreshMotionRunner{provider: r.provider, state: r.state}).RunMotion(ctx, task)
	case port.TaskRollout:
		return r.runRolloutMotion(ctx, task)
	default:
		return fmt.Errorf("task type %q has no motion", task.Type)
	}
}

// runRolloutMotion is the rollout task's motion: the graceful drain
// (the shutdown-drain sequence without the replica drop — the node's
// replacement re-joins under the same ordinal and re-syncs its parts),
// then the OS shutdown. The ASG's health check replaces the instance
// with the new launch-template version (the new AMI).
//
// The motion NEVER returns: the OS shutdown is the point. The task row
// stays executing; the sweep's staleness path (the executor is gone,
// its heartbeat expires) fails the row — the coordinator reads the
// REPLACEMENT's status row (the version gate) and moves on. A drain
// failure returns the error (the verdict fails the row; the next
// coordinator tick re-evaluates).
func (r *taskMotionRunner) runRolloutMotion(ctx context.Context, task port.FleetTask) error {
	log := logger.GetLogger(ctx)
	log.Info("task-poll: rollout motion starting (drain then shutdown; the ASG replacement boots the new AMI)",
		"task", task.TaskID, "target_version", task.Payload)
	// The drain: stop serving + release identity (the shutdown-drain
	// path; the replica drop happens when the replacement's dead-peer
	// GC finds the stale registration — the replacement RE-JOINS
	// under the same ordinal first, so the drop is the rare path, not
	// the expected one).
	if err := drain(ctx, r.state, r.provider, drainOptions{
		DeadlineSeconds: 240,
		Reason:          drainReasonRollout,
	}); err != nil {
		return fmt.Errorf("rollout motion drain: %w", err)
	}
	// The shutdown: systemctl poweroff is the last line — a return
	// here is a failure to reach the shutdown, never a "completed"
	// motion.
	if out, err := runCommand(ctx, "systemctl", "poweroff"); err != nil {
		return fmt.Errorf("rollout motion: shutdown failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Unreachable in practice; the honest answer if poweroff returns.
	return fmt.Errorf("rollout motion: the node survived poweroff (manual shutdown required)")
}

// refreshMotionRunner is the refresh task's motion: the drain-aware
// refresh — the operator's exclude→drain→restart→catch-up→include
// sequence, in the node-local form. The disruption budget has already
// gated the claim (one voluntary disruption at a time fleet-wide);
// this motion owns the node's own sequence.
type refreshMotionRunner struct {
	// provider and state are wired by task-poll (the runner needs the
	// DNS registrar for the exclude/include halves; the refresh itself
	// shells the same binary).
	provider port.Provider
	state    *nodeState
}

// RunMotion implements port.MotionRunner. The sequence:
//
//  1. withdraw the DNS A-record (the exclude: fan-outs stop resolving
//     the node BEFORE it stops serving — the operator's
//     excludeHostFromService equivalent);
//  2. stop serving: SYSTEM STOP REPLICATION QUEUES + STOP DISTRIBUTED
//     SENDS (no fetch/merge/send starts mid-restart — the operator's
//     completeQueries drain equivalent);
//  3. the refresh itself (chinit boot --refresh: re-render, restart);
//  4. resume: SYSTEM START REPLICATION QUEUES + START DISTRIBUTED
//     SENDS, then the DNS A-record upsert on EVERY exit path (the
//     include — fan-outs resume resolving the node only after it
//     serves again; a failed motion must never leave the node
//     unresolvable — the lease-renew converger is the backstop, but
//     the motion heals its own residue).
//
// Idempotency (a re-dispatch after a mid-motion crash re-runs
// cleanly): every step is convergent — the DNS delete/upsert, the
// STOP/START statements, and the refresh are all no-ops in their end
// states.
func (r *refreshMotionRunner) RunMotion(ctx context.Context, task port.FleetTask) error {
	log := logger.GetLogger(ctx)
	host := r.state.Identity.Hostname
	log.Info("task-poll: refresh motion starting (drain-aware)",
		"task", task.TaskID, "hostname", host)

	dns := port.AsDNSRegistrar(r.provider)
	if dns != nil && r.state.Identity.ZoneID != "" {
		// 1. Withdraw DNS (the exclude). A failed withdrawal fails the
		// motion: restarting while the record stays live is the exact
		// residue the converger heals — but a motion that cannot
		// withdraw signals a DNS outage that makes the sequence
		// unsafe (the restart would strand fan-outs mid-resolution).
		// ExactOnly: this node still holds its lease (the motion runs
		// under it), so the record is ours — but a failed exact delete
		// must not sweep a replacement's value either (the same race
		// the drain guards against).
		if err := dns.DeleteARecord(ctx, port.ARecordRequest{
			ZoneID:     r.state.Identity.ZoneID,
			ZoneName:   r.state.dnsZone(),
			RecordName: host,
			IP:         r.state.Identity.PrivateIP,
			ExactOnly:  true,
			TTL:        60,
		}); err != nil {
			return fmt.Errorf("refresh motion: DNS withdraw failed (aborting before the restart): %w", err)
		}
		defer func() {
			// 4b. Re-advertise on EVERY exit path.
			rctx, rcancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer rcancel()
			if err := dns.UpsertARecord(rctx, port.ARecordRequest{
				ZoneID:     r.state.Identity.ZoneID,
				ZoneName:   r.state.dnsZone(),
				RecordName: host,
				IP:         r.state.Identity.PrivateIP,
				TTL:        60,
			}); err != nil {
				logger.GetLogger(rctx).Warn("refresh motion: DNS re-advertise failed (the lease-renew converger will heal)",
					"record", host, "err", err)
			}
		}()
	}

	// 2. Stop serving (the drain). Best-effort: a failed statement is
	// logged, never fatal — the restart's own lifecycle is the
	// backstop, and the START pair below restores serving either way.
	clientEnv := clickhouseClientEnv()
	for _, query := range []string{
		"SYSTEM STOP REPLICATION QUEUES",
		"SYSTEM STOP DISTRIBUTED SENDS",
	} {
		out, err := runCommandWithEnv(ctx, clientEnv, "clickhouse-client", "--query", query)
		if err != nil {
			log.Warn("refresh motion: stop-serving statement failed (continuing)",
				"query", query, "err", err, "out", strings.TrimSpace(string(out)))
		}
	}

	// 3. The refresh itself.
	if err := r.runRefreshSubprocess(ctx, task); err != nil {
		return err
	}

	// 4. Resume serving (the include): the queues and sends first; the
	// record's re-advertise rides the deferred upsert.
	for _, query := range []string{
		"SYSTEM START REPLICATION QUEUES",
		"SYSTEM START DISTRIBUTED SENDS",
	} {
		out, err := runCommandWithEnv(ctx, clientEnv, "clickhouse-client", "--query", query)
		if err != nil {
			log.Warn("refresh motion: resume-serving statement failed (the queues resume on restart)",
				"query", query, "err", err, "out", strings.TrimSpace(string(out)))
		}
	}
	log.Info("task-poll: refresh motion complete", "task", task.TaskID, "hostname", host)
	return nil
}

// runRefreshSubprocess shells the node's own chinit refresh — the same
// binary, one motion per claim, idempotent by the refresh's own
// convergence. Split out so the sequence's steps stay readable.
//
// The binary path is a package-level seam: production resolves
// os.Executable (the chinit binary invoking itself); the motion specs
// inject a stub so the subprocess never runs under `go test`.
var chinitBinaryPath = func() (string, error) {
	return os.Executable()
}

// execCommandContext is the subprocess seam (the same var pattern as
// runCommand: production builds the real exec.Cmd; tests record).
var execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func (r *refreshMotionRunner) runRefreshSubprocess(ctx context.Context, task port.FleetTask) error {
	if task.Payload != "" {
		// The bundle SHA the refresh should converge onto: the payload
		// is informational for the refresh motion (the spec's own
		// config_bundle.sha256 gate is the authority — a payload
		// mismatch means the task was submitted against an older
		// bundle; the refresh converges to the CURRENT spec anyway
		// and the verdict's completion is true).
		logger.GetLogger(ctx).Info("task-poll: refresh task payload", "bundle_sha", task.Payload)
	}
	bin, err := chinitBinaryPath()
	if err != nil {
		return fmt.Errorf("resolve chinit binary: %w", err)
	}
	cmd := execCommandContext(ctx, bin, "boot", "-refresh")
	// The refresh reads the node's spec at its default path; the
	// motion adds no flags beyond -refresh (identity restore and the
	// bundle gate are the refresh's own contract).
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("refresh motion: %w: %s", err, truncateForLog(string(out), 2000))
	}
	return nil
}

// truncateForLog bounds a motion's captured output (the journal line
// must not carry a whole build log).
func truncateForLog(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// runFleetRollout is `chinit fleet-rollout` (the coordinator timer or
// an operator invocation): the AMI-rollout coordinator. Terraform
// bumps the launch template (out of chinit's scope); this coordinator
// evaluates the fleet's rollout position from the status rows and
// submits the NEXT node's rollout task — one outstanding task at a
// time, the lowest ordinal first, cordoned nodes skipped.
func runFleetRollout(args []string) error {
	fs := flag.NewFlagSet("chinit fleet-rollout", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	targetVersion := fs.String("target-version", "", "the ClickHouse version the new AMI carries (the version gate's authority)")
	role := fs.String("role", "writer", "the role this rollout governs (writer|reader)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *targetVersion == "" {
		return fmt.Errorf("fleet-rollout requires -target-version (the new AMI's ClickHouse version)")
	}
	if *role == "keeper" {
		return runKeeperRollout(*statePath, *targetVersion)
	}
	st, err := loadNodeState(*statePath)
	if err != nil {
		return err
	}
	table := fleetTaskTable(st)
	if table == "" {
		return fmt.Errorf("fleet-rollout: no lease table in identity or spec")
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	store := port.AsFleetTaskStore(provider)
	publisher := port.AsStatusPublisher(provider)
	if store == nil || publisher == nil {
		return fmt.Errorf("fleet-rollout: cloud %s lacks the task or status capability", provider.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	statuses, err := publisher.ListStatus(ctx, port.NodeStatusList{Table: table})
	if err != nil {
		return fmt.Errorf("fleet-rollout: %w", err)
	}
	tasks, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
	if err != nil {
		return fmt.Errorf("fleet-rollout: %w", err)
	}
	state := service.EvaluateRollout(service.RolloutInput{
		TargetVersion: *targetVersion,
		Statuses:      statuses,
		Tasks:         tasks,
		Role:          *role,
	})
	if state.Complete {
		logger.GetLogger(ctx).Info("fleet-rollout: complete — every non-cordoned node reports the target",
			"target", *targetVersion, "role", *role)
		fmt.Printf("fleet-rollout: complete (%s on every non-cordoned %s)\n", *targetVersion, *role)
		return nil
	}
	next := service.NextRolloutDispatch(state)
	if next == "" {
		// Either a task is outstanding (the previous node's verdict
		// gates the next) or every off-target node is cordoned.
		logger.GetLogger(ctx).Info("fleet-rollout: an outstanding task gates the next dispatch, or every off-target node is cordoned",
			"role", *role)
		fmt.Println("fleet-rollout: a rollout task is outstanding (the previous node's verdict gates the next dispatch)")
		return nil
	}
	// The disruption budget gates the submission (one voluntary
	// disruption at a time fleet-wide).
	gate := &statusDisruptionGate{provider: provider, table: table}
	verdict, err := gate.CheckDisruption(ctx, port.DisruptionCheckRequest{Role: *role})
	if err != nil {
		return fmt.Errorf("fleet-rollout: budget gate: %w", err)
	}
	if !verdict.Allowed {
		logger.GetLogger(ctx).Warn("fleet-rollout: budget defers the next dispatch", "reason", verdict.Reason)
		fmt.Printf("fleet-rollout: deferred (%s)\n", verdict.Reason)
		return nil
	}
	taskID := fmt.Sprintf("rollout-%s-%d", next, time.Now().Unix())
	submitted, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
		Table:      table,
		TaskID:     taskID,
		Type:       port.TaskRollout,
		Target:     next,
		Payload:    *targetVersion,
		MaxRetries: 1,
	})
	if err != nil {
		return fmt.Errorf("fleet-rollout: %w", err)
	}
	if !submitted {
		logger.GetLogger(ctx).Warn("fleet-rollout: submit refused (a live task row already exists)", "target", next)
		return nil
	}
	logger.GetLogger(ctx).Info("fleet-rollout: dispatched the next node's rollout task",
		"target", next, "task", taskID, "target_version", *targetVersion)
	fmt.Printf("fleet-rollout: dispatched %s (task %s, target %s)\n", next, taskID, *targetVersion)
	return nil
}

// runExporter is `chinit exporter [-port 9150] [-state FILE]`: the
// chinit-owned Prometheus endpoint. Serves the fleet-layer metrics
// (task phases, health/cordon counters, status staleness) on /metrics
// and proxies ClickHouse's own :9363 exposition on /clickhouse-metrics
// — one scrape target for both, the operator's metrics-exporter
// parity at the VM level.
func runExporter(args []string) error {
	fs := flag.NewFlagSet("chinit exporter", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	listenPort := fs.Int("port", 9150, "the exporter's listen port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadNodeState(*statePath)
	if err != nil {
		return err
	}
	table := fleetTaskTable(st)
	if table == "" {
		return fmt.Errorf("exporter: no lease table in identity or spec")
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	store := port.AsFleetTaskStore(provider)
	publisher := port.AsStatusPublisher(provider)
	if store == nil || publisher == nil {
		return fmt.Errorf("exporter: cloud %s lacks the task or status capability", provider.Name())
	}

	mux := http.NewServeMux()
	// /metrics: the fleet layer's own exposition.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		tasks, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
		if err != nil {
			http.Error(w, "task list failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		statuses, err := publisher.ListStatus(ctx, port.NodeStatusList{Table: table})
		if err != nil {
			http.Error(w, "status list failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if _, err := io.WriteString(w, service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname:    st.Identity.Hostname,
			Role:        string(st.Identity.Role),
			ClusterName: st.clusterName(),
			Tasks:       tasks,
			Statuses:    statuses,
			CAS:         probeCASMetrics(ctx),
			Now:         time.Now(),
		})); err != nil {
			// The client went away mid-scrape; nothing to act on.
			_ = err
		}
	})
	// /clickhouse-metrics: the pass-through of ClickHouse's own
	// exposition (the scrape target consolidation — one port serves
	// both layers).
	mux.HandleFunc("/clickhouse-metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:9363/metrics", nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "clickhouse metrics unreachable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = io.Copy(w, resp.Body)
	})

	addr := fmt.Sprintf(":%d", *listenPort)
	logger.GetLogger(context.TODO()).Info("exporter: serving fleet metrics",
		"addr", addr, "cluster", st.clusterName())
	fmt.Printf("chinit exporter listening on %s (/metrics, /clickhouse-metrics)\n", addr)
	return http.ListenAndServe(addr, mux)
}

// probeCASMetrics queries system.cas_mounts and system.cas_gc_log on the
// local server when the Antalya CAS tables exist, returning zeroed
// CASMetrics (Enabled=false) on upstream builds or when CAS is unused.
var probeCASMetrics = defaultProbeCASMetrics

func defaultProbeCASMetrics(ctx context.Context) service.CASMetrics {
	const casQuery = `
SELECT
    countIf(state = 'live'),
    toInt64(sum(wedged_namespace_count)),
    (SELECT count() FROM system.cas_gc_log WHERE event_time >= now() - INTERVAL 1 HOUR AND error != '')
FROM system.cas_mounts
`
	out, err := runCommandWithEnv(ctx, clickhouseClientEnv(), "clickhouse-client", "--query", casQuery)
	if err != nil {
		return service.CASMetrics{}
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 3 {
		return service.CASMetrics{}
	}
	live, err1 := strconv.Atoi(fields[0])
	wedged, err2 := strconv.Atoi(fields[1])
	gcErrs, err3 := strconv.Atoi(fields[2])
	if err1 != nil || err2 != nil || err3 != nil || (live == 0 && wedged == 0 && gcErrs == 0) {
		return service.CASMetrics{}
	}
	return service.CASMetrics{
		Enabled:          true,
		LiveMounts:       live,
		WedgedNamespaces: wedged,
		GCErrors:         gcErrs,
	}
}

// runKeeperRollout is the keeper mode of fleet-rollout: the keeper
// coordinator (keeper-0 — the same gate autoscale's keeper path uses)
// probes the ENSEMBLE via mntr, gates on the raft-safety rule (an
// unhealthy ensemble never rolls; a <3-voting-member ensemble never
// rolls), and submits the next keeper's rollout task — one at a time.
// The keeper task's version gate is the mntr zk_version the
// replacement reports (keepers publish no status row; the ensemble
// probe IS the keeper's telemetry).
func runKeeperRollout(statePath, targetVersion string) error {
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	if st.Identity.Hostname != keeperOrdinalZero() {
		logger.GetLogger(context.TODO()).Info("fleet-rollout: the keeper coordinator is the ordinal-0 keeper",
			"coordinator", keeperOrdinalZero(), "this_node", st.Identity.Hostname)
		return nil
	}
	table := fleetTaskTable(st)
	if table == "" {
		return fmt.Errorf("fleet-rollout: no lease table in identity or spec")
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	store := port.AsFleetTaskStore(provider)
	if store == nil {
		return fmt.Errorf("fleet-rollout: cloud %s lacks the task capability", provider.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The ensemble probe: the coordinator's own mntr answers the
	// quorum facts (the operator's raft-safety input), parsed by the
	// domain mntr service — no string wrangling in the CLI.
	ens, err := clusterOps.KeeperMntrTyped(ctx, clusterops.MntrRequest{
		Address: "localhost:9181",
		Timeout: 10 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("fleet-rollout (keeper): ensemble probe failed — refusing to roll: %w", err)
	}
	votingMembers := ens.VotingMembers
	if votingMembers == 0 {
		votingMembers = ens.SyncedFollowers + followersOf(ens) + 1
	}
	leaderPresent := ens.ServerState == "leader" || followersOf(ens) > 0
	if err := service.KeeperRolloutGate(votingMembers, ens.SyncedFollowers, leaderPresent); err != nil {
		logger.GetLogger(ctx).Warn("fleet-rollout (keeper): gated", "reason", err.Error())
		fmt.Printf("fleet-rollout (keeper): %s\n", err.Error())
		return nil
	}

	// The keeper inventory from the spec: the off-target keepers in
	// ordinal order, one outstanding task at a time (the writer path's
	// rule — the previous keeper's replacement must be re-synced
	// before the next).
	spec, _ := loadInventorySpec()
	var keepers []string
	if spec != nil {
		keepers = spec.KeeperNodes
	}
	tasks, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
	if err != nil {
		return fmt.Errorf("fleet-rollout (keeper): %w", err)
	}
	for _, t := range tasks {
		if t.Type == port.TaskRollout && !port.FleetTaskTerminal(t) {
			fmt.Println("fleet-rollout (keeper): a rollout task is outstanding (the previous keeper's verdict gates the next)")
			return nil
		}
	}
	// Keepers report their version through mntr's zk_version; the
	// off-target set is probed per keeper (the coordinator reaches each
	// keeper's 4lw endpoint over the private network).
	ordinalZero := keeperOrdinalZero()
	for _, k := range keepers {
		if k == ordinalZero {
			continue // the coordinator rolls LAST (it is the probe's own node)
		}
		kRes, kerr := clusterOps.KeeperMntrTyped(ctx, clusterops.MntrRequest{
			Address: k + ":9181",
			Timeout: 5 * time.Second,
		})
		if kerr != nil {
			logger.GetLogger(ctx).Warn("fleet-rollout (keeper): keeper probe failed (deferring)",
				"keeper", k, "err", kerr.Error())
			continue
		}
		kVersion := kRes.Fields["zk_version"]
		if kVersion == targetVersion {
			continue
		}
		taskID := fmt.Sprintf("rollout-%s-%d", k, time.Now().Unix())
		submitted, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
			Table:      table,
			TaskID:     taskID,
			Type:       port.TaskRollout,
			Target:     k,
			Payload:    targetVersion,
			MaxRetries: 1,
		})
		if err != nil {
			return fmt.Errorf("fleet-rollout (keeper): %w", err)
		}
		if !submitted {
			continue
		}
		logger.GetLogger(ctx).Info("fleet-rollout (keeper): dispatched the next keeper's rollout task",
			"target", k, "task", taskID, "zk_version", kVersion)
		fmt.Printf("fleet-rollout (keeper): dispatched %s (task %s, target %s)\n", k, taskID, targetVersion)
		return nil
	}
	fmt.Println("fleet-rollout (keeper): every keeper reports the target version (or the coordinator is last)")
	return nil
}

// followersOf reads zk_followers from the verbatim mntr payload (the
// typed result carries the quorum fields it uses; the follower count
// rides the fields map).
func followersOf(r service.MntrResult) int {
	n, _ := strconv.Atoi(r.Fields["zk_followers"])
	return n
}
