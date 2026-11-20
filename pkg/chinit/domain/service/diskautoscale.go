// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// The disk-autoscale domain service: keeps effective disk headroom
// above a floor by growing the node's own data volume, falling back to
// partition relief when growth cannot help. POLICY lives here; the
// app layer binds the ports to on-host commands and cloud APIs.

// diskFloorPolicy derives the grow floor and the growth target from
// the measured headroom — pure decision logic over the probe's
// answer, with no I/O of its own.
type diskFloorPolicy struct {
	// fixedHeadroomBytes is the minimum margin above keep_free_space
	// the floor carries, whatever the volume's size.
	fixedHeadroomBytes uint64
	// proportionalFraction scales the headroom with the filesystem:
	// large volumes get multiple ticks of lead time before the disk
	// is critical, small volumes stay near the fixed margin.
	proportionalFraction float64
	// proportionalMaxBytes caps the scaled headroom.
	proportionalMaxBytes uint64
	// growthFactor is the per-grow multiplier of the current size.
	growthFactor float64
	// maxGB bounds growth (the account's cost envelope).
	maxGB float64
}

// defaultDiskFloorPolicy is sized for the metadata-disk role: a
// minimum margin that absorbs part-catalog churn between ticks, and
// a 3x growth factor that buys days of headroom per modification
// while the 512 GiB ceiling bounds the runaway case.
func defaultDiskFloorPolicy() diskFloorPolicy {
	return diskFloorPolicy{
		fixedHeadroomBytes:   2 << 30,
		proportionalFraction: 0.15,
		proportionalMaxBytes: 64 << 30,
		growthFactor:         3,
		maxGB:                512,
	}
}

// HeadroomBytes computes the grow floor's margin above keep_free —
// the fixed headroom OR a fraction of the filesystem, whichever is
// larger, capped. fsSize 0 (an unreadable filesystem) keeps the
// fixed margin.
func (p diskFloorPolicy) HeadroomBytes(fsSize uint64) uint64 {
	proportional := uint64(float64(fsSize) * p.proportionalFraction)
	if proportional > p.proportionalMaxBytes {
		proportional = p.proportionalMaxBytes
	}
	if proportional < p.fixedHeadroomBytes {
		return p.fixedHeadroomBytes
	}
	return proportional
}

// FloorBytes derives the grow floor from the server's own reservation
// config: keep_free_space plus the volume-aware headroom. Deriving
// (rather than hardcoding) keeps the floor > keep_free invariant true
// by construction, whatever storage.xml says.
func (p diskFloorPolicy) FloorBytes(h port.DiskHeadroom) uint64 {
	return h.KeepFreeBytes + p.HeadroomBytes(h.FilesystemSizeBytes)
}

// GrowTarget computes the growth target for a volume of currentGB:
// the current size times the growth factor, capped at MaxGB. A target
// at or below the current size means growth cannot help (the ceiling
// shape) — the caller routes to partition relief.
func (p diskFloorPolicy) GrowTarget(currentGB int32) int32 {
	target := int32(float64(currentGB) * p.growthFactor)
	if float64(target) > p.maxGB {
		return int32(p.maxGB)
	}
	return target
}

// DiskAutoscaleRequest names the node and volume the autoscaler acts
// on for one tick.
type DiskAutoscaleRequest struct {
	// InstanceID is this node's cloud instance identifier.
	InstanceID string
	// Device is the data volume's attachment device.
	Device string
}

// DiskAutoscaleOutcome is one tick's observable result, for logging
// and the metric side.
type DiskAutoscaleOutcome struct {
	// UnreservedGB and FloorGB are the probe-derived pair the alarm
	// side keys on.
	UnreservedGB, FloorGB float64
	// Grew is true when the volume grew this tick.
	Grew bool
	// ReliefMoved is the partitions moved to the cold volume when
	// relief ran.
	ReliefMoved int
	// Err is the tick's failure when pressure could not be relieved.
	// A nil Err with Grew=false and ReliefMoved=0 is a clean no-op
	// (headroom above the floor).
	Err error
}

// DiskAutoscaler is the disk-autoscale domain service: one Run() per
// timer tick. It probes headroom, checks the floor, and drives the
// growth path through the ports, degrading to partition relief when
// growth cannot help.
type DiskAutoscaler struct {
	// probe measures the disk's headroom state.
	probe port.DiskHeadroomProbe
	// resizer extends the filesystem to the volume's capacity.
	resizer port.FilesystemResizer
	// grower is the cloud's online volume-growth capability. May be
	// nil — the capability-absent shape reports pressure and fails
	// loudly rather than silently skipping.
	grower port.VolumeGrower
	// relief moves hot partitions to the cold volume when growth
	// cannot help (ceiling shape, grow errors).
	relief *DiskRelief
	// policy derives the floor and the growth target.
	policy diskFloorPolicy
}

// NewDiskAutoscaler builds the autoscaler over its ports.
func NewDiskAutoscaler(probe port.DiskHeadroomProbe, resizer port.FilesystemResizer, grower port.VolumeGrower, relief *DiskRelief) *DiskAutoscaler {
	return &DiskAutoscaler{
		probe:   probe,
		resizer: resizer,
		grower:  grower,
		relief:  relief,
		policy:  defaultDiskFloorPolicy(),
	}
}

// Run executes one autoscale tick. The filesystem resize runs FIRST
// and unconditionally (a previously grown volume whose filesystem
// never followed is converged immediately, at zero modification
// cost); its failure is logged and tolerated — the probe still
// decides whether growth is needed, and the next tick retries.
func (a *DiskAutoscaler) Run(ctx context.Context, req DiskAutoscaleRequest) DiskAutoscaleOutcome {
	a.resizeFilesystem(ctx)
	h, err := a.probe.Probe()
	if err != nil {
		return a.probeFailedOutcome(ctx, err)
	}
	floorBytes := a.policy.FloorBytes(h)
	outcome := DiskAutoscaleOutcome{
		UnreservedGB: bytesToGiB(h.UnreservedBytes),
		FloorGB:      bytesToGiB(floorBytes),
	}
	if h.UnreservedBytes >= floorBytes {
		return outcome // headroom above the floor: idle tick
	}
	return a.relievePressure(ctx, req, outcome)
}

// resizeFilesystem converges the filesystem to the volume's current
// capacity. Idempotent; failures never block the tick's decision.
func (a *DiskAutoscaler) resizeFilesystem(ctx context.Context) {
	if err := a.resizer.Resize(); err != nil {
		logger.GetLogger(ctx).Warn("disk-autoscale: filesystem converge step failed (continuing)", "err", err)
	}
}

// probeFailedOutcome handles a probe that could not measure the
// disk: the tick fails loudly — the timer's next run retries. A
// total failure is never a silent skip: the alarm side must know.
func (a *DiskAutoscaler) probeFailedOutcome(ctx context.Context, err error) DiskAutoscaleOutcome {
	logger.GetLogger(ctx).Warn("disk-autoscale: headroom probe failed", "err", err)
	return DiskAutoscaleOutcome{Err: fmt.Errorf("effective free probe failed: %w", err)}
}

// relievePressure drives the grow-or-relieve path once pressure is
// confirmed: describe the volume, plan the target, then grow or move
// hot partitions to the cold volume.
func (a *DiskAutoscaler) relievePressure(ctx context.Context, req DiskAutoscaleRequest, outcome DiskAutoscaleOutcome) DiskAutoscaleOutcome {
	if a.grower == nil {
		outcome.Err = fmt.Errorf("effective free %.2f GiB below floor but the provider cannot grow volumes", outcome.UnreservedGB)
		return outcome
	}
	vol, err := a.grower.DescribeOwnVolume(ctx, port.VolumeGrowRequest{
		InstanceID: req.InstanceID,
		Device:     req.Device,
	})
	if err != nil {
		outcome.Err = fmt.Errorf("find data volume: %w", err)
		return outcome
	}
	target := a.policy.GrowTarget(vol.SizeGB)
	if target > vol.SizeGB {
		return a.growVolume(ctx, outcome, target, vol)
	}
	// Growth cannot lift the target past the current size (the volume
	// sits at the ceiling): partition relief is the remaining remedy.
	return a.reliefAtCeiling(ctx, outcome, target, vol)
}

// reliefAtCeiling moves hot partitions to the cold volume when the
// volume sits at the growth ceiling with pressure.
func (a *DiskAutoscaler) reliefAtCeiling(ctx context.Context, outcome DiskAutoscaleOutcome, ceilingGB int32, vol *port.VolumeInfo) DiskAutoscaleOutcome {
	res, err := a.relief.Relieve(ctx)
	if err != nil {
		outcome.Err = fmt.Errorf("volume %s at the %d GiB ceiling and partition relief failed: %w", vol.VolumeID, ceilingGB, err)
		return outcome
	}
	if res.Moved == 0 {
		outcome.Err = fmt.Errorf("effective free %.2f GiB below floor, volume %s at the %d GiB ceiling, and no hot partitions left to move",
			outcome.UnreservedGB, vol.VolumeID, ceilingGB)
		return outcome
	}
	logger.GetLogger(ctx).Warn("disk-autoscale: at the volume ceiling — moved hot partitions to the cold volume", "partitions", res.Moved)
	outcome.ReliefMoved = res.Moved
	return outcome
}

// growVolume grows the volume to the target, waits for the
// modification, then resizes the filesystem. A grow failure routes
// to relief (motion in the modification gap); a wait failure is
// tolerated — the next tick completes it, idempotent by design.
func (a *DiskAutoscaler) growVolume(ctx context.Context, outcome DiskAutoscaleOutcome, targetGB int32, vol *port.VolumeInfo) DiskAutoscaleOutcome {
	grew, err := a.grower.GrowVolume(ctx, vol.VolumeID, targetGB)
	if err != nil {
		return a.growFailed(ctx, outcome, err)
	}
	if !grew {
		return outcome // no modification started; nothing to wait on
	}
	if werr := a.grower.WaitForModification(ctx, vol.VolumeID); werr != nil {
		logger.GetLogger(ctx).Warn("disk-autoscale: modification wait ended early (the next timer run completes it)", "err", werr)
	}
	a.resizeFilesystem(ctx)
	logger.GetLogger(ctx).Info("disk-autoscale: volume grown and filesystem resized",
		"volume", vol.VolumeID, "size_gib", targetGB)
	outcome.Grew = true
	return outcome
}

// growFailed handles a volume modification that could not start
// (transient IncorrectModificationState, rate limits): relief moves
// hot partitions in the gap so pressure heals even while growth
// waits; with nothing movable the tick fails loudly.
func (a *DiskAutoscaler) growFailed(ctx context.Context, outcome DiskAutoscaleOutcome, err error) DiskAutoscaleOutcome {
	res, rerr := a.relief.Relieve(ctx)
	switch {
	case rerr == nil && res.Moved > 0:
		logger.GetLogger(ctx).Warn("disk-autoscale: grow unavailable this tick — moved hot partitions instead (the next tick retries the grow)",
			"partitions", res.Moved, "grow_err", err)
		outcome.ReliefMoved = res.Moved
		return outcome
	case rerr != nil:
		outcome.Err = fmt.Errorf("grow failed (%w) and partition relief failed: %w", err, rerr)
	default:
		outcome.Err = fmt.Errorf("grow failed (%w) and partition relief found nothing to move", err)
	}
	return outcome
}

// bytesToGiB renders bytes as binary GiB for logs and metrics.
func bytesToGiB(b uint64) float64 {
	return float64(b) / (1 << 30)
}
