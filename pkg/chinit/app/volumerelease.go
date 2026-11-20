// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// volumeReleaseBudget bounds the shutdown release: 80s, under the
// unit's 90s TimeoutStopSec and far under EC2's 180s shutdown
// window — the release must never be the reason a termination times
// out and the instance is killed mid-delete.
const volumeReleaseBudget = 80 * time.Second

// runVolumeRelease is `chinit volume-release` — the in-node shutdown
// path the chinit-volume-release.service unit's ExecStop invokes
// (ordered Before=clickhouse-server, so the server has ALREADY stopped
// when this runs; the drain-shutdown unit — ordered the other way —
// released the lease/DNS/replica against the still-live server first).
//
// The entry is a thin CLI shell (flags, guards, exit semantics); the
// release policy lives in the coordinator below. Every release is
// best-effort: a failing volume logs and the loop continues — the
// daily orphan sweep (untagged+unattached census) is the safety net,
// and one stuck volume must never hold the shutdown.
func runVolumeRelease(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	if skip := volumeReleaseGate(statePath); skip {
		return nil
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		// Plain error (exit 1): a node that never settled identity
		// cannot prove which volumes are its own — the launch
		// template's delete_on_termination flags plus the daily
		// orphan sweep are the safety nets.
		logger.GetLogger(context.TODO()).Warn("volume-release: cannot load node state; the termination flags and the orphan sweep own cleanup",
			"state_path", statePath,
			"err", err)
		return fmt.Errorf("volume-release: cannot load node state from %s: %w", statePath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), volumeReleaseBudget)
	defer cancel()
	return (&volumeReleaseCoordinator{state: st}).run(ctx)
}

// volumeReleaseGate answers whether this invocation should stop
// before any state is loaded. A reboot never releases volumes — they
// survive it by definition — and the check is best-effort BY DESIGN
// (the drain's guard, same seam): a systemd IPC failure must not
// become a skipped release on a real termination.
func volumeReleaseGate(statePath string) bool {
	log := logger.GetLogger(context.TODO())
	rebooting, err := shutdownIsReboot()
	if err != nil {
		log.Warn("volume-release: reboot check failed; assuming termination", "err", err)
		return false
	}
	if rebooting {
		log.Info("volume-release: reboot detected — skipping (volumes survive a reboot)",
			"state_path", statePath)
		return true
	}
	return false
}

// volumeReleaseCoordinator owns the release pass: identity, provider,
// discovery, and the per-volume loop. Small methods, early returns —
// each stage is readable on its own and the run sequence is the
// method bodies in order.
type volumeReleaseCoordinator struct {
	state *nodeState
}

// run performs the release: gate by role, resolve the capability,
// resolve the release list (live discovery, state fallback), release
// each volume best-effort.
func (c *volumeReleaseCoordinator) run(ctx context.Context) error {
	if !c.state.Identity.Role.HasStatelessVolumes() {
		// The role gate: keepers and the backup replica re-attach
		// standalone persistent volumes across replacements —
		// releasing them would be data loss by design.
		logger.GetLogger(ctx).Info("volume-release: role carries persistent volumes by design; skipping",
			"role", c.state.Identity.Role)
		return nil
	}
	releaser, ok := c.releaser(ctx)
	if !ok {
		return nil // degraded loudly inside; the orphan sweep owns cleanup
	}
	vols := c.releaseList(ctx, releaser)
	if len(vols) == 0 {
		logger.GetLogger(ctx).Info("volume-release: no volumes to release")
		return nil
	}
	c.releaseAll(ctx, releaser, vols)
	return nil
}

// releaser resolves the volume-release capability, degrading loudly
// when the provider cannot be built or lacks it (the orphan sweep
// remains the reclaim either way).
func (c *volumeReleaseCoordinator) releaser(ctx context.Context) (port.VolumeReleaser, bool) {
	provider, err := buildProvider(providerNameOf(c.state), c.state.Identity.Region)
	if err != nil {
		logger.GetLogger(ctx).Warn("volume-release: provider build failed; the orphan sweep owns cleanup",
			"err", err.Error())
		return nil, false
	}
	releaser := port.AsVolumeReleaser(provider)
	if releaser == nil {
		logger.GetLogger(ctx).Warn("volume-release: provider has no volume-release capability; the orphan sweep owns cleanup",
			"cloud", provider.Name())
		return nil, false
	}
	return releaser, true
}

// releaseList resolves the volumes this node should release: live
// discovery filtered to non-root devices — the authoritative list
// (a stale state file could name a replacement's volume; live
// discovery names only THIS instance's) — falling back to the
// state's tracked IDs when the cloud API is unreachable mid-teardown.
func (c *volumeReleaseCoordinator) releaseList(ctx context.Context, releaser port.VolumeReleaser) []port.AttachedVolume {
	live, err := releaser.ListOwnVolumes(ctx, port.VolumeReleaseRequest{
		InstanceID: instanceIDOf(c.state),
	})
	if err != nil {
		logger.GetLogger(ctx).Warn("volume-release: live discovery failed; falling back to the state's tracked volumes",
			"err", err.Error())
		return c.trackedVolumes()
	}
	var out []port.AttachedVolume
	for _, v := range live {
		if !isRootDevice(v.Device) {
			out = append(out, v)
		}
	}
	return out
}

// trackedVolumes synthesizes the fallback list from the boot's
// recording: every tracked volume is a release candidate by
// construction (the boot tracks only volumes attached to this
// instance), so only the ID carries over — the device/size context
// is a live-discovery luxury the shutdown path does not need.
func (c *volumeReleaseCoordinator) trackedVolumes() []port.AttachedVolume {
	out := make([]port.AttachedVolume, 0, len(c.state.TrackedVolumes))
	for _, id := range c.state.TrackedVolumes {
		out = append(out, port.AttachedVolume{VolumeID: id})
	}
	return out
}

// releaseAll releases each volume best-effort: a failure logs and
// the loop continues — one stuck volume must never hold the
// shutdown, and the orphan sweep reports what this pass could not
// reach.
func (c *volumeReleaseCoordinator) releaseAll(ctx context.Context, releaser port.VolumeReleaser, vols []port.AttachedVolume) {
	log := logger.GetLogger(ctx)
	log.Info("volume-release: releasing own volumes",
		"hostname", c.state.Identity.Hostname,
		"count", len(vols))
	released, failed := 0, 0
	for _, vol := range vols {
		if err := releaser.ReleaseVolume(ctx, vol.VolumeID); err != nil {
			log.Warn("volume-release: release failed (the orphan sweep will report it)",
				"volume", vol.VolumeID, "device", vol.Device, "err", err.Error())
			failed++
			continue
		}
		log.Info("volume-release: volume released",
			"volume", vol.VolumeID, "device", vol.Device, "size_gb", vol.SizeGB)
		released++
	}
	log.Info("volume-release: pass complete",
		"released", released, "failed", failed)
}

// isRootDevice reports whether the attachment device is the OS root
// (the launch templates pin root at /dev/sda1; AWS deletes it by
// flag and the OS cannot run the release from a deleted disk). An
// empty device conservatively reads as root — the tracked-fallback
// shape carries no device, and the fallback list never reaches this
// filter.
func isRootDevice(device string) bool {
	return strings.TrimSpace(device) == "" || filepath.Base(device) == "sda1"
}
