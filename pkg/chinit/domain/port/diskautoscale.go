// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

// The disk-autoscale ports: the headroom probe the grow floor derives
// from, and the filesystem resize the grown volume needs before the
// new capacity is usable. The app layer binds each to on-host
// commands (clickhouse-client for the probe, df/findmnt/resize2fs for
// the resize); the domain service owns the POLICY (when the floor is
// crossed and how much to grow).

// DiskHeadroom is the measured state of the default (hot) disk at one
// instant, in bytes.
type DiskHeadroom struct {
	// UnreservedBytes is what the reservation book would hand a new
	// write: free space minus everything background operations hold
	// (fetch storms, merges, part moves). The grow floor keys on THIS
	// — not free space.
	UnreservedBytes uint64
	// KeepFreeBytes is the server's keep_free_space reservation
	// (storage.xml). The floor derives FROM it, so the floor always
	// exceeds the reservation whatever the XML says.
	KeepFreeBytes uint64
	// FilesystemSizeBytes is the capacity of the mounted filesystem.
	// The floor's headroom scales with it, so large volumes trigger
	// growth earlier relative to their drain rates.
	FilesystemSizeBytes uint64
}

// DiskHeadroomProbe measures DiskHeadroom once. The probe combines
// the server's reservation book (unreserved + keep_free) with the
// mounted filesystem's size — the service derives the floor from all
// three without knowing how they were read.
type DiskHeadroomProbe interface {
	// Probe returns the default disk's headroom state. A partial
	// failure must NOT disable growth: the caller degrades to the
	// headroom it could measure (KeepFreeBytes alone when the rest
	// failed) rather than skipping the tick.
	Probe() (DiskHeadroom, error)
}

// FilesystemResizer grows the ext4 filesystem to fill the volume: the
// post-grow step whose absence leaves the block device bigger than
// the filesystem (the kernel sees the capacity; the server still
// writes inside the old bounds).
type FilesystemResizer interface {
	// Resize extends the filesystem on the data mount to the device's
	// full size. Idempotent: resizing an already-maximal filesystem is
	// a cheap no-op.
	Resize() error
}

// VolumePlan is the autoscaler's decision for one tick: the target
// size for the node's own data volume, or the idle/terminal states.
type VolumePlan struct {
	// GrowToGB is the target size in GiB; 0 means no growth needed.
	GrowToGB int32
	// CurrentGB is the volume's measured current size — the plan's
	// baseline and its ceiling anchor.
	CurrentGB int32
	// AtCeiling marks the plan that cannot relieve pressure by growth:
	// the target landed at the configured MaxGB. Partition relief is
	// the remaining remedy.
	AtCeiling bool
}

// Idle marks the plan when headroom sits above the floor: the tick is
// a no-op.
func (p VolumePlan) Idle() bool { return p.GrowToGB == 0 }

// Grew reports whether the plan grew past the current size.
func (p VolumePlan) Grew() bool { return p.GrowToGB > p.CurrentGB }
