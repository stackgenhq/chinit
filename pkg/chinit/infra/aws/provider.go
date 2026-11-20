// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// Provider is the AWS entry in chinit's provider registry. It composes
// the mandatory capabilities and implements the optional ones (ordinal
// claims, DNS, spot, metrics) — the registry constructor wires the real
// clients.
type Provider struct {
	region string
	spec   *model.BootSpec

	meta            *IMDSMetadata
	secrets         *SecretsManagerSource
	secretsOverride port.SecretSource
	bundle          *S3BundleSource
	bundleOverride  port.ConfigSource
	claims          *DynamoDBClaimer
	lister          *DynamoDBClaimLister
	dns             *Route53Registrar
	spot            *SpotWatcher
	metrics         *CloudWatchPusher
	volumes         *VolumeGrower
	releaser        *VolumeReleaser
	metricStore     *MetricStore
	resizer         *EC2Resizer
	roller          *FleetRoller
	fencer          *SelfFencer
	signals         *SignalsReader
	cooldown        *DynamoDBCooldown
	roleSpecs       *S3RoleSpecSource
	refreshLock     *DynamoDBRefreshLock
	sweeper         *sweeper
	fleetTasks      *DynamoDBFleetTaskStore
	status          *DynamoDBStatusPublisher
}

// Compile-time capability checks: the factory relies on these.
var (
	_ port.Provider           = (*Provider)(nil)
	_ port.OrdinalClaimer     = (*Provider)(nil)
	_ port.LeaseLister        = (*Provider)(nil)
	_ port.DNSRegistrar       = (*Provider)(nil)
	_ port.SpotSignals        = (*Provider)(nil)
	_ port.MetricPusher       = (*Provider)(nil)
	_ port.VolumeGrower       = (*Provider)(nil)
	_ port.MetricReader       = (*Provider)(nil)
	_ port.InstanceResizer    = (*Provider)(nil)
	_ port.InstanceLifecycle  = (*Provider)(nil)
	_ port.FleetResizer       = (*Provider)(nil)
	_ port.InstanceFencer     = (*Provider)(nil)
	_ port.ScaleSignalsReader = (*Provider)(nil)
	_ port.CooldownStore      = (*Provider)(nil)
	_ port.RoleSpecSource     = (*Provider)(nil)
	_ port.RefreshLock        = (*Provider)(nil)
	_ port.OrphanSweeper      = (*Provider)(nil)
	_ port.VolumeReleaser     = (*Provider)(nil)
	_ port.FleetTaskStore     = (*Provider)(nil)
	_ port.StatusPublisher    = (*Provider)(nil)
)

// New builds the AWS provider with real SDK clients. The metadata
// client is injectable for the conformance suite (fake IMDS).
func New(cfg port.Config) port.Provider {
	region := cfg.Region
	if region == "" && cfg.Spec != nil {
		region = cfg.Spec.Region
	}
	p := &Provider{
		region: region,
		spec:   cfg.Spec,
	}
	p.meta = NewIMDSMetadata()
	p.secrets = NewSecretsManagerSource(region)
	p.bundle = NewS3BundleSource(region)
	p.claims = NewDynamoDBClaimer(region)
	p.lister = NewDynamoDBClaimLister(region)
	p.dns = NewRoute53Registrar(region)
	p.spot = NewSpotWatcher()
	p.metrics = NewCloudWatchPusher(region)
	p.volumes = NewVolumeGrower(region)
	p.releaser = NewVolumeReleaser(region)
	p.metricStore = NewMetricStore(region)
	p.resizer = NewEC2Resizer(region)
	p.roller = NewFleetRoller(region)
	p.fencer = NewSelfFencer(region)
	p.signals = NewSignalsReader(p.metricStore)
	p.cooldown = NewDynamoDBCooldown(region)
	p.roleSpecs = NewS3RoleSpecSource(region)
	p.refreshLock = NewDynamoDBRefreshLock(region)
	p.sweeper = NewOrphanSweeper(region)
	p.fleetTasks = NewDynamoDBFleetTaskStore(region)
	p.status = NewDynamoDBStatusPublisher(region)
	return p
}

// FetchRoleSpec implements port.RoleSpecSource (delegated).
func (p *Provider) FetchRoleSpec(ctx context.Context, req port.RoleSpecRequest) ([]byte, bool, error) {
	return p.roleSpecs.FetchRoleSpec(ctx, req)
}

// AcquireRefreshLock implements port.RefreshLock (delegated).
func (p *Provider) AcquireRefreshLock(ctx context.Context, req port.RefreshLockRequest) (bool, error) {
	return p.refreshLock.AcquireRefreshLock(ctx, req)
}

// ReleaseRefreshLock implements port.RefreshLock (delegated).
func (p *Provider) ReleaseRefreshLock(ctx context.Context, req port.RefreshLockRequest) error {
	return p.refreshLock.ReleaseRefreshLock(ctx, req)
}

// GetMetricStatistics implements port.MetricReader (delegated to the
// store client — the provider surface keeps one capability object).
func (p *Provider) GetMetricStatistics(ctx context.Context, req port.MetricQuery) ([]float64, error) {
	return p.metricStore.GetMetricStatistics(ctx, req)
}

// DescribeInstance implements port.InstanceResizer (delegated).
func (p *Provider) DescribeInstance(ctx context.Context, instanceID string) (*port.InstanceState, error) {
	return p.resizer.DescribeInstance(ctx, instanceID)
}

// ChangeInstanceType implements port.InstanceResizer (delegated).
func (p *Provider) ChangeInstanceType(ctx context.Context, req port.InstanceTypeChange) error {
	return p.resizer.ChangeInstanceType(ctx, req)
}

// StartInstance implements port.InstanceResizer (delegated).
func (p *Provider) StartInstance(ctx context.Context, instanceID string) error {
	return p.resizer.StartInstance(ctx, instanceID)
}

// FindInstanceByName implements port.InstanceLifecycle (delegated).
func (p *Provider) FindInstanceByName(ctx context.Context, nameTag string) (*port.InstanceState, error) {
	return p.resizer.FindInstanceByName(ctx, nameTag)
}

// StopInstance implements port.InstanceLifecycle (delegated).
func (p *Provider) StopInstance(ctx context.Context, instanceID string) error {
	return p.resizer.StopInstance(ctx, instanceID)
}

// DescribeFleetLaunchTemplate implements port.FleetResizer (delegated).
func (p *Provider) DescribeFleetLaunchTemplate(ctx context.Context, asgName string) (*port.FleetLaunchTemplate, error) {
	return p.roller.DescribeFleetLaunchTemplate(ctx, asgName)
}

// CreateLaunchTemplateVersion implements port.FleetResizer (delegated).
func (p *Provider) CreateLaunchTemplateVersion(ctx context.Context, templateID, instanceType string) (int64, error) {
	return p.roller.CreateLaunchTemplateVersion(ctx, templateID, instanceType)
}

// StartFleetRefresh implements port.FleetResizer (delegated).
func (p *Provider) StartFleetRefresh(ctx context.Context, req port.FleetRefreshRequest) error {
	return p.roller.StartFleetRefresh(ctx, req)
}

// SetSelfUnhealthy implements port.InstanceFencer (delegated).
func (p *Provider) SetSelfUnhealthy(ctx context.Context, instanceID string) error {
	return p.fencer.SetSelfUnhealthy(ctx, instanceID)
}

// ReadKeeperSignals implements port.ScaleSignalsReader (delegated).
func (p *Provider) ReadKeeperSignals(ctx context.Context, clusterName string) (domain.ScaleSignals, error) {
	return p.signals.ReadKeeperSignals(ctx, clusterName)
}

// ReadWriterSignals implements port.ScaleSignalsReader (delegated).
func (p *Provider) ReadWriterSignals(ctx context.Context, clusterName string) (domain.ScaleSignals, error) {
	return p.signals.ReadWriterSignals(ctx, clusterName)
}

// PeekCooldown implements port.CooldownStore (delegated).
func (p *Provider) PeekCooldown(ctx context.Context, req port.CooldownRequest) (int64, bool) {
	return p.cooldown.PeekCooldown(ctx, req)
}

// MarkCooldown implements port.CooldownStore (delegated).
func (p *Provider) MarkCooldown(ctx context.Context, req port.CooldownRequest) error {
	return p.cooldown.MarkCooldown(ctx, req)
}

// SweepOrphans implements port.OrphanSweeper (delegated).
func (p *Provider) SweepOrphans(ctx context.Context, req port.OrphanSweepRequest) (*port.OrphanSweepResult, error) {
	return p.sweeper.SweepOrphans(ctx, req)
}

// ListOwnVolumes implements port.VolumeReleaser (delegated).
func (p *Provider) ListOwnVolumes(ctx context.Context, req port.VolumeReleaseRequest) ([]port.AttachedVolume, error) {
	return p.releaser.ListOwnVolumes(ctx, req)
}

// ReleaseVolume implements port.VolumeReleaser (delegated).
func (p *Provider) ReleaseVolume(ctx context.Context, volumeID string) error {
	return p.releaser.ReleaseVolume(ctx, volumeID)
}

// FleetTaskStore implementation (delegated): the epoch-fenced task
// rows live beside the claims in the same table.

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

// WithSecrets overrides the secret source (conformance tests inject a
// fake; production uses the instance-role Secrets Manager source).
func (p *Provider) WithSecrets(s port.SecretSource) port.Provider {
	p.secretsOverride = s
	return p
}

// WithBundle overrides the config-bundle fetcher (conformance tests).
func (p *Provider) WithBundle(c port.ConfigSource) port.Provider {
	p.bundleOverride = c
	return p
}

// Name is the registry key.
func (p *Provider) Name() string { return "aws" }

// Metadata probes EC2 IMDSv2.
func (p *Provider) Metadata(ctx context.Context) (port.InstanceMetadata, error) {
	m, err := p.meta.Get(ctx)
	if err != nil {
		return port.InstanceMetadata{}, err
	}
	m.Provider = p.Name()
	return m, nil
}

// GetSecretValue resolves a Secrets Manager entry (the adapter), or
// the test-injected override when present.
func (p *Provider) GetSecretValue(ctx context.Context, request port.SecretRequest) (string, error) {
	if p.secretsOverride != nil {
		return p.secretsOverride.GetSecretValue(ctx, request)
	}
	return p.secrets.GetSecretValue(ctx, request)
}

// FetchBundle downloads the config bundle from S3 with the SHA gate.
func (p *Provider) FetchBundle(ctx context.Context, bundle model.ConfigBundle, destDir string) ([]string, error) {
	if p.bundleOverride != nil {
		return p.bundleOverride.FetchBundle(ctx, bundle, destDir)
	}
	return p.bundle.FetchBundle(ctx, bundle, destDir)
}

// ClaimOrdinal claims a reader ordinal via DynamoDB.
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
// inventory source for remote_servers and the -host flags).
func (p *Provider) ListActiveClaims(ctx context.Context, req port.ListClaimsRequest) ([]port.Claim, error) {
	return p.lister.ListActiveClaims(ctx, req)
}

// ResolveZoneID finds the Route53 private zone by exact DNS name.
func (p *Provider) ResolveZoneID(ctx context.Context, zoneName string) (string, error) {
	return p.dns.ResolveZoneID(ctx, zoneName)
}

// UpsertARecord creates or replaces the node's private A record.
func (p *Provider) UpsertARecord(ctx context.Context, req port.ARecordRequest) error {
	return p.dns.UpsertARecord(ctx, req)
}

// DeleteARecord removes the node's A record.
func (p *Provider) DeleteARecord(ctx context.Context, req port.ARecordRequest) error {
	return p.dns.DeleteARecord(ctx, req)
}

// Watch polls the IMDS spot interruption endpoint until the notice
// arrives, then delivers it once.
func (p *Provider) Watch(ctx context.Context) <-chan port.InterruptionNotice {
	return p.spot.Watch(ctx)
}

// PutMetric publishes one self-healing datapoint (keeper_up, resize
// outcomes) — the MetricPusher capability the recurring jobs probe
// for before degrading to log-only.
func (p *Provider) PutMetric(ctx context.Context, req port.MetricPushRequest) error {
	return p.metrics.PutMetric(ctx, req)
}

// DescribeOwnVolume returns the data volume attached to this instance.
func (p *Provider) DescribeOwnVolume(ctx context.Context, req port.VolumeGrowRequest) (*port.VolumeInfo, error) {
	return p.volumes.DescribeOwnVolume(ctx, req)
}

// GrowVolume extends this instance's volume online (no-op at target).
func (p *Provider) GrowVolume(ctx context.Context, volumeID string, targetSizeGB int32) (bool, error) {
	return p.volumes.GrowVolume(ctx, volumeID, targetSizeGB)
}

// WaitForModification blocks until the volume modification completes.
func (p *Provider) WaitForModification(ctx context.Context, volumeID string) error {
	return p.volumes.WaitForModification(ctx, volumeID)
}

// awsConfig loads the SDK config with the provider's region pin. Every
// client constructor calls this — one place for the credential chain
// (instance role) and retry-mode settings.
func awsConfig(ctx context.Context, region string) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(ctx, func(opts *config.LoadOptions) error {
		if region != "" {
			opts.Region = region
		}
		return nil
	})
	if err != nil {
		return aws.Config{}, fmt.Errorf("load AWS config: %w", err)
	}
	return cfg, nil
}

// StatusPublisher implementation (delegated): the node-status rows
// ride the claims table's status/ key space.

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
