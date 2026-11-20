// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"time"

	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// VolumeReleaser is the AWS port.VolumeReleaser: it lists and deletes
// THIS instance's own volumes at shutdown time so a volume whose
// delete_on_termination flag drifts (or whose instance dies before the
// flag can act) leaves no orphan behind. The decision of WHICH volumes
// qualify stays in the domain caller — the adapter's contract is "only
// volumes attached to the named instance, and only ever delete what
// ListOwnVolumes returned".
type VolumeReleaser struct {
	region string
	client ec2ReleaseClient
}

// ec2ReleaseClient is the SDK surface the releaser needs (the
// conformance-suite injection seam).
type ec2ReleaseClient interface {
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DetachVolume(ctx context.Context, params *ec2.DetachVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error)
	DeleteVolume(ctx context.Context, params *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
}

// NewVolumeReleaser builds the production releaser.
func NewVolumeReleaser(region string) *VolumeReleaser {
	return &VolumeReleaser{region: region}
}

// WithReleaseClient injects the API client (conformance tests).
func (r *VolumeReleaser) WithReleaseClient(c ec2ReleaseClient) *VolumeReleaser {
	r.client = c
	return r
}

// client_ resolves the SDK client once (injected in tests, built from
// the region in production).
func (r *VolumeReleaser) client_(ctx context.Context) (ec2ReleaseClient, error) {
	if r.client != nil {
		return r.client, nil
	}
	if r.region == "" {
		return nil, fmt.Errorf("volume releaser: no region resolved")
	}
	cfg, err := awsConfig(ctx, r.region)
	if err != nil {
		return nil, fmt.Errorf("volume releaser aws config: %w", err)
	}
	r.client = ec2.NewFromConfig(cfg)
	return r.client, nil
}

// ListOwnVolumes implements port.VolumeReleaser: every volume attached
// to the instance, with the attachment's device and
// delete_on_termination flag (the caller's release criteria).
func (r *VolumeReleaser) ListOwnVolumes(ctx context.Context, req port.VolumeReleaseRequest) ([]port.AttachedVolume, error) {
	if req.InstanceID == "" {
		return nil, fmt.Errorf("volume releaser: instance id required")
	}
	c, err := r.client_(ctx)
	if err != nil {
		return nil, err
	}
	out, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{{
			Name:   awsapi.String("attachment.instance-id"),
			Values: []string{req.InstanceID},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("describe volumes for %s: %w", req.InstanceID, err)
	}
	return attachedVolumes(out.Volumes, req.InstanceID), nil
}

// attachedVolumes maps the SDK volume set onto the port shape,
// keeping only the attachments that belong to this instance.
func attachedVolumes(volumes []ec2types.Volume, instanceID string) []port.AttachedVolume {
	var out []port.AttachedVolume
	for _, v := range volumes {
		for _, att := range v.Attachments {
			if id := awsapi.ToString(att.InstanceId); id != instanceID {
				continue
			}
			out = append(out, port.AttachedVolume{
				VolumeID:            awsapi.ToString(v.VolumeId),
				Device:              awsapi.ToString(att.Device),
				SizeGB:              awsapi.ToInt32(v.Size),
				DeleteOnTermination: awsapi.ToBool(att.DeleteOnTermination),
			})
		}
	}
	return out
}

// releaseSettleDelay paces the available-state poll after a detach
// (a package var so tests shrink it; production uses 2s).
var releaseSettleDelay = 2 * time.Second

// releasePollAttempts bounds the settle poll: the detach reaches the
// "available" state asynchronously; ten 2s ticks span the worst
// observed settle while staying far inside the shutdown budget.
const releasePollAttempts = 10

// ReleaseVolume implements port.VolumeReleaser: force-detach (the
// shutdown path runs after the caller's own unmount attempt; the OS
// teardown may not complete first) and delete. Idempotent: a volume
// that already vanished (the delete_on_termination flag won the
// race) or is already detached is a success.
func (r *VolumeReleaser) ReleaseVolume(ctx context.Context, volumeID string) error {
	if volumeID == "" {
		return fmt.Errorf("volume releaser: volume id required")
	}
	c, err := r.client_(ctx)
	if err != nil {
		return err
	}
	if err := r.detach(ctx, c, volumeID); err != nil {
		return err
	}
	return r.awaitAvailableThenDelete(ctx, c, volumeID)
}

// detach issues the force-detach. A detach that does not stick
// (already-detached, a peer attachment, throttling) is NOT terminal —
// the caller's poll resolves the volume's true state and re-issues
// the detach while it settles. Only a vanished volume short-circuits
// the release as already-complete.
func (r *VolumeReleaser) detach(ctx context.Context, c ec2ReleaseClient, volumeID string) error {
	if _, err := c.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId: awsapi.String(volumeID),
		Force:    awsapi.Bool(true),
	}); err != nil {
		if isVolumeGone(err) {
			return nil
		}
		logger.GetLogger(ctx).Warn("volume releaser: detach did not stick; resolving state by poll",
			"volume", volumeID, "err", err.Error())
	}
	return nil
}

// awaitAvailableThenDelete polls the volume to the "available" state
// and deletes it there — a DeleteVolume against "detaching"/"in-use"
// fails immediately, so the poll closes the detach's asynchronous
// settle. A still-attached volume re-issues the detach each tick (the
// first one may have been rejected mid-race) and a vanished volume is
// the success the delete_on_termination flag won.
func (r *VolumeReleaser) awaitAvailableThenDelete(ctx context.Context, c ec2ReleaseClient, volumeID string) error {
	for range releasePollAttempts {
		volume, gone, err := describeVolume(ctx, c, volumeID)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}
		switch volume.state {
		case ec2types.VolumeStateAvailable:
			return deleteVolume(ctx, c, volumeID)
		case ec2types.VolumeStateDeleted:
			return nil
		case ec2types.VolumeStateInUse:
			// Still attached (the settle is asynchronous and the
			// first detach may have been rejected mid-race): re-issue
			// it so a transient failure still converges within the
			// poll window.
			if err := r.detach(ctx, c, volumeID); err != nil {
				return err
			}
		}
		if err := sleepReleasePoll(ctx); err != nil {
			return err
		}
	}
	return fmt.Errorf("volume %s never reached available state after detach", volumeID)
}

// volumeSnapshot is the poll's narrow view of one volume: its state
// only (the size/tags are irrelevant to the delete decision).
type volumeSnapshot struct {
	state ec2types.VolumeState
}

// describeVolume reads one volume by id. A vanished volume (the
// describe's NotFound, or an empty answer) reports gone=true — the
// caller's release is already complete.
func describeVolume(ctx context.Context, c ec2ReleaseClient, volumeID string) (volumeSnapshot, bool, error) {
	out, err := c.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	})
	if err != nil {
		if isVolumeGone(err) {
			return volumeSnapshot{}, true, nil
		}
		return volumeSnapshot{}, false, fmt.Errorf("describe volume %s post-detach: %w", volumeID, err)
	}
	if len(out.Volumes) == 0 {
		return volumeSnapshot{}, true, nil
	}
	return volumeSnapshot{state: out.Volumes[0].State}, false, nil
}

// deleteVolume deletes an available volume. A NotFound here is the
// same already-gone success the poll's describe answers.
func deleteVolume(ctx context.Context, c ec2ReleaseClient, volumeID string) error {
	if _, err := c.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
		VolumeId: awsapi.String(volumeID),
	}); err != nil {
		if isVolumeGone(err) {
			return nil
		}
		return fmt.Errorf("delete volume %s: %w", volumeID, err)
	}
	return nil
}

// sleepReleasePoll paces one poll tick, honoring the caller's context.
func sleepReleasePoll(ctx context.Context) error {
	select {
	case <-time.After(releaseSettleDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isVolumeGone reports whether the error is the "no such volume"
// class (the delete_on_termination flag or a prior release already
// removed it).
func isVolumeGone(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidVolume.NotFound"
	}
	return false
}
