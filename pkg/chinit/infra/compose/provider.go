// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// Provider is the Docker Compose entry in chinit's provider registry.
// It satisfies mandatory Provider ports using mounted container files and
// optional coordination ports (OrdinalClaimer, LeaseLister,
// StatusPublisher, RoleSpecSource, RefreshLock) using a shared
// flock-guarded JSON store.
type Provider struct {
	region          string
	spec            *model.BootSpec
	meta            *MetadataSource
	secrets         *FileSecretSource
	secretsOverride port.SecretSource
	bundle          *LocalBundleSource
	bundleOverride  port.ConfigSource
	leases          *FileLeaseStore
}

var (
	_ port.Provider        = (*Provider)(nil)
	_ port.OrdinalClaimer  = (*Provider)(nil)
	_ port.LeaseLister     = (*Provider)(nil)
	_ port.StatusPublisher = (*Provider)(nil)
	_ port.FleetTaskStore  = (*Provider)(nil)
	_ port.RoleSpecSource  = (*Provider)(nil)
	_ port.RefreshLock     = (*Provider)(nil)
)

// New builds the Docker Compose provider.
func New(cfg port.Config) port.Provider {
	region := cfg.Region
	if region == "" && cfg.Spec != nil {
		region = cfg.Spec.Region
	}
	return &Provider{
		region:  region,
		spec:    cfg.Spec,
		meta:    NewMetadataSource(region),
		secrets: NewFileSecretSource(),
		bundle:  NewLocalBundleSource(),
		leases:  NewFileLeaseStore(),
	}
}

// Name is the registry key.
func (p *Provider) Name() string { return "compose" }

// WithMetadata pins fixed instance metadata (conformance/unit tests).
func (p *Provider) WithMetadata(meta port.InstanceMetadata) *Provider {
	p.meta = p.meta.WithMetadata(meta)
	return p
}

// WithSecrets overrides the secret source (conformance tests).
func (p *Provider) WithSecrets(s port.SecretSource) *Provider {
	p.secretsOverride = s
	return p
}

// WithBundle overrides the config bundle fetcher (conformance tests).
func (p *Provider) WithBundle(c port.ConfigSource) *Provider {
	p.bundleOverride = c
	return p
}

// WithSecretsDir overrides the secrets directory (tests).
func (p *Provider) WithSecretsDir(dir string) *Provider {
	p.secrets = p.secrets.WithDir(dir)
	return p
}

// WithBundleDir overrides the bundle directory (tests).
func (p *Provider) WithBundleDir(dir string) *Provider {
	p.bundle = p.bundle.WithDir(dir)
	return p
}

// WithLeaseFile overrides the lease file path (tests).
func (p *Provider) WithLeaseFile(path string) *Provider {
	p.leases = p.leases.WithPath(path)
	return p
}

// Metadata returns container instance metadata.
func (p *Provider) Metadata(ctx context.Context) (port.InstanceMetadata, error) {
	return p.meta.Get(ctx)
}

// GetSecretValue resolves a secret from mounted files or the test override.
func (p *Provider) GetSecretValue(ctx context.Context, req port.SecretRequest) (string, error) {
	if p.secretsOverride != nil {
		return p.secretsOverride.GetSecretValue(ctx, req)
	}
	return p.secrets.GetSecretValue(ctx, req)
}

// FetchBundle copies the bundle from mounted local files or the test override.
func (p *Provider) FetchBundle(ctx context.Context, bundle model.ConfigBundle, destDir string) ([]string, error) {
	if p.bundleOverride != nil {
		return p.bundleOverride.FetchBundle(ctx, bundle, destDir)
	}
	return p.bundle.FetchBundle(ctx, bundle, destDir)
}

// ClaimOrdinal acquires an ordinal lease in the shared Compose lease file.
func (p *Provider) ClaimOrdinal(ctx context.Context, req port.ClaimRequest) (int, error) {
	return p.leases.ClaimOrdinal(ctx, req)
}

// RenewLease extends an ordinal lease in the shared Compose lease file.
func (p *Provider) RenewLease(ctx context.Context, req port.RenewRequest) error {
	return p.leases.RenewLease(ctx, req)
}

// ReleaseLease releases an ordinal lease in the shared Compose lease file.
func (p *Provider) ReleaseLease(ctx context.Context, req port.ReleaseRequest) error {
	return p.leases.ReleaseLease(ctx, req)
}

// ListActiveClaims returns active leases in the shared Compose lease file.
func (p *Provider) ListActiveClaims(ctx context.Context, req port.ListClaimsRequest) ([]port.Claim, error) {
	return p.leases.ListActiveClaims(ctx, req)
}

// PublishStatus writes a node status row.
func (p *Provider) PublishStatus(ctx context.Context, req port.NodeStatusPublish) error {
	return p.leases.PublishStatus(ctx, req)
}

// ListStatus reads fleet status rows.
func (p *Provider) ListStatus(ctx context.Context, req port.NodeStatusList) ([]port.NodeStatus, error) {
	return p.leases.ListStatus(ctx, req)
}

// SetCordon updates a node's cordoned flag.
func (p *Provider) SetCordon(ctx context.Context, req port.NodeCordonRequest) error {
	return p.leases.SetCordon(ctx, req)
}

// FetchRoleSpec reads a role-specific chinit.yaml from the local bundle dir.
func (p *Provider) FetchRoleSpec(ctx context.Context, req port.RoleSpecRequest) ([]byte, bool, error) {
	return p.bundle.FetchRoleSpec(ctx, req)
}

// AcquireRefreshLock takes the per-role refresh lock.
func (p *Provider) AcquireRefreshLock(ctx context.Context, req port.RefreshLockRequest) (bool, error) {
	return p.leases.AcquireRefreshLock(ctx, req)
}

// ReleaseRefreshLock releases the per-role refresh lock.
func (p *Provider) ReleaseRefreshLock(ctx context.Context, req port.RefreshLockRequest) error {
	return p.leases.ReleaseRefreshLock(ctx, req)
}

// SubmitTask implements port.FleetTaskStore.
func (p *Provider) SubmitTask(ctx context.Context, req port.FleetTaskSubmit) (bool, error) {
	return p.leases.SubmitTask(ctx, req)
}

// ClaimTask implements port.FleetTaskStore.
func (p *Provider) ClaimTask(ctx context.Context, req port.FleetTaskClaim) (bool, error) {
	return p.leases.ClaimTask(ctx, req)
}

// HeartbeatTask implements port.FleetTaskStore.
func (p *Provider) HeartbeatTask(ctx context.Context, req port.FleetTaskHeartbeat) (bool, error) {
	return p.leases.HeartbeatTask(ctx, req)
}

// CompleteTask implements port.FleetTaskStore.
func (p *Provider) CompleteTask(ctx context.Context, req port.FleetTaskVerdict) (bool, error) {
	return p.leases.CompleteTask(ctx, req)
}

// FailTask implements port.FleetTaskStore.
func (p *Provider) FailTask(ctx context.Context, req port.FleetTaskVerdict) (bool, error) {
	return p.leases.FailTask(ctx, req)
}

// ListFleetTasks implements port.FleetTaskStore.
func (p *Provider) ListFleetTasks(ctx context.Context, req port.FleetTaskList) ([]port.FleetTask, error) {
	return p.leases.ListFleetTasks(ctx, req)
}

// RetryTask implements port.FleetTaskStore.
func (p *Provider) RetryTask(ctx context.Context, req port.FleetTaskVerdict) (bool, error) {
	return p.leases.RetryTask(ctx, req)
}
