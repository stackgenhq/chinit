// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// DiskRelief relieves hot-disk space pressure when online volume growth
// is unavailable by moving the largest hot-disk partitions to the cold
// object-storage volume.
type DiskRelief struct {
	// store is the SQL surface: list the candidates, move each one.
	store port.HotPartitionStore
	// perPass bounds one relief pass: each MOVE PARTITION lands data
	// in S3; a bounded pass keeps the motion inside the timer budget
	// while freeing gigabytes.
	perPass int
}

// NewDiskRelief builds the relief over a partition store. perPass <= 0
// falls back to the default bound.
func NewDiskRelief(store port.HotPartitionStore, perPass int) *DiskRelief {
	if perPass <= 0 {
		perPass = defaultReliefPerPass
	}
	return &DiskRelief{store: store, perPass: perPass}
}

// defaultReliefPerPass bounds one 5-minute-timer relief pass.
const defaultReliefPerPass = 3

// ReliefResult reports what one pass moved.
type ReliefResult struct {
	// Moved is the partitions sent to the cold volume.
	Moved int
	// Failures is the partitions whose move errored (the pass
	// continues past them; the next tick and the TTL mover both
	// retry).
	Failures int
}

// Relieve moves up to perPass partitions, biggest first. A move
// failure never aborts the pass — the steady-state TTL mover owns the
// retry either way. Returns the moved count (0 = no qualifying
// candidates; the caller decides whether that is terminal).
func (r *DiskRelief) Relieve(ctx context.Context) (ReliefResult, error) {
	candidates, err := r.store.HotPartitions(r.perPass)
	if err != nil {
		return ReliefResult{}, err
	}
	result := ReliefResult{}
	for _, p := range candidates {
		if result.Moved >= r.perPass {
			break
		}
		if err := r.store.MoveToCold(p); err != nil {
			result.Failures++
			logger.GetLogger(ctx).Warn("disk relief: partition move failed (continuing)",
				"table", p.Table, "partition", p.Partition, "err", err)
			continue
		}
		result.Moved++
		logger.GetLogger(ctx).Info("disk relief: partition moved to the cold volume",
			"table", p.Table, "partition", p.Partition)
	}
	return result, nil
}
