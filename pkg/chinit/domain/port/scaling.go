// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
)

// MetricReader queries the cloud's metric store for a metric over a time
// window.
type MetricReader interface {
	// GetMetricStatistics answers the statistic's values over the
	// window, chronological. The caller owns the breach-side
	// aggregate (every sample must breach — evaluation_periods
	// parity, never one noisy minute).
	GetMetricStatistics(ctx context.Context, req MetricQuery) ([]float64, error)
}

// MetricQuery names one metric over a window (the cloud wire shape).
type MetricQuery struct {
	// Namespace is the metric namespace ("ClickHouse/Keeper").
	Namespace string
	// MetricName is the metric's name.
	MetricName string
	// Dimensions narrow the metric (ANDed); a HostPattern key is the
	// provider's fan-out form (CloudWatch SEARCH's host-prefix).
	Dimensions map[string]string
	// Stat is the per-period aggregation ("Average"/"Maximum"/...).
	Stat string
	// Period is the aggregation window seconds.
	Period int32
	// StartTime/EndTime bound the window; zero EndTime = now, zero
	// StartTime = now-15m (the service always sets both).
	StartTime time.Time
	EndTime   time.Time
}

// InstanceResizer is the same-instance vertical step (static-identity
// fleets: keepers). The COORDINATOR drives it — never a per-node
// free-for-all (the coordinator contract: domain/scaling.go).
type InstanceResizer interface {
	// DescribeInstance answers an instance's type and state at
	// decision time (identity resolved live by the caller — claims,
	// tags, or self; never an apply-time snapshot).
	DescribeInstance(ctx context.Context, instanceID string) (*InstanceState, error)
	// ChangeInstanceType runs the bounded stop → modify → start
	// sequence on one instance. The coordinator calls it for peers
	// and (self-last, pre-sequenced) for itself.
	ChangeInstanceType(ctx context.Context, req InstanceTypeChange) error
	// StartInstance starts a stopped instance (the start-back when a
	// modify leaves it stopped mid-sequence).
	StartInstance(ctx context.Context, instanceID string) error
}

// InstanceState is an instance's compute identity at decision time
// (the wire shape; domain decisions consume it, never hold it).
type InstanceState struct {
	// InstanceID is the cloud's instance identifier.
	InstanceID string
	// InstanceType is the current machine type.
	InstanceType string
	// State is the lifecycle state ("running"/"stopped").
	State string
}

// InstanceFencer is the identity-contested node's exit: mark its own
// EC2 instance unhealthy so the ASG replaces it. The self-fence runs
// when lease-renew proves a LIVE replacement holds the node's own
// ordinal (the split-brain shape: two nodes serving as one name —
// the duplicate must stop, and the ASG is the only thing that can
// reclaim the machine).
type InstanceFencer interface {
	// SetSelfUnhealthy marks this instance unhealthy in its ASG (the
	// EC2 health check flips InService→Terminating; the replacement
	// boots and claims cleanly).
	SetSelfUnhealthy(ctx context.Context, instanceID string) error
}

// InstanceLifecycle controls the on-demand start/stop lifecycle of a
// named static role instance (the backup replica's StatefulSet 0<->1
// pattern: the ordinal-0 writer wakes backup-0 on the backup schedule,
// and backup-0 stops itself once clickhouse-backup completes).
type InstanceLifecycle interface {
	// FindInstanceByName resolves a non-terminated instance by its Name
	// tag ("<cluster>-backup-0") at decision time.
	FindInstanceByName(ctx context.Context, nameTag string) (*InstanceState, error)
	// StartInstance starts a stopped instance and waits for running.
	StartInstance(ctx context.Context, instanceID string) error
	// StopInstance initiates an instance stop (idempotent when already
	// stopping/stopped; does not block waiting for stopped when called
	// from inside the instance being stopped).
	StopInstance(ctx context.Context, instanceID string) error
}

// InstanceTypeChange asks the resizer to step one instance.
type InstanceTypeChange struct {
	// InstanceID is the instance to change.
	InstanceID string
	// TargetType is the ladder's bounded step.
	TargetType string
}

// FleetResizer is the ASG-borne fleet's vertical step: bump the
// launch template to a new type and roll the fleet onto it (the
// writers' path; the same two calls the replaced Lambda made, from
// the coordinator, with AutoRollback armed).
type FleetResizer interface {
	// DescribeFleetLaunchTemplate answers the ASG's CURRENT launch
	// template (id + version). The coordinator versions the template
	// the fleet actually runs — the spec names only the ASG, so a
	// template change between spec renders cannot wedge the roll.
	DescribeFleetLaunchTemplate(ctx context.Context, asgName string) (*FleetLaunchTemplate, error)
	// CreateLaunchTemplateVersion adds a numbered version with the
	// new instance type over $Latest, returning the version number
	// (AutoRollback requires a pinned version — $Latest/$Default are
	// rejected by StartInstanceRefresh's desired-config shape).
	CreateLaunchTemplateVersion(ctx context.Context, templateID, instanceType string) (int64, error)
	// StartFleetRefresh rolls the ASG to the pinned version with
	// health-percentage auto-rollback armed.
	StartFleetRefresh(ctx context.Context, req FleetRefreshRequest) error
}

// FleetLaunchTemplate is the ASG's current template binding.
type FleetLaunchTemplate struct {
	// LaunchTemplateID is the template's resource id (lt-...).
	LaunchTemplateID string
	// Version is the template version the ASG pins ("2", "$Latest").
	Version string
}

// FleetRefreshRequest names one fleet roll (the wire shape).
type FleetRefreshRequest struct {
	// AutoScalingGroupName is the ASG to roll.
	AutoScalingGroupName string
	// LaunchTemplateID is the template the version belongs to.
	LaunchTemplateID string
	// Version is the numbered LT version created for the new type.
	Version string
	// InstanceWarmup is the per-instance settle seconds (the boot +
	// chinit convergence budget).
	InstanceWarmup int32
	// MinHealthyPercentage holds the fleet through the roll (100 =
	// never take the last healthy instance down).
	MinHealthyPercentage int32
}

// CooldownStore is the autoscale pacing record: a keyed expiry the
// coordinator writes after a step and reads before the next decision
// (the replaced Lambda's DynamoDB cooldown — same semantics, minus
// the function). The record rides the cluster's existing claims/lease
// table under a key hostname lease rows can never produce.
type CooldownStore interface {
	// PeekCooldown answers the record's expiry (unix seconds) and
	// whether a record exists.
	PeekCooldown(ctx context.Context, req CooldownRequest) (int64, bool)
	// MarkCooldown writes the record (Minutes from now).
	MarkCooldown(ctx context.Context, req CooldownRequest) error
}

// CooldownRequest names one cooldown record.
type CooldownRequest struct {
	// Table is the claims/lease table the record rides.
	Table string
	// Key is the record's key ("autoscale-keeper").
	Key string
	// Minutes is the cooldown window (MarkCooldown only).
	Minutes int
}

// ScaleSignalsReader is the aggregated decision-input port: the
// service asks for one role's signals and the adapter fans out the
// metric queries and applies the breach-side aggregate (min over the
// window — every sample breached). Splitting this from MetricReader
// keeps the SERVICE free of per-metric query plumbing: the domain
// consumes domain.ScaleSignals, the adapter owns the wire.
type ScaleSignalsReader interface {
	// ReadKeeperSignals answers the ensemble's signals over the
	// window (the adapter knows the namespaces/dimensions the
	// fleet's agents push).
	ReadKeeperSignals(ctx context.Context, clusterName string) (domain.ScaleSignals, error)
	// ReadWriterSignals answers the writers' fleet signal (the
	// busiest writer's average idle).
	ReadWriterSignals(ctx context.Context, clusterName string) (domain.ScaleSignals, error)
}
