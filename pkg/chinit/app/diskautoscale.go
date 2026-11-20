// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

// `chinit disk-autoscale` — the writers'/readers' timer: keep
// effective disk headroom above the floor by growing the node's OWN
// data volume + filesystem online. The POLICY (floor derivation,
// grow-or-relieve) lives in the domain service
// (service.DiskAutoscaler); this file is the wiring — the on-host
// adapters for the ports and the CLI plumbing that turns the
// service's outcome into logs and metrics.

// diskAutoscaleConfig is the job's on-host binding: where the data
// disk lives. The floor and growth bounds live in the domain policy
// (service.defaultDiskFloorPolicy) so the CLI stays transport-only.
type diskAutoscaleConfig struct {
	// DataDiskDevice is the EC2 attachment device of the data volume.
	DataDiskDevice string
	// DataMountPoint is where the data disk is mounted (ext4 resize
	// target; NVMe names map from the device at runtime).
	DataMountPoint string
}

// defaultDiskAutoscaleConfig follows the compute module's
// launch-template convention (/dev/sdb → /var/lib/clickhouse).
func defaultDiskAutoscaleConfig() diskAutoscaleConfig {
	return diskAutoscaleConfig{
		DataDiskDevice: "/dev/sdb",
		DataMountPoint: "/var/lib/clickhouse",
	}
}

// runDiskAutoscale wires the ports to the on-host adapters and runs
// one tick of the domain service.
func runDiskAutoscale(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	cfg := defaultDiskAutoscaleConfig()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	provider, perr := buildProvider(providerNameOf(st), st.Identity.Region)
	if perr != nil {
		return fmt.Errorf("disk-autoscale: build provider: %w", perr)
	}
	relief := service.NewDiskRelief(&cliHotPartitionStore{queryContext: ctx}, 0)
	autoscaler := service.NewDiskAutoscaler(
		&cliDiskHeadroomProbe{ctx: ctx, mountPoint: cfg.DataMountPoint},
		&cliFilesystemResizer{ctx: ctx, mountPoint: cfg.DataMountPoint},
		port.AsVolumeGrower(provider),
		relief,
	)
	outcome := autoscaler.Run(ctx, service.DiskAutoscaleRequest{
		InstanceID: instanceIDOf(st),
		Device:     cfg.DataDiskDevice,
	})
	publishDiskAutoscaleOutcome(ctx, st, outcome)
	return outcome.Err
}

// publishDiskAutoscaleOutcome logs the tick and pushes the metric
// side (the alarm side keys on the gauge; the outcome record stands
// even when CloudWatch is the broken thing).
func publishDiskAutoscaleOutcome(ctx context.Context, st *nodeState, outcome service.DiskAutoscaleOutcome) {
	if outcome.Err != nil && outcome.FloorGB == 0 {
		// The probe never answered: the service already warned, and a
		// zero reading would false-page the alarm side — the alarm's
		// own missing-data treatment owns the gap.
		return
	}
	logger.GetLogger(ctx).Info("disk-autoscale: tick outcome",
		"unreserved_gib", outcome.UnreservedGB,
		"floor_gib", outcome.FloorGB,
		"grew", outcome.Grew,
		"relief_moved", outcome.ReliefMoved,
		"err", outcome.Err)
	if outcome.Err == nil && !outcome.Grew && outcome.ReliefMoved == 0 {
		return // idle tick: the record above is the tick's trace
	}
	pushDiskPressure(ctx, st, outcome.UnreservedGB, outcome.Grew)
}

// diskFreeQuery is the health probe: the metric writes actually gate
// on. UNRESERVED space is what the reservation book hands a new write
// — free space minus everything background operations hold (fetch
// storms, merges, part moves). Keying the grow floor on free_space
// alone is unsafe: a backfill fetch storm can hold nearly all free
// space, the server refuses new writes (NOT_ENOUGH_SPACE) while the
// job still sees headroom. keep_free_space rides the same probe so
// the FLOOR is derived from the running server's own reservation
// config — never a hardcoded number the storage XML can silently
// disagree with (the floor-must-exceed-keep_free invariant holds by
// construction).
const diskFreeQuery = "SELECT unreserved_space, keep_free_space FROM system.disks WHERE name = 'default'"

// cliDiskHeadroomProbe implements port.DiskHeadroomProbe over the
// local clickhouse-client (the reservation book) and df (the
// filesystem size). Transport only — the floor derivation is the
// domain policy's.
type cliDiskHeadroomProbe struct {
	// ctx bounds and authenticates every query.
	ctx context.Context
	// mountPoint is the data mount the filesystem size is read from.
	mountPoint string
}

// Probe implements port.DiskHeadroomProbe: the reservation-book
// probe (mandatory) and the fs-size probe (best-effort — a df
// failure degrades the floor to the fixed headroom, never disables
// growth).
func (p *cliDiskHeadroomProbe) Probe() (port.DiskHeadroom, error) {
	h, err := p.probeReservationBook()
	if err != nil {
		return port.DiskHeadroom{}, err
	}
	h.FilesystemSizeBytes, err = p.probeFilesystemSize()
	if err != nil {
		logger.GetLogger(p.ctx).Warn("disk-autoscale: filesystem size probe failed (using fixed-headroom floor)", "err", err)
	}
	return h, nil
}

// probeReservationBook reads the default disk's unreserved bytes and
// keep_free_space from the running server. Credentials go to the
// client via CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env — argv is
// world-readable in /proc/<pid>/cmdline for the process's whole
// lifetime. The bare client defaults to the `default` user, whose
// profile is readonly on this stack and whose connection is refused
// outright when a password is configured.
func (p *cliDiskHeadroomProbe) probeReservationBook() (port.DiskHeadroom, error) {
	out, err := runCommandWithEnv(p.ctx, clickhouseClientEnv(), "clickhouse-client", "--query", diskFreeQuery)
	if err != nil {
		return port.DiskHeadroom{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return port.DiskHeadroom{}, fmt.Errorf("parse disks probe %q: expected <unreserved> <keep_free>", strings.TrimSpace(string(out)))
	}
	unreserved, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return port.DiskHeadroom{}, fmt.Errorf("parse unreserved_space %q: %w", fields[0], err)
	}
	keepFree, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return port.DiskHeadroom{}, fmt.Errorf("parse keep_free_space %q: %w", fields[1], err)
	}
	return port.DiskHeadroom{UnreservedBytes: unreserved, KeepFreeBytes: keepFree}, nil
}

// probeFilesystemSize reads the data mount's filesystem size in
// bytes (df -B1 --output=size): on-host, so it reflects the LIVE
// post-grow capacity without a cloud API call on the probe path.
func (p *cliDiskHeadroomProbe) probeFilesystemSize() (uint64, error) {
	out, err := runCommand(p.ctx, "df", "-B1", "--output=size", p.mountPoint)
	if err != nil {
		return 0, fmt.Errorf("df %s: %s", p.mountPoint, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("parse df output %q: expected a size row", strings.TrimSpace(string(out)))
	}
	size, err := strconv.ParseUint(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse df size %q: %w", strings.TrimSpace(lines[len(lines)-1]), err)
	}
	return size, nil
}

// cliFilesystemResizer implements port.FilesystemResizer over
// findmnt/resize2fs: extend the ext4 filesystem to the volume's
// current capacity.
type cliFilesystemResizer struct {
	// ctx bounds every command.
	ctx context.Context
	// mountPoint is the resize target.
	mountPoint string
}

// Resize implements port.FilesystemResizer. NVMe rescan first: the
// kernel caches the block device's size, and the fresh capacity is
// invisible to resize2fs until the controller re-reads it.
func (r *cliFilesystemResizer) Resize() error {
	dev, err := resolveNVMeDevice(r.ctx, r.mountPoint)
	if err != nil {
		return err
	}
	if ctrl := nvmeControllerOf(dev); ctrl != "" {
		_, _ = runCommand(r.ctx, "sh", "-c", "echo 1 > /sys/class/nvme/"+ctrl+"/device/rescan")
	}
	if out, err := runCommand(r.ctx, "resize2fs", dev); err != nil {
		return fmt.Errorf("resize2fs %s: %s", dev, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveNVMeDevice finds the block device backing the mount point.
func resolveNVMeDevice(ctx context.Context, mountPoint string) (string, error) {
	out, err := runCommand(ctx, "findmnt", "-n", "-o", "SOURCE", mountPoint)
	if err != nil {
		return "", fmt.Errorf("findmnt %s: %s", mountPoint, strings.TrimSpace(string(out)))
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", fmt.Errorf("no block device mounted at %s", mountPoint)
	}
	return dev, nil
}

// nvmeControllerOf extracts the nvme controller name ("nvme1") from a
// block device path ("/dev/nvme1n1"); "" when not NVMe.
func nvmeControllerOf(dev string) string {
	base := strings.TrimPrefix(dev, "/dev/")
	if i := strings.Index(base, "n1"); i > 0 && strings.HasPrefix(base, "nvme") {
		return base[:i]
	}
	return ""
}

// pushDiskPressure publishes the job outcome: disk_effective_free_gb
// (the gauge the alarm side keys on) and disk_autoscaled (1 when this
// run grew the volume). The push is best-effort — the stdout/stderr
// record stands even when CloudWatch is the broken thing. The OTEL
// dual-write ships the same numbers to the OTLP endpoint (disabled =
// no-op): either pipeline being broken never masks the other.
func pushDiskPressure(ctx context.Context, st *nodeState, freeGB float64, grew bool) {
	telemetry.DiskPressure(ctx, telemetry.DiskPressureRequest{
		FreeGB: freeGB,
		Grew:   grew,
		Host:   st.Identity.Hostname,
	})
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return
	}
	pusher := port.AsMetricPusher(provider)
	if pusher == nil {
		return
	}
	dims := map[string]string{
		"ClusterName": st.clusterName(),
		"NodeType":    string(st.Identity.Role),
		"Host":        st.Identity.Hostname,
	}
	_ = pusher.PutMetric(ctx, port.MetricPushRequest{
		Namespace:  "ClickHouse/SelfHeal",
		Name:       "DiskEffectiveFreeGB",
		Value:      freeGB,
		Unit:       "Count",
		Dimensions: dims,
	})
	if grew {
		_ = pusher.PutMetric(ctx, port.MetricPushRequest{
			Namespace:  "ClickHouse/SelfHeal",
			Name:       "DiskAutoscaled",
			Value:      1,
			Unit:       "Count",
			Dimensions: dims,
		})
	}
}
