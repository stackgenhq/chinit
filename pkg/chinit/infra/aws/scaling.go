// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"

	"sort"
	"strconv"
	"time"

	awsapi "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// MetricStore is the AWS MetricReader: CloudWatch GetMetricStatistics
// over the EMF namespaces the fleet's agents push (the READ side of
// the CloudWatchPusher; the autoscale job queries the same metrics the
// alarms evaluate — at decision time, with live identity, instead of
// through the SNS alarm snapshot chain).
type MetricStore struct {
	region string
	client cwStatsClient
}

// cwStatsClient is the SDK surface the reader needs (one method so the
// conformance fake is a one-liner — the CloudWatchPusher pattern).
type cwStatsClient interface {
	GetMetricStatistics(ctx context.Context, params *cloudwatch.GetMetricStatisticsInput, optFns ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricStatisticsOutput, error)
}

// NewMetricStore builds the production reader.
func NewMetricStore(region string) *MetricStore {
	s := &MetricStore{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			s.client = cloudwatch.NewFromConfig(cfg)
		}
	}
	return s
}

// WithStatsClient is the conformance-suite injection seam.
func (s *MetricStore) WithStatsClient(c cwStatsClient) *MetricStore {
	s.client = c
	return s
}

// GetMetricStatistics implements port.MetricReader.
func (s *MetricStore) GetMetricStatistics(ctx context.Context, req port.MetricQuery) ([]float64, error) {
	if s.client == nil {
		return nil, fmt.Errorf("metric store: no client (region unset or config failed)")
	}
	if req.EndTime.IsZero() {
		req.EndTime = time.Now()
	}
	if req.StartTime.IsZero() {
		req.StartTime = req.EndTime.Add(-15 * time.Minute)
	}
	if req.Period <= 0 {
		req.Period = 60
	}
	var dims []cwtypes.Dimension
	for _, k := range sortedKeys(req.Dimensions) {
		dims = append(dims, cwtypes.Dimension{Name: awsapi.String(k), Value: awsapi.String(req.Dimensions[k])})
	}
	out, err := s.client.GetMetricStatistics(ctx, &cloudwatch.GetMetricStatisticsInput{
		Namespace:  awsapi.String(req.Namespace),
		MetricName: awsapi.String(req.MetricName),
		Dimensions: dims,
		StartTime:  awsapi.Time(req.StartTime),
		EndTime:    awsapi.Time(req.EndTime),
		Period:     awsapi.Int32(req.Period),
		Statistics: []cwtypes.Statistic{cwtypes.Statistic(req.Stat)},
	})
	if err != nil {
		return nil, fmt.Errorf("metric store: %w", err)
	}
	values := make([]float64, 0, len(out.Datapoints))
	for _, dp := range out.Datapoints {
		v := dp.Average
		switch cwtypes.Statistic(req.Stat) {
		case cwtypes.StatisticMaximum:
			v = dp.Maximum
		case cwtypes.StatisticMinimum:
			v = dp.Minimum
		case cwtypes.StatisticSum:
			v = dp.Sum
		}
		if v != nil {
			values = append(values, *v)
		}
	}
	// SDK Datapoints arrive newest-first; the service contract is
	// chronological — reverse once.
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
	return values, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EC2Resizer is the AWS InstanceResizer: stop/modify/start over the
// EC2 API (the keeper coordinator's per-instance step). Identity
// resolution is DescribeInstance-by-ID — the caller passes the id it
// resolved from the cluster's live inventory (claims/tag), never an
// apply-time snapshot.
type EC2Resizer struct {
	region string
	client ec2ResizeClient
}

// ec2ResizeClient is the SDK surface the resizer needs (the fake
// injection seam).
type ec2ResizeClient interface {
	DescribeInstances(ctx context.Context, params *ec2.DescribeInstancesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	StopInstances(ctx context.Context, params *ec2.StopInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	ModifyInstanceAttribute(ctx context.Context, params *ec2.ModifyInstanceAttributeInput, optFns ...func(*ec2.Options)) (*ec2.ModifyInstanceAttributeOutput, error)
	StartInstances(ctx context.Context, params *ec2.StartInstancesInput, optFns ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
}

// NewEC2Resizer builds the production resizer.
func NewEC2Resizer(region string) *EC2Resizer {
	r := &EC2Resizer{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			r.client = ec2.NewFromConfig(cfg)
		}
	}
	return r
}

// WithResizeClient is the conformance-suite injection seam.
func (r *EC2Resizer) WithResizeClient(c ec2ResizeClient) *EC2Resizer {
	r.client = c
	return r
}

// instanceState polls the instance's lifecycle state ("running",
// "stopped", ...) with a bounded wait — the stop and start steps'
// completion signal.
func (r *EC2Resizer) instanceState(ctx context.Context, instanceID string) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("ec2 resizer: no client (region unset or config failed)")
	}
	out, err := r.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return "", fmt.Errorf("ec2 resizer: describe %s: %w", instanceID, err)
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			return string(inst.State.Name), nil
		}
	}
	// Empty result: the id no longer exists (replaced between the
	// caller's resolution and now) — the caller's guard decides.
	return "", fmt.Errorf("ec2 resizer: %s has no describe result (terminated or replaced?)", instanceID)
}

// DescribeInstance implements port.InstanceResizer.
func (r *EC2Resizer) DescribeInstance(ctx context.Context, instanceID string) (*port.InstanceState, error) {
	if r.client == nil {
		return nil, fmt.Errorf("ec2 resizer: no client")
	}
	out, err := r.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2 resizer: describe %s: %w", instanceID, err)
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			return &port.InstanceState{
				InstanceID:   instanceID,
				InstanceType: string(inst.InstanceType),
				State:        string(inst.State.Name),
			}, nil
		}
	}
	return nil, fmt.Errorf("ec2 resizer: %s has no describe result (terminated or replaced?)", instanceID)
}

// waitState polls the lifecycle state until it matches or the deadline
// passes (the resizer's steps are synchronous: stop→modify→start must
// each complete before the next).
func (r *EC2Resizer) waitState(ctx context.Context, instanceID, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := r.instanceState(ctx, instanceID)
		if err != nil {
			return err
		}
		if state == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("ec2 resizer: %s did not reach %s within %s", instanceID, want, timeout)
}

// ChangeInstanceType implements port.InstanceResizer: the bounded
// stop → modify → start sequence. The stop step only runs when the
// instance is up (an already-stopped instance goes straight to
// modify); the start-back is the caller's choice (the keeper
// coordinator starts peers immediately and its own instance last).
func (r *EC2Resizer) ChangeInstanceType(ctx context.Context, req port.InstanceTypeChange) error {
	if r.client == nil {
		return fmt.Errorf("ec2 resizer: no client")
	}
	state, err := r.instanceState(ctx, req.InstanceID)
	if err != nil {
		return err
	}
	if state == "running" {
		if _, err := r.client.StopInstances(ctx, &ec2.StopInstancesInput{
			InstanceIds: []string{req.InstanceID},
		}); err != nil {
			return fmt.Errorf("ec2 resizer: stop %s: %w", req.InstanceID, err)
		}
		if err := r.waitState(ctx, req.InstanceID, "stopped", 5*time.Minute); err != nil {
			return err
		}
	}
	if _, err := r.client.ModifyInstanceAttribute(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId: awsapi.String(req.InstanceID),
		InstanceType: &ec2types.AttributeValue{
			Value: awsapi.String(req.TargetType),
		},
	}); err != nil {
		return fmt.Errorf("ec2 resizer: modify %s to %s: %w", req.InstanceID, req.TargetType, err)
	}
	if _, err := r.client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{req.InstanceID},
	}); err != nil {
		return fmt.Errorf("ec2 resizer: start %s: %w", req.InstanceID, err)
	}
	return r.waitState(ctx, req.InstanceID, "running", 5*time.Minute)
}

// StartInstance implements port.InstanceResizer and port.InstanceLifecycle.
func (r *EC2Resizer) StartInstance(ctx context.Context, instanceID string) error {
	if r.client == nil {
		return fmt.Errorf("ec2 resizer: no client")
	}
	if _, err := r.client.StartInstances(ctx, &ec2.StartInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("ec2 resizer: start %s: %w", instanceID, err)
	}
	return r.waitState(ctx, instanceID, "running", 5*time.Minute)
}

// FindInstanceByName implements port.InstanceLifecycle: resolves a
// non-terminated EC2 instance by its Name tag at decision time.
func (r *EC2Resizer) FindInstanceByName(ctx context.Context, nameTag string) (*port.InstanceState, error) {
	if r.client == nil {
		return nil, fmt.Errorf("ec2 resizer: no client")
	}
	out, err := r.client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		Filters: []ec2types.Filter{
			{
				Name:   awsapi.String("tag:Name"),
				Values: []string{nameTag},
			},
			{
				Name:   awsapi.String("instance-state-name"),
				Values: []string{"pending", "running", "stopping", "stopped"},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("ec2 resizer: find instance %s: %w", nameTag, err)
	}
	for _, res := range out.Reservations {
		for _, inst := range res.Instances {
			if inst.InstanceId == nil {
				continue
			}
			return &port.InstanceState{
				InstanceID:   *inst.InstanceId,
				InstanceType: string(inst.InstanceType),
				State:        string(inst.State.Name),
			}, nil
		}
	}
	return nil, fmt.Errorf("ec2 resizer: no non-terminated instance with tag Name=%s", nameTag)
}

// StopInstance implements port.InstanceLifecycle: initiates a stop on
// the target instance without polling for "stopped" (when called from
// inside backup-0 at the end of backup-daily, the OS shuts down as soon
// as StopInstances is accepted).
func (r *EC2Resizer) StopInstance(ctx context.Context, instanceID string) error {
	if r.client == nil {
		return fmt.Errorf("ec2 resizer: no client")
	}
	state, err := r.instanceState(ctx, instanceID)
	if err != nil {
		return err
	}
	if state == "stopped" || state == "stopping" {
		return nil
	}
	if _, err := r.client.StopInstances(ctx, &ec2.StopInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		return fmt.Errorf("ec2 resizer: stop %s: %w", instanceID, err)
	}
	return nil
}

// FleetRoller is the AWS FleetResizer: launch-template version bump +
// instance refresh with auto-rollback (the writers' vertical step —
// the same two calls the Lambda made, from the coordinator).
type FleetRoller struct {
	region string
	client asRollClient
}

// asRollClient is the SDK surface the roller needs (the fake seam).
// CreateLaunchTemplateVersion lives in the EC2 API, not autoscaling —
// the roller composes both clients behind one seam.
type asRollClient interface {
	CreateLaunchTemplateVersion(ctx context.Context, params *ec2.CreateLaunchTemplateVersionInput, optFns ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateVersionOutput, error)
	StartInstanceRefresh(ctx context.Context, params *autoscaling.StartInstanceRefreshInput, optFns ...func(*autoscaling.Options)) (*autoscaling.StartInstanceRefreshOutput, error)
	DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error)
}

// NewFleetRoller builds the production roller (EC2 client for LT
// versions + autoscaling client for refreshes).
func NewFleetRoller(region string) *FleetRoller {
	f := &FleetRoller{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			f.client = &rollPair{ec2c: ec2.NewFromConfig(cfg), asc: autoscaling.NewFromConfig(cfg)}
		}
	}
	return f
}

// rollPair adapts the two SDK clients to the one-method pair seam.
type rollPair struct {
	ec2c *ec2.Client
	asc  *autoscaling.Client
}

func (p *rollPair) CreateLaunchTemplateVersion(ctx context.Context, params *ec2.CreateLaunchTemplateVersionInput, optFns ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateVersionOutput, error) {
	return p.ec2c.CreateLaunchTemplateVersion(ctx, params, optFns...)
}

func (p *rollPair) StartInstanceRefresh(ctx context.Context, params *autoscaling.StartInstanceRefreshInput, optFns ...func(*autoscaling.Options)) (*autoscaling.StartInstanceRefreshOutput, error) {
	return p.asc.StartInstanceRefresh(ctx, params, optFns...)
}

func (p *rollPair) DescribeAutoScalingGroups(ctx context.Context, params *autoscaling.DescribeAutoScalingGroupsInput, optFns ...func(*autoscaling.Options)) (*autoscaling.DescribeAutoScalingGroupsOutput, error) {
	return p.asc.DescribeAutoScalingGroups(ctx, params, optFns...)
}

// WithRollClient is the conformance-suite injection seam.
func (f *FleetRoller) WithRollClient(c asRollClient) *FleetRoller {
	f.client = c
	return f
}

// DescribeFleetLaunchTemplate implements port.FleetResizer: the
// ASG's current launch-template binding (the coordinator versions
// the template the fleet actually runs — the spec names only the
// ASG, so template changes between spec renders cannot wedge the
// roll).
func (f *FleetRoller) DescribeFleetLaunchTemplate(ctx context.Context, asgName string) (*port.FleetLaunchTemplate, error) {
	if f.client == nil {
		return nil, fmt.Errorf("fleet roller: no client")
	}
	out, err := f.client.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{asgName},
	})
	if err != nil {
		return nil, fmt.Errorf("fleet roller: describe ASG %s: %w", asgName, err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return nil, fmt.Errorf("fleet roller: ASG %s not found (renamed or never created)", asgName)
	}
	lt := out.AutoScalingGroups[0].LaunchTemplate
	if lt == nil || lt.LaunchTemplateId == nil {
		// MixedInstancesPolicy shapes are not supported by the
		// vertical path (one type per fleet is the ladder's contract).
		return nil, fmt.Errorf("fleet roller: ASG %s has no launch-template binding (mixed-instances fleets are not on the vertical ladder)", asgName)
	}
	version := ""
	if lt.Version != nil {
		version = *lt.Version
	}
	return &port.FleetLaunchTemplate{LaunchTemplateID: *lt.LaunchTemplateId, Version: version}, nil
}

// CreateLaunchTemplateVersion implements port.FleetResizer: a NEW
// numbered version over $Latest with the stepped instance type
// (AutoRollback requires a pinned version — $Latest/$Default make
// StartInstanceRefresh reject the desired-configuration shape).
func (f *FleetRoller) CreateLaunchTemplateVersion(ctx context.Context, templateID, instanceType string) (int64, error) {
	if f.client == nil {
		return 0, fmt.Errorf("fleet roller: no client")
	}
	out, err := f.client.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{
		LaunchTemplateId: awsapi.String(templateID),
		SourceVersion:    awsapi.String("$Latest"),
		LaunchTemplateData: &ec2types.RequestLaunchTemplateData{
			InstanceType: ec2types.InstanceType(instanceType),
		},
	})
	if err != nil {
		return 0, fmt.Errorf("fleet roller: LT version %s -> %s: %w", templateID, instanceType, err)
	}
	if out.LaunchTemplateVersion == nil || out.LaunchTemplateVersion.VersionNumber == nil {
		return 0, fmt.Errorf("fleet roller: LT version created without a number")
	}
	return *out.LaunchTemplateVersion.VersionNumber, nil
}

// StartFleetRefresh implements port.FleetResizer: the rolling ASG
// refresh to the pinned version with MinHealthyPercentage and
// auto-rollback armed (the same DesiredConfiguration the live
// instance-refresh path requires — the API rejects AutoRollback with
// an empty desired config).
func (f *FleetRoller) StartFleetRefresh(ctx context.Context, req port.FleetRefreshRequest) error {
	if f.client == nil {
		return fmt.Errorf("fleet roller: no client")
	}
	_, err := f.client.StartInstanceRefresh(ctx, &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: awsapi.String(req.AutoScalingGroupName),
		Strategy:             astypes.RefreshStrategyRolling,
		DesiredConfiguration: &astypes.DesiredConfiguration{
			LaunchTemplate: &astypes.LaunchTemplateSpecification{
				LaunchTemplateId: awsapi.String(req.LaunchTemplateID),
				Version:          awsapi.String(req.Version),
			},
		},
		Preferences: &astypes.RefreshPreferences{
			InstanceWarmup:       awsapi.Int32(req.InstanceWarmup),
			MinHealthyPercentage: awsapi.Int32(req.MinHealthyPercentage),
			AutoRollback:         awsapi.Bool(true),
		},
	})
	if err != nil {
		return fmt.Errorf("fleet roller: refresh %s to LT v%s: %w", req.AutoScalingGroupName, req.Version, err)
	}
	return nil
}

// ============================================================================
// ScaleSignalsReader — the aggregated decision-input adapter
// ============================================================================

// SelfFencer marks the node's own EC2 instance unhealthy in its ASG
// (the identity-contested duplicate's exit — SetSelfUnhealthy flips
// the EC2 health check so the ASG terminates and replaces it).
type SelfFencer struct {
	region string
	client asFenceClient
}

// asFenceClient is the SDK surface the fencer needs (the fake seam).
type asFenceClient interface {
	SetInstanceHealth(ctx context.Context, params *autoscaling.SetInstanceHealthInput, optFns ...func(*autoscaling.Options)) (*autoscaling.SetInstanceHealthOutput, error)
}

// NewSelfFencer builds the production fencer.
func NewSelfFencer(region string) *SelfFencer {
	f := &SelfFencer{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			f.client = autoscaling.NewFromConfig(cfg)
		}
	}
	return f
}

// WithFenceClient injects a fake (tests).
func (f *SelfFencer) WithFenceClient(c asFenceClient) *SelfFencer {
	f.client = c
	return f
}

// SetSelfUnhealthy implements port.InstanceFencer.
func (f *SelfFencer) SetSelfUnhealthy(ctx context.Context, instanceID string) error {
	if f.client == nil {
		return fmt.Errorf("self fencer: no client")
	}
	_, err := f.client.SetInstanceHealth(ctx, &autoscaling.SetInstanceHealthInput{
		InstanceId:               awsapi.String(instanceID),
		HealthStatus:             awsapi.String("Unhealthy"),
		ShouldRespectGracePeriod: awsapi.Bool(false),
	})
	if err != nil {
		return fmt.Errorf("self fence %s: %w", instanceID, err)
	}
	return nil
}

// SignalsReader fans the autoscale decision inputs out over the
// MetricStore and applies the breach-side aggregate: the MINIMUM of
// the window's values (every sample must have breached — the alarm's
// evaluation_periods parity; one noisy minute never steps a fleet).
//
// Why the adapter owns the query plumbing: the service consumes
// domain.ScaleSignals and stays pure; this file is where the EMF
// namespaces/dimensions the fleet's agents push (ClickHouse/Keeper,
// ClickHouse/Node) are wired to domain names. Changing a metric name
// is an adapter edit; changing a threshold is a domain edit.
type SignalsReader struct {
	store *MetricStore
	// SampleMinutes/Breaches are the window shape (the alarm's
	// evaluation window parity).
	SampleMinutes int
	Breaches      int
}

// NewSignalsReader builds the reader over the store with the default
// window (15 min, 3 breaches — the alarms' defaults).
func NewSignalsReader(store *MetricStore) *SignalsReader {
	return &SignalsReader{store: store, SampleMinutes: 15, Breaches: 3}
}

// breachMin answers the breach-side aggregate of one metric series:
// the MINIMUM (least pressure) with the sample-count gate — fewer
// than Breaches datapoints in the window is "unreadable" (the
// decision never runs on thin data).
func (r *SignalsReader) breachMin(ctx context.Context, req port.MetricQuery) (float64, error) {
	if r.Breaches <= 0 {
		r.Breaches = 3
	}
	window := time.Duration(r.SampleMinutes) * time.Minute
	if window <= 0 {
		window = 15 * time.Minute
	}
	req.EndTime = time.Now().UTC()
	req.StartTime = req.EndTime.Add(-window)
	if req.Period <= 0 {
		req.Period = 60
	}
	vals, err := r.store.GetMetricStatistics(ctx, req)
	if err != nil {
		return 0, err
	}
	if len(vals) < r.Breaches {
		return 0, fmt.Errorf("only %d datapoints in the window (need %d)", len(vals), r.Breaches)
	}
	minVal := vals[0]
	for _, v := range vals[1:] {
		if v < minVal {
			minVal = v
		}
	}
	return minVal, nil
}

// ReadKeeperSignals implements port.ScaleSignalsReader: the ensemble's
// max latency + outstanding requests (ClickHouse/Keeper EMF, the
// alarms' exact metrics) and the coordinator's own CPU idle
// (ClickHouse/Node, the agent's host label).
func (r *SignalsReader) ReadKeeperSignals(ctx context.Context, clusterName string) (domain.ScaleSignals, error) {
	if r.store == nil {
		return domain.ScaleSignals{}, fmt.Errorf("signals reader: no metric store")
	}
	latency, err := r.breachMin(ctx, port.MetricQuery{
		Namespace:  "ClickHouse/Keeper",
		MetricName: "ClickHouseAsyncMetrics_KeeperMaxLatency",
		Dimensions: map[string]string{"ClusterName": clusterName, "NodeType": "keeper"},
		Stat:       "Maximum",
		Period:     60,
	})
	if err != nil {
		return domain.ScaleSignals{}, fmt.Errorf("keeper latency: %w", err)
	}
	outstanding, err := r.breachMin(ctx, port.MetricQuery{
		Namespace:  "ClickHouse/Keeper",
		MetricName: "ClickHouseMetrics_KeeperOutstandingRequests",
		Dimensions: map[string]string{"ClusterName": clusterName, "NodeType": "keeper"},
		Stat:       "Maximum",
		Period:     60,
	})
	if err != nil {
		return domain.ScaleSignals{}, fmt.Errorf("keeper outstanding: %w", err)
	}
	cpuIdle, err := r.breachMin(ctx, port.MetricQuery{
		Namespace:  "ClickHouse/Node",
		MetricName: "cpu_usage_idle",
		Dimensions: map[string]string{"cpu": "cpu-total"},
		Stat:       "Average",
		Period:     60,
	})
	if err != nil {
		// The keeper's own idle is a secondary signal: a read failure
		// degrades the field to 100 (never breaches alone) — the two
		// EMF signals carry the decision.
		cpuIdle = 100
	}
	return domain.ScaleSignals{
		KeeperMaxLatencyMs: latency,
		KeeperOutstanding:  outstanding,
		KeeperCPUIdlePct:   cpuIdle,
	}, nil
}

// ReadWriterSignals implements port.ScaleSignalsReader: the busiest
// writer's average CPU idle over the window. In CloudWatch
// GetMetricStatistics, synthetic "HostPattern" dimensions return zero
// datapoints against real AWS APIs, so when the pattern query yields no
// datapoints we fall back to the coordinator's own ClickHouse/Node
// series (host=writer-0, Period=60) and then the aggregate AWS/EC2
// ASG CPUUtilization series (<clusterName>-writers, converted to
// 100 - busy).
func (r *SignalsReader) ReadWriterSignals(ctx context.Context, clusterName string) (domain.ScaleSignals, error) {
	if r.store == nil {
		return domain.ScaleSignals{}, fmt.Errorf("signals reader: no metric store")
	}
	idle, err := r.breachMin(ctx, port.MetricQuery{
		Namespace:  "ClickHouse/Node",
		MetricName: "cpu_usage_idle",
		Dimensions: map[string]string{"cpu": "cpu-total", "HostPattern": "writer-*"},
		Stat:       "Average",
		Period:     300,
	})
	if err == nil {
		return domain.ScaleSignals{WriterCPUIdlePct: idle}, nil
	}
	if idleHost0, err0 := r.breachMin(ctx, port.MetricQuery{
		Namespace:  "ClickHouse/Node",
		MetricName: "cpu_usage_idle",
		Dimensions: map[string]string{"cpu": "cpu-total", "host": "writer-0"},
		Stat:       "Average",
		Period:     60,
	}); err0 == nil {
		return domain.ScaleSignals{WriterCPUIdlePct: idleHost0}, nil
	}
	if clusterName != "" {
		if busyASG, errASG := r.breachMin(ctx, port.MetricQuery{
			Namespace:  "AWS/EC2",
			MetricName: "CPUUtilization",
			Dimensions: map[string]string{"AutoScalingGroupName": clusterName + "-writers"},
			Stat:       "Maximum",
			Period:     60,
		}); errASG == nil {
			idlePct := 100.0 - busyASG
			if idlePct < 0 {
				idlePct = 0
			}
			return domain.ScaleSignals{WriterCPUIdlePct: idlePct}, nil
		}
	}
	return domain.ScaleSignals{}, fmt.Errorf("writer fleet idle: %w", err)
}

// ============================================================================
// CooldownStore — the pacing record adapter
// ============================================================================

// DynamoDBCooldown parks the autoscale pacing record in the cluster's
// claims/lease table: the key ("autoscale-keeper") can never collide
// with a hostname lease row (the writer-/reader- prefix contract),
// so one table serves both without a schema change. The record is
// GetItem/UpdateItem on the same client the claimer holds.
type DynamoDBCooldown struct {
	region string
	client cooldownDB
}

// cooldownDB is the SDK surface the cooldown needs (the fake seam).
type cooldownDB interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
}

// NewDynamoDBCooldown builds the production cooldown store.
func NewDynamoDBCooldown(region string) *DynamoDBCooldown {
	c := &DynamoDBCooldown{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			client := dynamodb.NewFromConfig(cfg)
			c.client = client
		}
	}
	return c
}

// WithClient is the conformance-suite injection seam.
func (c *DynamoDBCooldown) WithClient(db cooldownDB) *DynamoDBCooldown {
	c.client = db
	return c
}

// PeekCooldown implements port.CooldownStore: the record's expiry or
// absence (absent table in the request answers absent record — the
// no-table fleet runs unpaced, the ladder still bounds).
func (c *DynamoDBCooldown) PeekCooldown(ctx context.Context, req port.CooldownRequest) (int64, bool) {
	if c.client == nil || req.Table == "" {
		return 0, false
	}
	out, err := c.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: awsapi.String(req.Table),
		Key: map[string]ddbtypes.AttributeValue{
			"hostname": &ddbtypes.AttributeValueMemberS{Value: req.Key},
		},
	})
	if err != nil || len(out.Item) == 0 {
		return 0, false
	}
	var row struct {
		ExpiresAt string `json:"expires_at"`
	}
	raw, ok := out.Item["expires_at"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case *ddbtypes.AttributeValueMemberN:
		row.ExpiresAt = v.Value
	default:
		return 0, false
	}
	exp, err := strconv.ParseInt(row.ExpiresAt, 10, 64)
	if err != nil {
		return 0, false
	}
	return exp, true
}

// MarkCooldown implements port.CooldownStore: the record's upsert
// (minutes from now; an existing record extends — the latest step
// always owns the window).
func (c *DynamoDBCooldown) MarkCooldown(ctx context.Context, req port.CooldownRequest) error {
	if c.client == nil || req.Table == "" {
		return fmt.Errorf("cooldown: no client or table")
	}
	expiry := time.Now().Add(time.Duration(req.Minutes) * time.Minute).Unix()
	_, err := c.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: awsapi.String(req.Table),
		Key: map[string]ddbtypes.AttributeValue{
			"hostname": &ddbtypes.AttributeValueMemberS{Value: req.Key},
		},
		UpdateExpression: awsapi.String("SET expires_at = :exp"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":exp": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(expiry, 10)},
		},
	})
	if err != nil {
		return fmt.Errorf("cooldown: mark %s/%s: %w", req.Table, req.Key, err)
	}
	return nil
}

// ============================================================================
// OrphanSweeper — the account-hygiene census adapter
// ============================================================================

// OrphanSweeper is the AWS port.OrphanSweeper: the untagged-resource
// census over DescribeVolumes/DescribeAddresses/DescribeSnapshots.
// Deletion (ec2:DeleteVolume) happens only for the volume category and
// only past the age threshold; the report-only categories never
// mutate.
type sweeper struct {
	region string
	client sweepClient
}

// sweepClient is the SDK surface the sweeper needs (the fake seam).
type sweepClient interface {
	DescribeVolumes(ctx context.Context, params *ec2.DescribeVolumesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error)
	DeleteVolume(ctx context.Context, params *ec2.DeleteVolumeInput, optFns ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	DescribeAddresses(ctx context.Context, params *ec2.DescribeAddressesInput, optFns ...func(*ec2.Options)) (*ec2.DescribeAddressesOutput, error)
	DescribeSnapshots(ctx context.Context, params *ec2.DescribeSnapshotsInput, optFns ...func(*ec2.Options)) (*ec2.DescribeSnapshotsOutput, error)
}

// NewOrphanSweeper builds the production sweeper.
func NewOrphanSweeper(region string) *sweeper {
	s := &sweeper{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			c := ec2.NewFromConfig(cfg)
			s.client = c
		}
	}
	return s
}

// WithSweepClient is the conformance-suite injection seam.
func (s *sweeper) WithSweepClient(c sweepClient) *sweeper {
	s.client = c
	return s
}

// untagged answers whether the tag set is empty (the ownership
// contract: untagged = unowned; tagged = never touched).
func untagged(tags []ec2types.Tag) bool { return len(tags) == 0 }

// SweepOrphans implements port.OrphanSweeper.
func (s *sweeper) SweepOrphans(ctx context.Context, req port.OrphanSweepRequest) (*port.OrphanSweepResult, error) {
	if s.client == nil {
		return nil, fmt.Errorf("orphan sweeper: no client (region unset or config failed)")
	}
	if req.AgeThreshold <= 0 {
		req.AgeThreshold = 24 * time.Hour
	}
	cutoff := time.Now().Add(-req.AgeThreshold)
	result := &port.OrphanSweepResult{}

	// Volumes: unattached + untagged + past the age threshold.
	vols, err := s.client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []ec2types.Filter{
			{Name: awsapi.String("status"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("orphan sweeper: describe volumes: %w", err)
	}
	for _, v := range vols.Volumes {
		if !untagged(v.Tags) || v.CreateTime == nil || v.CreateTime.After(cutoff) {
			continue
		}
		id := ""
		if v.VolumeId != nil {
			id = *v.VolumeId
		}
		result.VolumesFound = append(result.VolumesFound, id)
		if req.DryRun {
			continue
		}
		if _, err := s.client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{
			VolumeId: v.VolumeId,
		}); err != nil {
			// One stuck volume never fails the sweep; it reports.
			logger.GetLogger(ctx).Warn("orphan sweeper: delete failed (reports in findings)", "volume", id, "err", err.Error())
			continue
		}
		result.VolumesDeleted = append(result.VolumesDeleted, id)
	}

	// EIPs: report only.
	addrs, err := s.client.DescribeAddresses(ctx, &ec2.DescribeAddressesInput{})
	if err == nil {
		for _, a := range addrs.Addresses {
			if a.AssociationId == nil {
				if a.PublicIp != nil {
					result.EIPsUnassociated = append(result.EIPsUnassociated, *a.PublicIp)
				}
			}
		}
	}

	// Snapshots: report only (untagged + past the threshold).
	snaps, err := s.client.DescribeSnapshots(ctx, &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
	})
	if err == nil {
		for _, s := range snaps.Snapshots {
			if untagged(s.Tags) && s.StartTime != nil && s.StartTime.Before(cutoff) {
				if s.SnapshotId != nil {
					result.SnapshotsUntagged = append(result.SnapshotsUntagged, *s.SnapshotId)
				}
			}
		}
	}
	return result, nil
}
