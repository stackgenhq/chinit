// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"
	"net/http"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// Provider is the Azure registry entry.
type Provider struct {
	region string
	spec   *model.BootSpec

	meta            *IMDSMetadata
	secrets         *KeyVaultSource
	secretsOverride port.SecretSource
	bundle          *BlobBundleSource
	bundleOverride  port.ConfigSource
	claims          *TableClaimer
	lister          *TableClaimLister
	dns             *PrivateDNS
	volumes         *VolumeGrower
	spot            *ScheduledEventsWatcher
	fleetTasks      *TableFleetTaskStore
	status          *TableStatusPublisher
}

// Compile-time capability checks.
var (
	_ port.Provider        = (*Provider)(nil)
	_ port.OrdinalClaimer  = (*Provider)(nil)
	_ port.LeaseLister     = (*Provider)(nil)
	_ port.DNSRegistrar    = (*Provider)(nil)
	_ port.SpotSignals     = (*Provider)(nil)
	_ port.VolumeGrower    = (*Provider)(nil)
	_ port.FleetTaskStore  = (*Provider)(nil)
	_ port.StatusPublisher = (*Provider)(nil)
)

// New builds the Azure provider.
func New(cfg port.Config) port.Provider {
	region := cfg.Region
	if region == "" && cfg.Spec != nil {
		region = cfg.Spec.Region
	}
	p := &Provider{region: region, spec: cfg.Spec}
	p.meta = NewIMDSMetadata()
	p.secrets = NewKeyVaultSource(cfg.Spec)
	p.bundle = NewBlobBundleSource(cfg.Spec)
	p.claims = NewTableClaimer(cfg.Spec)
	p.lister = NewTableClaimLister(p.claims)
	p.dns = NewPrivateDNS(cfg.Spec)
	p.volumes = NewVolumeGrower()
	p.spot = NewScheduledEventsWatcher()
	p.fleetTasks = NewTableFleetTaskStore()
	p.status = NewTableStatusPublisher()
	return p
}

// Name is the registry key.
func (p *Provider) Name() string { return "azure" }

// FleetTaskStore implementation (delegated): the epoch-fenced task
// rows live in the claims table's fleet-tasks partition.

// SubmitTask implements port.FleetTaskStore (delegated).
func (p *Provider) SubmitTask(ctx context.Context, req port.FleetTaskSubmit) (bool, error) {
	return p.fleetTasks.SubmitTask(ctx, req)
}

// ClaimTask implements port.FleetTaskStore (delegated).
func (p *Provider) ClaimTask(ctx context.Context, req port.FleetTaskClaim) (bool, error) {
	return p.fleetTasks.ClaimTask(ctx, req)
}

// HeartbeatTask implements port.FleetTaskStore (delegated).
func (p *Provider) HeartbeatTask(ctx context.Context, req port.FleetTaskHeartbeat) (bool, error) {
	return p.fleetTasks.HeartbeatTask(ctx, req)
}

// CompleteTask implements port.FleetTaskStore (delegated).
func (p *Provider) CompleteTask(ctx context.Context, req port.FleetTaskVerdict) (bool, error) {
	return p.fleetTasks.CompleteTask(ctx, req)
}

// FailTask implements port.FleetTaskStore (delegated).
func (p *Provider) FailTask(ctx context.Context, req port.FleetTaskVerdict) (bool, error) {
	return p.fleetTasks.FailTask(ctx, req)
}

// ListFleetTasks implements port.FleetTaskStore (delegated).
func (p *Provider) ListFleetTasks(ctx context.Context, req port.FleetTaskList) ([]port.FleetTask, error) {
	return p.fleetTasks.ListFleetTasks(ctx, req)
}

// RetryTask implements port.FleetTaskStore (delegated).
func (p *Provider) RetryTask(ctx context.Context, req port.FleetTaskVerdict) (bool, error) {
	return p.fleetTasks.RetryTask(ctx, req)
}

// WithIMDSClient injects a metadata HTTP client (conformance tests).
func (p *Provider) WithIMDSClient(c interface {
	Do(req *http.Request) (*http.Response, error)
}) port.Provider {
	p.meta = p.meta.WithClient(c)
	return p
}

// Metadata probes the Azure IMDS.
func (p *Provider) Metadata(ctx context.Context) (port.InstanceMetadata, error) {
	return p.meta.Get(ctx)
}

// GetSecretValue resolves a Key Vault secret (managed identity), or
// the test-injected override when present.
func (p *Provider) GetSecretValue(ctx context.Context, request port.SecretRequest) (string, error) {
	if p.secretsOverride != nil {
		return p.secretsOverride.GetSecretValue(ctx, request)
	}
	return p.secrets.GetSecretValueCtx(ctx, request.Name, request.Field)
}

// WithSecrets overrides the secret source (conformance tests).
func (p *Provider) WithSecrets(s port.SecretSource) port.Provider {
	p.secretsOverride = s
	return p
}

// WithBundle overrides the config-bundle fetcher (conformance tests).
func (p *Provider) WithBundle(c port.ConfigSource) port.Provider {
	p.bundleOverride = c
	return p
}

// FetchBundle downloads the config bundle from Blob Storage.
func (p *Provider) FetchBundle(ctx context.Context, bundle model.ConfigBundle, destDir string) ([]string, error) {
	if p.bundleOverride != nil {
		return p.bundleOverride.FetchBundle(ctx, bundle, destDir)
	}
	return p.bundle.FetchBundle(ctx, bundle, destDir)
}

// ClaimOrdinal claims a reader ordinal via Table Storage.
func (p *Provider) ClaimOrdinal(ctx context.Context, req port.ClaimRequest) (int, error) {
	return p.claims.Claim(ctx, req)
}

// RenewLease extends the node's lease.
func (p *Provider) RenewLease(ctx context.Context, req port.RenewRequest) error {
	return p.claims.Renew(ctx, req)
}

// ReleaseLease drops the node's claim.
func (p *Provider) ReleaseLease(ctx context.Context, req port.ReleaseRequest) error {
	return p.claims.Release(ctx, req)
}

// ListActiveClaims enumerates the cluster's live ordinal leases (the
// inventory source for remote_servers and the -host flags) over the
// same Table Storage the claimer writes.
func (p *Provider) ListActiveClaims(ctx context.Context, req port.ListClaimsRequest) ([]port.Claim, error) {
	return p.lister.ListActiveClaims(ctx, req)
}

// DescribeOwnVolume returns the data disk attached to this VM.
func (p *Provider) DescribeOwnVolume(ctx context.Context, req port.VolumeGrowRequest) (*port.VolumeInfo, error) {
	return p.volumes.DescribeOwnVolume(ctx, req)
}

// GrowVolume extends this VM's managed disk online (no-op at target).
func (p *Provider) GrowVolume(ctx context.Context, volumeID string, targetSizeGB int32) (bool, error) {
	return p.volumes.GrowVolume(ctx, volumeID, targetSizeGB)
}

// WaitForModification blocks until the disk resize converges.
func (p *Provider) WaitForModification(ctx context.Context, volumeID string) error {
	return p.volumes.WaitForModification(ctx, volumeID)
}

// ResolveZoneID finds the private DNS zone by name (ARM query).
func (p *Provider) ResolveZoneID(ctx context.Context, zoneName string) (string, error) {
	return p.dns.ResolveZoneID(ctx, zoneName)
}

// UpsertARecord creates or replaces the node's A record.
func (p *Provider) UpsertARecord(ctx context.Context, req port.ARecordRequest) error {
	return p.dns.UpsertARecord(ctx, req)
}

// DeleteARecord removes the node's A record.
func (p *Provider) DeleteARecord(ctx context.Context, req port.ARecordRequest) error {
	return p.dns.DeleteARecord(ctx, req)
}

// Watch polls scheduled events for the Preempt document.
func (p *Provider) Watch(ctx context.Context) <-chan port.InterruptionNotice {
	return p.spot.Watch(ctx)
}

// azureConfigError names missing spec fields a constructor needed (the
// app layer renders it before any client is built).
// fmtGuard keeps fmt imported for the smaller files in this package.
var _ = fmt.Sprintf

// StatusPublisher implementation (delegated): the node-status rows
// ride the claims table's node-status partition.

// PublishStatus implements port.StatusPublisher (delegated).
func (p *Provider) PublishStatus(ctx context.Context, req port.NodeStatusPublish) error {
	return p.status.PublishStatus(ctx, req)
}

// ListStatus implements port.StatusPublisher (delegated).
func (p *Provider) ListStatus(ctx context.Context, req port.NodeStatusList) ([]port.NodeStatus, error) {
	return p.status.ListStatus(ctx, req)
}

// SetCordon implements port.StatusPublisher (delegated).
func (p *Provider) SetCordon(ctx context.Context, req port.NodeCordonRequest) error {
	return p.status.SetCordon(ctx, req)
}
