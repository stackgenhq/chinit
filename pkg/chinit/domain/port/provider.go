// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"
	"errors"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// InstanceMetadata is what the boot pipeline needs from the platform the
// node is running on. Everything identity-related derives from it.
type InstanceMetadata struct {
	Provider   string // "aws", "azure", …
	Region     string
	Zone       string // availability zone, when the platform has one
	InstanceID string
	PrivateIP  string
	// HostnameHint is the platform-assigned hostname (EC2's ip-* or the
	// cloud-init hostname); the identity step overrides it with the
	// settled cluster identity.
	HostnameHint string
}

// MetadataProvider reads instance metadata. Mandatory: every provider
// implements this (AWS IMDSv2, Azure IMDS; a cloud without an IMDS
// endpoint implements it from /etc/chinit/metadata baked by user-data).
type MetadataProvider interface {
	// Metadata returns the instance metadata, probing the platform.
	// Callers retry transient failures at the policy layer.
	Metadata(ctx context.Context) (InstanceMetadata, error)
}

// ConfigSource fetches the node's config bundle (chinit.yaml, static
// XMLs, chi.yaml/keeper.yaml) from provider object storage.
//
// Why a port: bootstrap.sh shelled aws s3 cp with hand-rolled retries;
// Azure needs Blob, and the SHA gate must sit in one place.
type ConfigSource interface {
	// FetchBundle downloads the bundle directory into destDir and
	// returns the files written. The SHA256 gate (when the spec pins
	// one) is the provider's contract: a mismatch is a Fatal error,
	// never a retry.
	FetchBundle(ctx context.Context, bundle model.ConfigBundle, destDir string) ([]string, error)
}

// OrdinalClaimer lets an ASG member claim a stable reader-N identity.
//
// Why a lease and not static naming: readers are a spot/on-demand mix
// behind an ASG; identity must be stable across replacements while the
// instance itself is ephemeral. First writer-registered claim wins; the
// TTL lets a dead node's ordinal expire (AWS: DynamoDB conditional put;
// Azure: Table Storage entity with lease_until).
type OrdinalClaimer interface {
	// ClaimOrdinal acquires the first free ordinal in [0, count) and
	// returns it. It retries the sweep internally up to the deadline
	// (one lease window) — an instance-refresh replacement boots while
	// the old node still holds its lease.
	ClaimOrdinal(ctx context.Context, req ClaimRequest) (int, error)
	// RenewLease extends the holder's lease (the renewal unit calls it
	// every 5 minutes against a 15-minute TTL).
	RenewLease(ctx context.Context, req RenewRequest) error
	// ReleaseLease deletes the node's claim (spot drain / termination).
	ReleaseLease(ctx context.Context, req ReleaseRequest) error
}

// ClaimRequest is the input to ClaimOrdinal.
type ClaimRequest struct {
	Table        string // provider lease table (DynamoDB table / Storage table)
	Prefix       string // hostname prefix ("reader-")
	Count        int    // ordinals 0..Count-1
	InstanceID   string
	PrivateIP    string
	LeaseSeconds int
	// Deadline bounds the whole claim sweep (16 min > 15-min lease).
	DeadlineSeconds int
	// ExactOrdinal restricts the claim to the caller's exact ordinal
	// (prefix+Count) rather than sweeping for any free slot. Used when
	// re-claiming an existing node's own ordinal during lease renewal.
	ExactOrdinal bool
}

// RenewRequest is the input to RenewLease.
type RenewRequest struct {
	Table        string
	Hostname     string
	InstanceID   string
	LeaseSeconds int
}

// ReleaseRequest is the input to ReleaseLease.
type ReleaseRequest struct {
	Table    string
	Hostname string
	// InstanceID scopes the release to the claim THIS instance holds
	// (the ownership guard): a drain overlapping its replacement's
	// re-claim must not delete the new node's row. Providers without
	// an ownership concept may ignore it.
	InstanceID string
}

// ErrLeaseStolen is the sentinel for a FAILED-OWNERSHIP renewal: the
// lease's conditional update rejected this instance because another
// instance holds the ordinal now. Every OrdinalClaimer maps its
// native conditional failure (DynamoDB ConditionalCheckFailed, Table
// Storage's etag precondition) onto it. The boot's identity settle
// treats it as Fatal — the node must not serve under a name someone
// else owns — while TRANSIENT renewal errors (throttling, network)
// stay plain errors the retry policy owns. Without the sentinel, the
// two classes shared one Fatal classification and one API blip
// aborted healthy in-place refreshes.
var ErrLeaseStolen = errors.New("lease stolen: another instance holds this ordinal")

// DNSRegistrar manages the node's private-zone A record. Readers
// self-register at claim time and the spot drain deregisters; writers
// register statically when the zone id is known.
type DNSRegistrar interface {
	// ResolveZoneID finds the private zone by exact DNS name (the
	// Terraform graph cannot pass the zone id without a cycle, so it
	// is resolved at boot and cached).
	ResolveZoneID(ctx context.Context, zoneName string) (string, error)
	// UpsertARecord creates or replaces the node's A record.
	UpsertARecord(ctx context.Context, req ARecordRequest) error
	// DeleteARecord removes it (drain/termination).
	DeleteARecord(ctx context.Context, req ARecordRequest) error
}

// ARecordRequest names one record operation.
type ARecordRequest struct {
	ZoneID     string
	ZoneName   string
	RecordName string // short hostname; FQDN = RecordName + "." + ZoneName
	IP         string
	TTL        int
	// ExactOnly deletes the DNS record only when its current IP matches
	// the requested IP, preventing a terminating instance from deleting
	// a record that a replacement instance has already re-registered.
	ExactOnly bool
}

// InterruptionNotice is the platform's "you are being reclaimed" signal.
type InterruptionNotice struct {
	// DeadlineSeconds is how long the node has (EC2 spot: 120; Azure
	// scheduled events: notice window from the event payload).
	DeadlineSeconds int
	// Raw is the platform payload for the drain log.
	Raw string
}

// SpotSignals watches the platform's interruption endpoint and delivers
// the notice once. EC2: IMDS spot/instance-action; Azure: Scheduled
// Events Preempt document. Long-running — the spot-watch subcommand
// owns the loop; the channel closes after the first notice.
type SpotSignals interface {
	Watch(ctx context.Context) <-chan InterruptionNotice
}

// MetricPushRequest is one custom metric datapoint (the self-healing
// jobs' CloudWatch handshake — keeper_up, disk-autoscale outcomes).
type MetricPushRequest struct {
	// Namespace is the CloudWatch namespace ("ClickHouse/Keeper").
	Namespace string
	// Name is the metric name ("keeper_up").
	Name string
	// Value is the datapoint.
	Value float64
	// Unit is the CloudWatch unit enum string ("None", "Count", ...).
	Unit string
	// Dimensions are the EMF-style dimension pairs the alarms key on.
	// The alarm's dimensions must be a SUBSET of these.
	Dimensions map[string]string
}

// MetricPusher is the optional capability for chinit's maintenance
// jobs to publish liveness and outcome metrics.
type MetricPusher interface {
	// PutMetric publishes one datapoint. Transient failures are the
	// caller's to retry (jobs run on timers); a missing capability is
	// NOT an error — probe first.
	PutMetric(ctx context.Context, req MetricPushRequest) error
}

// VolumeInfo is the discovered state of one attached volume.
type VolumeInfo struct {
	// VolumeID is the provider's volume identifier.
	VolumeID string
	// SizeGB is the current capacity.
	SizeGB int32
	// AttachedTo is the instance the volume serves.
	AttachedTo string
}

// VolumeGrowRequest names the volume to grow and the target size.
type VolumeGrowRequest struct {
	// InstanceID is THIS node's instance identifier (the grower
	// double-gates ownership: it only ever returns volumes attached
	// to this instance).
	InstanceID string
	// Device is the attachment device path ("/dev/sdb" — the data
	// disk by launch-template convention).
	Device string
	// TargetSizeGB is the desired capacity; a no-op when the volume
	// is already at or beyond it.
	TargetSizeGB int32
}

// VolumeGrower is the optional capability for online data-volume
// growth — the disk-autoscale job's cloud seam.
//
// Why a port: effective free can sit near zero under a
// keep_free_space reservation (a 32 GiB volume with a 10 GiB
// reservation) while merges hit NOT_ENOUGH_SPACE and inserts
// approach the parts_to_throw_insert hard reject. Growing an
// EBS volume is non-disruptive and idempotent, but the cloud call is
// provider-specific (EC2 ModifyVolume vs Azure managed-disk resize),
// so the job keys on the capability: absent → loud warning, the job
// still reports the pressure it found (the alarm-side signal).
type VolumeGrower interface {
	// DescribeOwnVolume returns the volume attached to this instance
	// at the device, or an error when none exists.
	DescribeOwnVolume(ctx context.Context, req VolumeGrowRequest) (*VolumeInfo, error)
	// GrowVolume extends the volume to the request's target and
	// returns whether a modification actually started.
	GrowVolume(ctx context.Context, volumeID string, targetSizeGB int32) (bool, error)
	// WaitForModification blocks until the in-flight modification
	// completes (the caller owns the deadline).
	WaitForModification(ctx context.Context, volumeID string) error
}

// Provider is the factory product: the mandatory capabilities every
// cloud implements, plus the chctl SecretSource (passwords/TLS — the
// same port chctl already uses, so one adapter serves both binaries).
type Provider interface {
	// Name is the provider registry key ("aws", "azure", …).
	Name() string
	// Metadata probes instance metadata (mandatory capability).
	MetadataProvider
	// Secrets resolves secret-store entries by name (mandatory).
	SecretSource
	// ConfigSource fetches the config bundle (mandatory).
	ConfigSource
}

// Capability probes — the factory's optional-capability seam. The boot
// steps call these once and degrade loudly (WARNING + feature disabled)
// when a provider lacks a capability, so a Civo-shaped provider (no
// IMDS, no managed DNS) still boots instead of panic-failing.

// AsOrdinalClaimer returns the claim capability, or nil.
func AsOrdinalClaimer(p Provider) OrdinalClaimer {
	if c, ok := p.(OrdinalClaimer); ok {
		return c
	}
	return nil
}

// AsDNSRegistrar returns the DNS capability, or nil.
func AsDNSRegistrar(p Provider) DNSRegistrar {
	if c, ok := p.(DNSRegistrar); ok {
		return c
	}
	return nil
}

// AsSpotSignals returns the interruption-watch capability, or nil.
func AsSpotSignals(p Provider) SpotSignals {
	if c, ok := p.(SpotSignals); ok {
		return c
	}
	return nil
}

// AsMetricPusher returns the metric-publish capability, or nil. The
// self-healing jobs degrade to log-only when absent (the keeper's
// mntr exit status remains authoritative even when the push cannot
// happen).
func AsMetricPusher(p Provider) MetricPusher {
	if c, ok := p.(MetricPusher); ok {
		return c
	}
	return nil
}

// AsVolumeGrower returns the online volume-growth capability, or nil.
// The disk-autoscale job degrades to alarm-only when absent (the
// pressure it found is still reported through the health metric).
func AsVolumeGrower(p Provider) VolumeGrower {
	if c, ok := p.(VolumeGrower); ok {
		return c
	}
	return nil
}

// AttachedVolume is one non-root volume attached to this instance,
// as the volume-release path sees it.
type AttachedVolume struct {
	// VolumeID is the provider's volume identifier.
	VolumeID string
	// Device is the EC2 attachment device ("/dev/sdb").
	Device string
	// SizeGB is the capacity (context for the release log).
	SizeGB int32
	// DeleteOnTermination reports the attachment's flag: a false
	// value is the orphan-leak class (the volume would outlive the
	// instance), the release path's primary target.
	DeleteOnTermination bool
}

// VolumeReleaseRequest names the instance whose volumes release.
type VolumeReleaseRequest struct {
	// InstanceID is THIS node's identifier (the releaser only ever
	// returns volumes attached to it).
	InstanceID string
}

// VolumeReleaser is the optional capability the shutdown volume-release
// path calls: delete this node's own stateless volumes at termination
// so nothing is left for the orphan sweep to find.
//
// Why a port: the release is provider-specific (EC2 Detach+Delete
// vs Azure managed-disk detach+delete), while the policy — release
// only at termination, never on reboot, only the volumes attached to
// THIS instance — is domain logic every cloud shares.
type VolumeReleaser interface {
	// ListOwnVolumes returns every volume attached to this instance
	// (the caller decides which qualify for release).
	ListOwnVolumes(ctx context.Context, req VolumeReleaseRequest) ([]AttachedVolume, error)
	// ReleaseVolume force-detaches and deletes one volume. Idempotent:
	// an already-deleted volume is a success, not an error.
	ReleaseVolume(ctx context.Context, volumeID string) error
}

// AsVolumeReleaser returns the volume-release capability, or nil.
func AsVolumeReleaser(p Provider) VolumeReleaser {
	if c, ok := p.(VolumeReleaser); ok {
		return c
	}
	return nil
}

// Claim is one live ordinal-lease row (see LeaseLister): the identity a
// cluster member currently holds — hostname reader-N/writer-N with the
// instance that owns it.
type Claim struct {
	Hostname   string
	InstanceID string
	IP         string
}

// LeaseLister enumerates the cluster's ACTIVE ordinal leases.
//
// Why an optional capability: cluster inventory (remote_servers, the
// schema-sync -host flags, chctl health fan-out) must enumerate only
// ACTIVE lease holders — a reserved-but-vacant slot's CR host would put
// an unreachable peer in remote_servers/clusterAllReplicas and fail
// every DDL read that fans out . The ASG owns
// slot RESERVATIONS (max_size); the lease table owns LIVENESS, so the
// lease table is the only source that knows which reserved slots have a
// machine behind them. Providers without a listable lease store keep
// the static spec inventory (loud warning at the call site).
type LeaseLister interface {
	// ListActiveClaims returns every lease row with lease_until > now
	// from the table (one DynamoDB Scan / Azure Table query — the
	// caller treats the result as the cluster's live member set).
	ListActiveClaims(ctx context.Context, req ListClaimsRequest) ([]Claim, error)
}

// ListClaimsRequest is the input to ListActiveClaims.
type ListClaimsRequest struct {
	// Table is the provider lease table to scan (DynamoDB table /
	// Storage table) — the same table ClaimOrdinal writes.
	Table string
}

// AsLeaseLister returns the lease-listing capability, or nil.
func AsLeaseLister(p Provider) LeaseLister {
	if c, ok := p.(LeaseLister); ok {
		return c
	}
	return nil
}

// Registry is the provider factory: name → constructor. Detection fills
// in the constructor's config from metadata; a new cloud is one entry
// here plus one infra package.
type Registry map[string]func(cfg Config) Provider

// Config carries what a constructor needs before metadata exists: the
// region hint and the resolved spec (for provider-specific knobs like
// the Route53 zone id override).
type Config struct {
	Region string
	Spec   *model.BootSpec
}

// Register adds a provider constructor. Called from each infra package's
// init — app imports the infra packages for their side effect and the
// registry is complete.
func (r Registry) Register(name string, ctor func(Config) Provider) {
	r[name] = ctor
}

// Build instantiates the named provider.
func (r Registry) Build(name string, cfg Config) (Provider, error) {
	ctor, ok := r[name]
	if !ok {
		return nil, &UnknownProviderError{Name: name}
	}
	return ctor(cfg), nil
}

// UnknownProviderError names a provider the registry does not know.
type UnknownProviderError struct{ Name string }

func (e *UnknownProviderError) Error() string {
	return "unknown cloud provider: " + e.Name + " (known: register it in pkg/chinit/infra)"
}
