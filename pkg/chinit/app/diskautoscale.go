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

// diskAutoscaleConfig bounds the disk-autoscale job. The free-space
// floor is derived dynamically from the running server's keep_free_space
// setting plus diskFloorHeadroom.
type diskAutoscaleConfig struct {
	// GrowthFactor is the per-grow multiplier of the current volume size.
	GrowthFactor float64
	// MaxGB is the ceiling (a runaway job must never grow a volume
	// past the account's cost envelope).
	MaxGB float64
	// DataDiskDevice is the EC2 attachment device of the data volume.
	DataDiskDevice string
	// DataMountPoint is where the data disk is mounted (ext4 resize
	// target; NVMe names map from the device at runtime).
	DataMountPoint string
}

// defaultDiskAutoscaleConfig is sized for the metadata-disk role:
// headroom for one day of part-catalog churn at ingest, capped at a
// 512 GiB envelope. The mount/device defaults are the compute
// module's launch-template convention (/dev/sdb → /var/lib/clickhouse).
func defaultDiskAutoscaleConfig() diskAutoscaleConfig {
	return diskAutoscaleConfig{
		// GrowthFactor 3x: the hot tier is a SCRATCH tier — parts land
		// on insert and TTL-move to S3 after merge. Under the measured
		// 60TB/day ingest the hot tier accumulates 40+ GB in ONE day's
		// partition before the TTL mover catches up, and a +32 GiB
		// linear step lasted HOURS (the 2026-11-17 morning finding:
		// every writer sat at the 12 GiB floor retrying grows every 5
		// minutes until they hit AWS's VolumeModificationRateExceeded —
		// the next modification locked out for 19 HOURS, with the only
		// remedy the (syntax-broken) relief). Tripling the CURRENT size
		// per grow event buys multiple days of headroom per
		// modification, keeps the fleet far from the rate limit, and
		// converges faster (one 3x step vs N +32 steps each eating a
		// rate-limited modification slot). The MaxGB ceiling still
		// bounds the runaway case.
		GrowthFactor:   3,
		MaxGB:          512,
		DataDiskDevice: "/dev/sdb",
		DataMountPoint: "/var/lib/clickhouse",
	}
}

// diskFreeQuery is the health probe: the metric writes actually gate
// on. UNRESERVED space is what the reservation book hands a new write
// — free_space minus everything background operations hold (fetch
// storms, merges, part moves). Keying the grow floor on free_space
// was the dead zone: Code 243 NOT_ENOUGH_SPACE fired at
// 11 GiB free (floor 8) because unreserved was ~0 under the
// back-fill fetch storm — the node refused writes while the job saw
// headroom. keep_free_space rides the same probe so the FLOOR is
// derived from the running server's own reservation config — never
// a hardcoded number the storage XML can silently disagree with
// (the floor-must-exceed-keep_free invariant holds by construction).
const diskFreeQuery = "SELECT unreserved_space, keep_free_space FROM system.disks WHERE name = 'default'"

// diskFloorHeadroom is the margin above keep_free_space that the
// grow floor carries: once unreserved drops below keep_free + this,
// the volume grows. 2 GiB keeps a fetch storm from hitting the
// refuse-window between timer ticks.
const diskFloorHeadroom = 2 << 30

// runDiskAutoscale is `chinit disk-autoscale` (the writers'/readers'
// timer): keep effective disk headroom above the floor by growing the
// node's OWN data volume + filesystem online.
//
// Why self-healing: a metadata-disk at its floor stalls merges
// (NOT_ENOUGH_SPACE on merge output) while inserts keep landing
// parts climb until parts_to_throw_insert hard-rejects ingest. Growth is
// the one remedy that needs no human and no downtime: gp3 volumes
// resize live and ext4 grows online. The job is idempotent (a no-op
// above the floor), bounded (MaxGB), and pushes its outcome to
// CloudWatch so the alarm side sees what the job did.
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

	// The fs step runs FIRST and UNCONDITIONALLY: a previously grown
	// volume whose fs never followed (the 2026-11-17 live finding:
	// every tick errored on VolumeModificationRateExceeded or
	// IncorrectModificationState BEFORE reaching resize2fs — volumes
	// sat at 160-192 GiB while df showed 63 GiB, and the node refused
	// writes at a floor the block device had long since outgrown).
	// resize2fs on an already-maximal fs is a cheap no-op, so running
	// it every tick converges the gap the moment the volume's OPTIMIZING
	// window ends, with zero modification budget spent.
	if serr := growFilesystem(ctx, cfg); serr != nil {
		// The fs step failing is not fatal to the tick — the probe
		// below still decides whether growth is needed. But it IS
		// surfaced: a persistently failing fs step is how the gap
		// forms.
		logger.GetLogger(ctx).Warn("disk-autoscale: filesystem converge step failed (continuing)", "err", serr)
	}

	freeBytes, keepFreeBytes, err := diskEffectiveFree(ctx)
	if err != nil {
		return fmt.Errorf("disk-autoscale: effective free probe failed: %w", err)
	}
	// The floor is DERIVED from the running server's own reservation
	// config: keep_free_space + headroom. The dead zone
	// was a hardcoded floor (8 GiB) below storage.xml's
	// keep_free_space_bytes (10 GiB) — the node entered the total
	// write-refuse window (Code 243) while the job still saw
	// headroom. Deriving keeps the invariant (floor >
	// keep_free_space) true by construction, whatever the XML says.
	floorBytes := keepFreeBytes + diskFloorHeadroom
	freeGB := float64(freeBytes) / (1 << 30)
	floorGB := float64(floorBytes) / (1 << 30)
	logger.GetLogger(ctx).Info("disk-autoscale: effective disk headroom probed",
		"unreserved_gib", freeGB,
		"floor_gib", floorGB,
		"keep_free_gib", float64(keepFreeBytes)/(1<<30),
		"headroom_gib", float64(diskFloorHeadroom)/(1<<30))

	if freeBytes >= floorBytes {
		return nil
	}

	provider, perr := buildProvider(providerNameOf(st), st.Identity.Region)
	if perr != nil {
		return fmt.Errorf("disk-autoscale: build provider: %w", perr)
	}
	grower := port.AsVolumeGrower(provider)
	if grower == nil {
		// Pressure found, remedy absent: publish (the
		// alarm side keys on the metric) and fail loudly — the node
		// IS degraded, and a silent skip is how blind spots form.
		pushDiskPressure(ctx, st, freeGB, false)
		return fmt.Errorf("disk-autoscale: effective free %.2f GiB below floor but provider %s cannot grow volumes", freeGB, providerNameOf(st))
	}

	vol, verr := grower.DescribeOwnVolume(ctx, port.VolumeGrowRequest{
		InstanceID: instanceIDOf(st),
		Device:     cfg.DataDiskDevice,
	})
	if verr != nil {
		pushDiskPressure(ctx, st, freeGB, false)
		return fmt.Errorf("disk-autoscale: find data volume: %w", verr)
	}
	// 3x the CURRENT size (the bandwidth request): one grow event
	// buys days of scratch headroom instead of hours, staying far
	// from AWS's per-volume modification rate limit.
	target := int32(float64(vol.SizeGB) * cfg.GrowthFactor)
	if float64(target) > cfg.MaxGB {
		target = int32(cfg.MaxGB)
	}
	if target <= vol.SizeGB {
		// At the ceiling: growth cannot relieve the pressure — the
		// DiskRelief service moves hot partitions to the cold volume
		// instead (the writer-4 shape: 94.8% hot disk with parts
		// piling toward the insert cliff while S3 had room).
		relief := service.NewDiskRelief(&cliHotPartitionStore{queryContext: ctx}, 0)
		res, rerr := relief.Relieve(ctx)
		if rerr != nil {
			pushDiskPressure(ctx, st, freeGB, false)
			return fmt.Errorf("disk-autoscale: volume %s at the %d GiB ceiling and partition relief failed: %w", vol.VolumeID, int32(cfg.MaxGB), rerr)
		}
		if res.Moved == 0 {
			pushDiskPressure(ctx, st, freeGB, false)
			return fmt.Errorf("disk-autoscale: effective free %.2f GiB below floor, volume %s at the %d GiB ceiling, and no hot partitions left to move", freeGB, vol.VolumeID, int32(cfg.MaxGB))
		}
		logger.GetLogger(ctx).Warn("disk-autoscale: at the volume ceiling — moved hot partitions to the cold volume",
			"partitions", res.Moved)
		pushDiskPressure(ctx, st, freeGB, false)
		return nil
	}

	grew, gerr := grower.GrowVolume(ctx, vol.VolumeID, target)
	if gerr != nil {
		// The grow can fail transiently (IncorrectModificationState:
		// a previous modification still OPTIMIZING — gp3's live-resize
		// window). The relief motion runs in the gap so pressure heals
		// even when growth cannot proceed this tick.
		relief := service.NewDiskRelief(&cliHotPartitionStore{queryContext: ctx}, 0)
		res, rerr := relief.Relieve(ctx)
		if rerr == nil && res.Moved > 0 {
			logger.GetLogger(ctx).Warn("disk-autoscale: grow unavailable this tick — moved hot partitions instead (the next tick retries the grow)",
				"partitions", res.Moved, "grow_err", gerr)
			pushDiskPressure(ctx, st, freeGB, false)
			return nil
		}
		pushDiskPressure(ctx, st, freeGB, false)
		if rerr != nil {
			return fmt.Errorf("disk-autoscale: grow %s to %d: %w (partition relief: %w)", vol.VolumeID, target, gerr, rerr)
		}
		return fmt.Errorf("disk-autoscale: grow %s to %d: %w (partition relief found nothing to move)", vol.VolumeID, target, gerr)
	}
	if grew {
		if werr := grower.WaitForModification(ctx, vol.VolumeID); werr != nil {
			// The modification is in flight server-side; the fs step
			// below simply finds nothing to do and the NEXT timer run
			// completes it (the job is idempotent by design).
			logger.GetLogger(ctx).Warn("disk-autoscale: modification wait ended early (the next timer run completes it)", "err", werr)
		}
		if serr := growFilesystem(ctx, cfg); serr != nil {
			logger.GetLogger(ctx).Warn("disk-autoscale: post-grow filesystem resize failed (next tick retries)", "err", serr)
		}
	}

	logger.GetLogger(ctx).Info("disk-autoscale: volume grown and filesystem resized",
		"volume", vol.VolumeID,
		"size_gib", target)
	pushDiskPressure(ctx, st, float64(freeBytes)/(1<<30), true)
	return nil
}

// diskEffectiveFree queries the local server for the default disk's
// unreserved bytes (the reservation book's answer to a new write) and
// the keep_free_space the storage config demands. Credentials go to
// the client via CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env — argv is
// world-readable in /proc/<pid>/cmdline for the process's whole
// lifetime, env only while the reader can inspect the process. The
// bare client defaults to the `default` user, whose profile is
// readonly on this stack and whose connection is refused outright when
// a password is configured (AUTHENTICATION_FAILED — 0.2.4
// the probe ran before the password was threaded and every
// timer start failed).
func diskEffectiveFree(ctx context.Context) (unreserved, keepFree uint64, err error) {
	args := []string{"--query", diskFreeQuery}
	out, err := runCommandWithEnv(ctx, clickhouseClientEnv(), "clickhouse-client", args...)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("parse disks probe %q: expected <unreserved> <keep_free>", strings.TrimSpace(string(out)))
	}
	if unreserved, err = strconv.ParseUint(fields[0], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("parse unreserved_space %q: %w", fields[0], err)
	}
	if keepFree, err = strconv.ParseUint(fields[1], 10, 64); err != nil {
		return 0, 0, fmt.Errorf("parse keep_free_space %q: %w", fields[1], err)
	}
	return unreserved, keepFree, nil
}

// growFilesystem extends the ext4 filesystem to the grown device.
// NVMe rescan first: the kernel caches the block device's size, and
// the fresh capacity is invisible to resize2fs until the controller
// re-reads it (— /sys/block saw 64 GiB
// while resize2fs still read the stale 32 GiB fd).
func growFilesystem(ctx context.Context, cfg diskAutoscaleConfig) error {
	dev, err := resolveNVMeDevice(ctx, cfg)
	if err != nil {
		return err
	}
	// Controller rescan (best-effort): /sys/class/nvme/<ctrl>/device/rescan.
	if ctrl := nvmeControllerOf(dev); ctrl != "" {
		_, _ = runCommand(ctx, "sh", "-c", "echo 1 > /sys/class/nvme/"+ctrl+"/device/rescan")
	}
	if out, err := runCommand(ctx, "resize2fs", dev); err != nil {
		return fmt.Errorf("resize2fs %s: %s", dev, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveNVMeDevice finds the block device backing the mount point.
func resolveNVMeDevice(ctx context.Context, cfg diskAutoscaleConfig) (string, error) {
	out, err := runCommand(ctx, "findmnt", "-n", "-o", "SOURCE", cfg.DataMountPoint)
	if err != nil {
		return "", fmt.Errorf("findmnt %s: %s", cfg.DataMountPoint, strings.TrimSpace(string(out)))
	}
	dev := strings.TrimSpace(string(out))
	if dev == "" {
		return "", fmt.Errorf("no block device mounted at %s", cfg.DataMountPoint)
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
