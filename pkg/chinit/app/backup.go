// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

// runBackupDaily executes `chinit backup-daily` on the backup replica
// (clickhouse-backup create/upload/delete-local).
func runBackupDaily(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Hour)
	defer cancel()

	spec, _ := loadInventorySpec()
	onDemand := spec.backupOnDemand()
	if onDemand {
		// Always stop the on-demand backup replica when the pass exits
		// (even on error after publishing BackupSuccess=0) so a failed
		// backup never leaks 24/7 EC2 spend.
		defer stopSelfOnDemand(ctx, st)
		preBackupSync(ctx, st, spec)
	}

	name := "daily-" + time.Now().UTC().Format("20060102-150405")
	for _, argv := range [][]string{
		{"create", name},
		{"upload", name},
		{"delete", "local", name},
	} {
		out, err := runBackupTool(ctx, argv...)
		if err != nil {
			pushBackupOutcome(ctx, st, false)
			return fmt.Errorf("clickhouse-backup %s: %w: %s", strings.Join(argv, " "), err, out)
		}
	}
	pushBackupOutcome(ctx, st, true)
	return nil
}

// preBackupSync converges peer discovery, DDL, stuck replication
// queues, and replica part sync before an on-demand backup replica
// snapshots its catalog (while stopped, writer ordinals may have
// rotated IPs and new parts accumulated). Best-effort: warnings log
// without aborting the backup attempt itself.
var preBackupSync = func(ctx context.Context, st *nodeState, spec *inventorySpec) {
	log := logger.GetLogger(ctx)
	_ = convergeStaticNodePeers(st)
	if _, err := service.ApplyBootstrapDDL(ctx, st.Identity.Hostname, string(st.Identity.Role)); err != nil {
		log.Warn("backup-daily: pre-backup DDL convergence warning", "err", err)
	}
	if _, err := clusterOps.QueueHeal(ctx, clusterops.QueueHealRequest{
		Cluster:        clusterRequest(st),
		RunOn:          st.Identity.Hostname,
		BusyMergesSkip: false,
		ForceRestore:   newForceRestoreFlagger(ctx, st),
	}); err != nil {
		log.Warn("backup-daily: pre-backup queue-heal warning", "err", err)
	}
	if err := clusterOps.SyncReplica(ctx, clusterops.SyncReplicaRequest{
		Cluster:  clusterRequest(st),
		HostName: st.Identity.Hostname,
	}); err != nil {
		log.Warn("backup-daily: pre-backup replica-sync warning", "err", err)
	}
}

// stopSelfOnDemand stops this backup replica's EC2 instance once the
// on-demand backup pass finishes.
func stopSelfOnDemand(ctx context.Context, st *nodeState) {
	log := logger.GetLogger(ctx)
	if st == nil || st.Identity.InstanceID == "" {
		return
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		log.Warn("backup-daily: build provider for on-demand self-stop failed", "err", err)
		return
	}
	lifecycle := port.AsInstanceLifecycle(provider)
	if lifecycle == nil {
		log.Warn("backup-daily: provider lacks instance-lifecycle capability; skipping self-stop")
		return
	}
	if err := lifecycle.StopInstance(ctx, st.Identity.InstanceID); err != nil {
		log.Warn("backup-daily: on-demand self-stop failed", "instance_id", st.Identity.InstanceID, "err", err)
		return
	}
	log.Info("backup-daily: on-demand backup complete; initiated instance stop", "instance_id", st.Identity.InstanceID)
}

// runBackupWake is `chinit backup-wake` (the schedule timer on writers;
// gated on the ordinal-0 maintenance holder with vacancy election):
// starts the stopped backup-0 EC2 instance so it boots, syncs its
// replica catalog, runs clickhouse-backup, and stops itself.
func runBackupWake(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	spec, err := loadInventorySpec()
	if err != nil || spec == nil {
		return fmt.Errorf("backup-wake: load spec: %w", err)
	}
	if !spec.backupOnDemand() || spec.backupReplicaHostname() == "" {
		logger.GetLogger(ctx).Info("backup-wake: on-demand backup replica not enabled in spec; skipping")
		return nil
	}

	claims, cerr := listLiveClaims(ctx, st, gcLivelinessFrom(st))
	if cerr != nil {
		logger.GetLogger(ctx).Warn("backup-wake: live-claims listing failed; treating as no claims", "error", cerr.Error())
		claims = nil
	}
	if gateMaintenanceJobWithClaims(ctx, st, claims) {
		return nil
	}

	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return fmt.Errorf("backup-wake: build provider: %w", err)
	}
	lifecycle := port.AsInstanceLifecycle(provider)
	if lifecycle == nil {
		return fmt.Errorf("backup-wake: provider %s lacks instance-lifecycle capability", providerNameOf(st))
	}

	nameTag := fmt.Sprintf("%s-%s", st.clusterName(), spec.backupReplicaHostname())
	inst, err := lifecycle.FindInstanceByName(ctx, nameTag)
	if err != nil {
		return fmt.Errorf("backup-wake: %w", err)
	}
	if inst.State == "running" || inst.State == "pending" {
		logger.GetLogger(ctx).Info("backup-wake: backup replica already up",
			"name", nameTag, "instance_id", inst.InstanceID, "state", inst.State)
		return nil
	}
	if err := lifecycle.StartInstance(ctx, inst.InstanceID); err != nil {
		return fmt.Errorf("backup-wake: start %s (%s): %w", nameTag, inst.InstanceID, err)
	}
	logger.GetLogger(ctx).Info("backup-wake: started on-demand backup replica",
		"name", nameTag, "instance_id", inst.InstanceID)
	return nil
}

// pushBackupOutcome publishes the backup pass's verdict to the
// SelfHeal namespace — the alarm side's ONLY liveness signal for the
// backup path (the 2026-11-17 soak finding: the backup-missing alarm
// keyed on the S3 bucket's object count, which never detects a STALLED
// nightly backup once the bucket holds old objects; the fleet was
// blind to the nightly timer failing). A 1 lands on every clean
// pass; a 0 lands on failure; the alarm alarms on absence.
func pushBackupOutcome(ctx context.Context, st *nodeState, success bool) {
	telemetry.BackupOutcome(ctx, telemetry.BackupOutcomeRequest{
		Success: success,
		Host:    st.Identity.Hostname,
	})
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return
	}
	pusher := port.AsMetricPusher(provider)
	if pusher == nil {
		return
	}
	value := 0.0
	if success {
		value = 1.0
	}
	_ = pusher.PutMetric(ctx, port.MetricPushRequest{
		Namespace: "ClickHouse/SelfHeal",
		Name:      "BackupSuccess",
		Value:     value,
		Unit:      "Count",
		Dimensions: map[string]string{
			"ClusterName": st.clusterName(),
			"Host":        st.Identity.Hostname,
		},
	})
}

// runBackupTool shells clickhouse-backup (kept as a var seam for tests).
var runBackupTool = func(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "clickhouse-backup", args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
