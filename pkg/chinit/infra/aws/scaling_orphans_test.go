// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// fakeSweepClient pins the sweeper's contract over a pure fake.
type fakeSweepClient struct {
	volumes   []ec2types.Volume
	deleted   []string
	deleteErr map[string]error
}

func (f *fakeSweepClient) DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, o ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	return &ec2.DescribeVolumesOutput{Volumes: f.volumes}, nil
}
func (f *fakeSweepClient) DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, o ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	if f.deleteErr != nil && f.deleteErr[*in.VolumeId] != nil {
		return nil, f.deleteErr[*in.VolumeId]
	}
	f.deleted = append(f.deleted, *in.VolumeId)
	return &ec2.DeleteVolumeOutput{}, nil
}
func (f *fakeSweepClient) DescribeAddresses(ctx context.Context, in *ec2.DescribeAddressesInput, o ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error) {
	return &ec2.DescribeAddressesOutput{}, nil
}
func (f *fakeSweepClient) DescribeSnapshots(ctx context.Context, in *ec2.DescribeSnapshotsInput, o ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error) {
	return &ec2.DescribeSnapshotsOutput{}, nil
}

func vol(id string, tags []ec2types.Tag, age time.Duration) ec2types.Volume {
	return ec2types.Volume{
		VolumeId:   aws.String(id),
		Tags:       tags,
		CreateTime: aws.Time(time.Now().Add(-age)),
	}
}

func TestOrphanSweepDeletesOnlyUntaggedOld(t *testing.T) {
	old := 48 * time.Hour
	client := &fakeSweepClient{volumes: []ec2types.Volume{
		vol("vol-untagged-old", nil, old),
		vol("vol-untagged-fresh", nil, time.Hour),
		vol("vol-tagged-old", []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("x")}}, old),
	}}
	s := NewOrphanSweeper("").WithSweepClient(client)
	res, err := s.SweepOrphans(context.Background(), port.OrphanSweepRequest{AgeThreshold: 24 * time.Hour})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.VolumesFound) != 1 || res.VolumesFound[0] != "vol-untagged-old" {
		t.Fatalf("found = %v, want [vol-untagged-old]", res.VolumesFound)
	}
	if len(res.VolumesDeleted) != 1 || res.VolumesDeleted[0] != "vol-untagged-old" {
		t.Fatalf("deleted = %v, want [vol-untagged-old]", res.VolumesDeleted)
	}
}

func TestOrphanSweepDryRunReportsWithoutDeleting(t *testing.T) {
	client := &fakeSweepClient{volumes: []ec2types.Volume{
		vol("vol-untagged-old", nil, 48*time.Hour),
	}}
	s := NewOrphanSweeper("").WithSweepClient(client)
	res, err := s.SweepOrphans(context.Background(), port.OrphanSweepRequest{AgeThreshold: 24 * time.Hour, DryRun: true})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.VolumesFound) != 1 {
		t.Fatalf("found = %v", res.VolumesFound)
	}
	if len(res.VolumesDeleted) != 0 {
		t.Fatalf("dry-run deleted = %v, want none", res.VolumesDeleted)
	}
}

func TestOrphanSweepStuckVolumeNeverFailsRun(t *testing.T) {
	client := &fakeSweepClient{
		volumes: []ec2types.Volume{
			vol("vol-stuck", nil, 48*time.Hour),
			vol("vol-fine", nil, 48*time.Hour),
		},
		deleteErr: map[string]error{"vol-stuck": errFake},
	}
	s := NewOrphanSweeper("").WithSweepClient(client)
	res, err := s.SweepOrphans(context.Background(), port.OrphanSweepRequest{AgeThreshold: 24 * time.Hour})
	if err != nil {
		t.Fatalf("one stuck volume must not fail the sweep: %v", err)
	}
	if len(res.VolumesDeleted) != 1 || res.VolumesDeleted[0] != "vol-fine" {
		t.Fatalf("deleted = %v, want [vol-fine]", res.VolumesDeleted)
	}
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "fake delete failure" }
