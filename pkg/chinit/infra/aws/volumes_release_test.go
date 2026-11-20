// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"testing"
	"time"

	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// fakeReleaseClient fakes the release SDK surface: canned volumes,
// per-call state transitions, and recorded detach/delete calls.
type fakeReleaseClient struct {
	volumes   map[string]ec2types.Volume
	detached  []string
	deleted   []string
	failAfter int // detach failures allowed before success (0 = never)
	calls     int
}

func (f *fakeReleaseClient) DescribeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput, o ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	if in.VolumeIds != nil {
		var out []ec2types.Volume
		for _, id := range in.VolumeIds {
			if v, ok := f.volumes[id]; ok {
				out = append(out, v)
			}
		}
		return &ec2.DescribeVolumesOutput{Volumes: out}, nil
	}
	var out []ec2types.Volume
	for _, v := range f.volumes {
		for _, att := range v.Attachments {
			if att.InstanceId != nil && *att.InstanceId == in.Filters[0].Values[0] {
				out = append(out, v)
			}
		}
	}
	return &ec2.DescribeVolumesOutput{Volumes: out}, nil
}

func (f *fakeReleaseClient) DetachVolume(ctx context.Context, in *ec2.DetachVolumeInput, o ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error) {
	f.calls++
	if f.failAfter > 0 && f.calls <= f.failAfter {
		return nil, &smithy.GenericAPIError{Code: "VolumeInUse", Message: "still in use"}
	}
	f.detached = append(f.detached, *in.VolumeId)
	v, ok := f.volumes[*in.VolumeId]
	if ok {
		v.Attachments = nil
		v.State = ec2types.VolumeStateAvailable
		f.volumes[*in.VolumeId] = v
	}
	return &ec2.DetachVolumeOutput{}, nil
}

func (f *fakeReleaseClient) DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, o ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	f.deleted = append(f.deleted, *in.VolumeId)
	delete(f.volumes, *in.VolumeId)
	return &ec2.DeleteVolumeOutput{}, nil
}

func attachedVol(id, device, instance string, dot bool) ec2types.Volume {
	return ec2types.Volume{
		VolumeId: awsapi.String(id),
		Size:     awsapi.Int32(64),
		State:    ec2types.VolumeStateInUse,
		Attachments: []ec2types.VolumeAttachment{{
			InstanceId:          awsapi.String(instance),
			Device:              awsapi.String(device),
			DeleteOnTermination: awsapi.Bool(dot),
		}},
	}
}

func TestListOwnVolumesReturnsAttachments(t *testing.T) {
	client := &fakeReleaseClient{volumes: map[string]ec2types.Volume{
		"vol-root": attachedVol("vol-root", "/dev/sda1", "i-1", true),
		"vol-meta": attachedVol("vol-meta", "/dev/sdb", "i-1", true),
	}}
	r := NewVolumeReleaser("").WithReleaseClient(client)
	vols, err := r.ListOwnVolumes(context.Background(), port.VolumeReleaseRequest{InstanceID: "i-1"})
	if err != nil {
		t.Fatalf("ListOwnVolumes: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("want 2 volumes, got %d", len(vols))
	}
	for _, v := range vols {
		if v.VolumeID == "vol-meta" {
			if !v.DeleteOnTermination || v.Device != "/dev/sdb" {
				t.Fatalf("vol-meta shape wrong: %+v", v)
			}
		}
	}
}

func TestListOwnVolumesRequiresInstance(t *testing.T) {
	r := NewVolumeReleaser("")
	if _, err := r.ListOwnVolumes(context.Background(), port.VolumeReleaseRequest{}); err == nil {
		t.Fatal("empty instance id must error")
	}
}

func TestReleaseVolumeDetachesThenDeletes(t *testing.T) {
	client := &fakeReleaseClient{volumes: map[string]ec2types.Volume{
		"vol-meta": attachedVol("vol-meta", "/dev/sdb", "i-1", true),
	}}
	r := NewVolumeReleaser("").WithReleaseClient(client)
	if err := r.ReleaseVolume(context.Background(), "vol-meta"); err != nil {
		t.Fatalf("ReleaseVolume: %v", err)
	}
	if len(client.detached) != 1 || client.detached[0] != "vol-meta" {
		t.Fatalf("detach not recorded: %v", client.detached)
	}
	if len(client.deleted) != 1 || client.deleted[0] != "vol-meta" {
		t.Fatalf("delete not recorded: %v", client.deleted)
	}
}

func TestReleaseVolumeAlreadyGoneIsSuccess(t *testing.T) {
	client := &fakeReleaseClient{volumes: map[string]ec2types.Volume{}}
	r := NewVolumeReleaser("").WithReleaseClient(client)
	// Describe of a nonexistent volume id: the fake answers an empty
	// list, the releaser treats it as gone.
	if err := r.ReleaseVolume(context.Background(), "vol-gone"); err != nil {
		t.Fatalf("already-gone volume must be a success, got %v", err)
	}
}

func TestReleaseVolumeStuckDetachStillDeletesWhenAvailable(t *testing.T) {
	// A detach that does not stick (VolumeInUse once) must not fail
	// the release: the poll resolves the state and deletes the
	// moment the volume reaches available.
	client := &fakeReleaseClient{
		volumes: map[string]ec2types.Volume{
			"vol-meta": attachedVol("vol-meta", "/dev/sdb", "i-1", false),
		},
		failAfter: 1,
	}
	r := NewVolumeReleaser("").WithReleaseClient(client)
	// Shrink the settle delay so the test runs fast.
	origAfter := releaseSettleDelay
	releaseSettleDelay = time.Millisecond
	defer func() { releaseSettleDelay = origAfter }()
	if err := r.ReleaseVolume(context.Background(), "vol-meta"); err != nil {
		t.Fatalf("ReleaseVolume with one stuck detach: %v", err)
	}
	if len(client.deleted) != 1 {
		t.Fatalf("delete not recorded after stuck detach: %v", client.deleted)
	}
}

func TestIsVolumeGoneClassifiesNotFound(t *testing.T) {
	if !isVolumeGone(&smithy.GenericAPIError{Code: "InvalidVolume.NotFound"}) {
		t.Fatal("InvalidVolume.NotFound must classify as gone")
	}
	if isVolumeGone(&smithy.GenericAPIError{Code: "VolumeInUse"}) {
		t.Fatal("VolumeInUse must not classify as gone")
	}
	if isVolumeGone(errors.New("plain")) {
		t.Fatal("plain errors must not classify as gone")
	}
}
