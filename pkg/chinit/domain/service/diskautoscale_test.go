// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// The DiskAutoscaler domain service: one Run per timer tick — probe,
// floor check, grow-or-relieve. These specs pin the POLICY with fakes
// on every port; the app layer's exec-backed adapters are transport
// and are exercised in the app package's own suite.

// gb is the bytes-per-GiB multiplier used across the specs.
const gb = uint64(1) << 30

// fakeHeadroomProbe answers one DiskHeadroom.
type fakeHeadroomProbe struct {
	h   port.DiskHeadroom
	err error
}

func (f *fakeHeadroomProbe) Probe() (port.DiskHeadroom, error) {
	return f.h, f.err
}

// fakeAutoscaleResizer records resizes.
type fakeAutoscaleResizer struct {
	calls int
	err   error
}

func (f *fakeAutoscaleResizer) Resize() error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	return nil
}

// fakeVolumeGrower records grow calls and answers with fixed volumes.
type fakeVolumeGrower struct {
	volume      *port.VolumeInfo
	described   []port.VolumeGrowRequest
	grew        []int32
	waited      []string
	describeErr error
	growErr     error
}

func (f *fakeVolumeGrower) DescribeOwnVolume(ctx context.Context, req port.VolumeGrowRequest) (*port.VolumeInfo, error) {
	f.described = append(f.described, req)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return f.volume, nil
}

func (f *fakeVolumeGrower) GrowVolume(ctx context.Context, volumeID string, targetSizeGB int32) (bool, error) {
	if f.growErr != nil {
		return false, f.growErr
	}
	f.grew = append(f.grew, targetSizeGB)
	return true, nil
}

func (f *fakeVolumeGrower) WaitForModification(ctx context.Context, volumeID string) error {
	f.waited = append(f.waited, volumeID)
	return nil
}

// fakeHotPartitionStore implements port.HotPartitionStore for the
// relief path.
type fakeHotPartitionStore struct {
	candidates []port.HotPartition
	moved      []port.HotPartition
	fail       bool
}

func (f *fakeHotPartitionStore) HotPartitions(limit int) ([]port.HotPartition, error) {
	if f.fail {
		return nil, context.Canceled
	}
	return f.candidates, nil
}

func (f *fakeHotPartitionStore) MoveToCold(p port.HotPartition) error {
	f.moved = append(f.moved, p)
	return nil
}

// diskAutoscalerFixture wires the service over fresh fakes.
type diskAutoscalerFixture struct {
	probe   *fakeHeadroomProbe
	resizer *fakeAutoscaleResizer
	grower  *fakeVolumeGrower
	store   *fakeHotPartitionStore
	svc     *DiskAutoscaler
}

// newDiskAutoscalerFixture wires the service with a 32 GiB volume.
func newDiskAutoscalerFixture() *diskAutoscalerFixture {
	f := &diskAutoscalerFixture{
		probe:   &fakeHeadroomProbe{},
		resizer: &fakeAutoscaleResizer{},
		grower: &fakeVolumeGrower{volume: &port.VolumeInfo{
			VolumeID: "vol-test", SizeGB: 32, AttachedTo: "i-test",
		}},
		store: &fakeHotPartitionStore{},
	}
	f.svc = NewDiskAutoscaler(f.probe, f.resizer, f.grower, NewDiskRelief(f.store, 0))
	return f
}

// headroom256 builds a 256 GiB-fs headroom state: the production
// reader volume shape.
func headroom256(unreservedGB uint64) port.DiskHeadroom {
	return port.DiskHeadroom{
		UnreservedBytes:     unreservedGB * gb,
		KeepFreeBytes:       10 * gb,
		FilesystemSizeBytes: 256 * gb,
	}
}

// run executes one tick over the fixture.
func (f *diskAutoscalerFixture) run(t *testing.T) DiskAutoscaleOutcome {
	t.Helper()
	return f.svc.Run(context.Background(), DiskAutoscaleRequest{
		InstanceID: "i-test", Device: "/dev/sdb",
	})
}

func TestDiskAutoscalerIdleAboveFloor(t *testing.T) {
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(60) // 60 GiB unreserved > floor (10 + 38.4 = 48.4 GiB)

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.False(t, outcome.Grew)
	require.Empty(t, f.grower.grew)
	// The resize-first step runs unconditionally: a grown volume whose
	// fs never followed converges at zero modification cost.
	require.Equal(t, 1, f.resizer.calls)
}

func TestDiskAutoscalerGrowsBelowProportionalFloor(t *testing.T) {
	// The 40TB-run blind spot: a 256 GiB volume draining fast — 40 GiB
	// unreserved is ABOVE the old fixed floor (12 GiB) but BELOW the
	// proportional one (48.4 GiB): the grow must trigger here.
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(40)

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.True(t, outcome.Grew)
	require.Equal(t, []int32{96}, f.grower.grew) // 3x the current size
	require.Equal(t, []string{"vol-test"}, f.grower.waited)
	require.Equal(t, 2, f.resizer.calls) // unconditional first + post-grow
}

func TestDiskAutoscalerGrowsOnFixedFloorWhenFsSizeUnknown(t *testing.T) {
	// df failing must NEVER disable growth: the floor degrades to
	// keep_free 10 + fixed 2 = 12 GiB. 5 GiB unreserved still grows.
	f := newDiskAutoscalerFixture()
	f.probe.h = port.DiskHeadroom{UnreservedBytes: 5 * gb, KeepFreeBytes: 10 * gb}

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.True(t, outcome.Grew)
	require.Equal(t, []int32{96}, f.grower.grew)
}

func TestDiskAutoscalerSmallVolumeKeepsFixedFloor(t *testing.T) {
	// A 16 GiB volume: 15% = 2.4 GiB; the max() keeps the fixed 2 GiB —
	// floor stays 12 GiB. 20 GiB unreserved is above it: idle.
	f := newDiskAutoscalerFixture()
	f.probe.h = port.DiskHeadroom{
		UnreservedBytes:     20 * gb,
		KeepFreeBytes:       10 * gb,
		FilesystemSizeBytes: 16 * gb,
	}

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.False(t, outcome.Grew)
}

func TestDiskAutoscalerCapsProportionalHeadroom(t *testing.T) {
	// A 1 TiB fs: 15% = 154 GiB, capped at 64 GiB — floor = 74 GiB.
	// 70 GiB unreserved must trigger the grow.
	f := newDiskAutoscalerFixture()
	f.probe.h = port.DiskHeadroom{
		UnreservedBytes:     70 * gb,
		KeepFreeBytes:       10 * gb,
		FilesystemSizeBytes: 1024 * gb,
	}

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.True(t, outcome.Grew)
}

func TestDiskAutoscalerCapsTargetAtCeiling(t *testing.T) {
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(2)
	f.grower.volume.SizeGB = 250 // 3x = 750 > 512 ceiling

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.True(t, outcome.Grew)
	require.Equal(t, []int32{512}, f.grower.grew)
}

func TestDiskAutoscalerRelievesAtCeiling(t *testing.T) {
	// At the ceiling with pressure: partition relief is the only remedy.
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(2)
	f.grower.volume.SizeGB = 512
	f.store.candidates = []port.HotPartition{
		{Database: "db", Table: "tbl", Partition: "20261119", BytesOnDisk: 10 * gb},
	}

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.False(t, outcome.Grew)
	require.Equal(t, 1, outcome.ReliefMoved)
	require.Equal(t, []port.HotPartition{{Database: "db", Table: "tbl", Partition: "20261119", BytesOnDisk: 10 * gb}}, f.store.moved)
}

func TestDiskAutoscalerFailsLoudlyAtCeilingWithNothingMovable(t *testing.T) {
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(2)
	f.grower.volume.SizeGB = 512

	outcome := f.run(t)
	require.Error(t, outcome.Err)
	require.Contains(t, outcome.Err.Error(), "ceiling")
	require.Contains(t, outcome.Err.Error(), "no hot partitions")
}

func TestDiskAutoscalerFailsLoudlyWithoutGrower(t *testing.T) {
	// Capability-absent provider: pressure must still be reported.
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(2)
	f.svc = NewDiskAutoscaler(f.probe, f.resizer, nil, NewDiskRelief(f.store, 0))

	outcome := f.run(t)
	require.Error(t, outcome.Err)
	require.Contains(t, outcome.Err.Error(), "cannot grow volumes")
}

func TestDiskAutoscalerProbeFailureFailsTick(t *testing.T) {
	f := newDiskAutoscalerFixture()
	f.probe.err = context.Canceled

	outcome := f.run(t)
	require.Error(t, outcome.Err)
	require.Contains(t, outcome.Err.Error(), "probe failed")
}

func TestDiskAutoscalerGrowFailureRoutesToRelief(t *testing.T) {
	// A transient grow failure (rate limit, OPTIMIZING): relief moves
	// hot partitions in the gap; the next tick retries the grow.
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(2)
	f.grower.growErr = fmt.Errorf("IncorrectModificationState")
	f.store.candidates = []port.HotPartition{
		{Database: "db", Table: "tbl", Partition: "20261119", BytesOnDisk: 10 * gb},
	}

	outcome := f.run(t)
	require.NoError(t, outcome.Err)
	require.False(t, outcome.Grew)
	require.Equal(t, 1, outcome.ReliefMoved)
}

func TestDiskAutoscalerGrowFailureWithNothingMovableFailsLoudly(t *testing.T) {
	f := newDiskAutoscalerFixture()
	f.probe.h = headroom256(2)
	f.grower.growErr = fmt.Errorf("IncorrectModificationState")

	outcome := f.run(t)
	require.Error(t, outcome.Err)
	require.Contains(t, outcome.Err.Error(), "IncorrectModificationState")
}

func TestDiskFloorPolicyHeadroom(t *testing.T) {
	p := defaultDiskFloorPolicy()

	// Tiny/unknown fs: the fixed margin.
	require.Equal(t, uint64(2<<30), p.HeadroomBytes(0))
	require.Equal(t, uint64(2<<30), p.HeadroomBytes(8*gb))

	// Proportional in between (GiB with rounding tolerance — the
	// fraction truncates in float64). The 64 GiB cap engages at a
	// ~427 GiB fs: a 512 GiB fs already sits at it.
	require.InDelta(t, 38.4, float64(p.HeadroomBytes(256*gb))/float64(gb), 0.001)
	require.InDelta(t, 64, float64(p.HeadroomBytes(512*gb))/float64(gb), 0.001)

	// Capped.
	require.Equal(t, uint64(64<<30), p.HeadroomBytes(1024*gb))

	// The floor keeps the keep_free invariant.
	require.InDelta(t, 48.4, float64(p.FloorBytes(headroom256(40)))/float64(gb), 0.001)
}

func TestDiskFloorPolicyGrowTarget(t *testing.T) {
	p := defaultDiskFloorPolicy()

	require.Equal(t, int32(96), p.GrowTarget(32))
	require.Equal(t, int32(480), p.GrowTarget(160))
	require.Equal(t, int32(512), p.GrowTarget(250)) // capped at the ceiling
	require.Equal(t, int32(512), p.GrowTarget(512)) // at the ceiling: no growth
	require.Equal(t, int32(0), p.GrowTarget(0))
}
