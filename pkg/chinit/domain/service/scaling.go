// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// AutoscaleService is the vertical-scaling use case: the COORDINATOR
// that decides one step per invocation and executes it over the ports.
// It replaces the vertical-autoscaler Lambda (SNS alarm → function →
// AWS API glue) with decision-time reads and live identity — the
// node is the authority on itself.
//
// The service is pure over the ports: signals arrive through
// ScaleSignalsReader, identity through InstanceResizer/FleetResizer,
// pacing through CooldownStore — the fakes drive every branch in the
// suite; no cloud appears here (the DDD boundary the rest of this
// package draws: the SchemaService over its SchemaRunner).
//
// Coordinator contract (why ONE node decides — domain/scaling.go):
// keeper-0 for keepers (static identity; the multi-keeper step order
// is peers-first, self-last), the ordinal-0 maintenance holder for
// writers. The CALLER (app layer) holds the designation gate — the
// service never re-derives who it runs on; it executes the
// coordinator's will for the identity it is handed.
type AutoscaleService struct {
	signals port.ScaleSignalsReader
	keeper  port.InstanceResizer
	fleet   port.FleetResizer
	cool    port.CooldownStore
}

// NewAutoscaleService builds the service over its ports. Any nil port
// is a construction error — the caller reports the missing capability
// as the skip reason (a half-built service that crashes mid-decision
// is the failure shape the probes exist to prevent).
func NewAutoscaleService(signals port.ScaleSignalsReader, keeper port.InstanceResizer, fleet port.FleetResizer, cool port.CooldownStore) (*AutoscaleService, error) {
	if signals == nil {
		return nil, fmt.Errorf("autoscale service: signals reader is nil")
	}
	if cool == nil {
		return nil, fmt.Errorf("autoscale service: cooldown store is nil")
	}
	s := &AutoscaleService{signals: signals, cool: cool}
	if keeper != nil {
		s.keeper = keeper
	}
	if fleet != nil {
		s.fleet = fleet
	}
	return s, nil
}

// AutoscaleRequest is one coordinator invocation.
type AutoscaleRequest struct {
	// ClusterName is the cluster the metrics carry (the EMF
	// dimension every alarm and dashboard keys on).
	ClusterName string
	// Coordinator is the designation this node holds this tick
	// (the app layer's gate output; CoordinatorNone never reaches
	// the service — the CLI no-ops first).
	Coordinator domain.CoordinatorRole
	// Cooldown is the pacing request (Table + Key role-derived).
	Cooldown port.CooldownRequest
	// KeeperIdentity is the keeper coordinator's own instance (the
	// self-resize target; required for CoordinatorKeeper).
	KeeperInstanceID string
	// Ladder is the role's bounded ladder (spec-rendered).
	Ladder domain.TypeLadder
	// Fleet identifies the writers' ASG + launch template (the roll
	// target; required for CoordinatorWriter).
	Fleet port.FleetRefreshRequest
}

// AutoscaleResult is the invocation's outcome (the CLI prints it;
// the suite asserts it — a decision is data).
type AutoscaleResult struct {
	// Decision is what the tick concluded (breach, step, or no-op
	// with the reason line).
	Decision domain.ScaleDecision
	// Skipped is the pre-decision no-op (cooldown active, no
	// ladder, ceiling reached, unreadable signals) with the reason.
	Skipped string
}

// Autoscale runs one coordinator tick: read signals, judge breach,
// take at most ONE bounded step, pace it with the cooldown.
func (s *AutoscaleService) Autoscale(ctx context.Context, req AutoscaleRequest) (*AutoscaleResult, error) {
	if req.Coordinator == domain.CoordinatorNone {
		return nil, fmt.Errorf("autoscale: CoordinatorNone has no decision to run (the caller gates first)")
	}
	// Pacing first: an active cooldown blocks the decision entirely
	// (the Lambda's DynamoDB cooldown, same semantics — the ladder
	// bounds the runaway, the cooldown paces the steps). The record
	// key is the SERVICE's: "autoscale-<role>" over the caller's
	// table — the caller's own Cooldown.Key is not consulted (the
	// peek and the mark must agree, and the key must never collide
	// with a hostname lease row).
	coolReq := port.CooldownRequest{Table: req.Cooldown.Table, Key: "autoscale-" + string(req.Coordinator)}
	if exp, ok := s.cool.PeekCooldown(ctx, coolReq); ok && time.Now().Unix() < exp {
		return &AutoscaleResult{Skipped: fmt.Sprintf("cooldown active (%ds left)", exp-time.Now().Unix())}, nil
	}

	thresholds := domain.DefaultScaleThresholds()
	switch req.Coordinator {
	case domain.CoordinatorKeeper:
		return s.keeperStep(ctx, req, thresholds)
	case domain.CoordinatorWriter:
		return s.writerStep(ctx, req, thresholds)
	default:
		return nil, fmt.Errorf("autoscale: coordinator role %q not recognized", req.Coordinator)
	}
}

// keeperStep decides the keeper's self-resize: any keeper signal
// breached → one bounded ladder step on the coordinator's own
// instance (the keeper's identity is static — IMDS-resolved by the
// caller; never a snapshot).
func (s *AutoscaleService) keeperStep(ctx context.Context, req AutoscaleRequest, thresholds domain.ScaleThresholds) (*AutoscaleResult, error) {
	if s.keeper == nil {
		return &AutoscaleResult{Skipped: "provider has no instance-resize capability"}, nil
	}
	signals, err := s.signals.ReadKeeperSignals(ctx, req.ClusterName)
	if err != nil {
		return &AutoscaleResult{Skipped: "signals unreadable: " + err.Error()}, nil
	}
	decision := domain.ScaleDecision{Signals: signals}
	decision.Breach = thresholds.KeeperBreach(signals)
	if !decision.Breach {
		decision.Reason = "keeper signals within thresholds"
		return &AutoscaleResult{Decision: decision}, nil
	}
	if req.KeeperInstanceID == "" {
		return &AutoscaleResult{Skipped: "keeper breach but no coordinator identity (state file lacks instance id)"}, nil
	}
	self, err := s.keeper.DescribeInstance(ctx, req.KeeperInstanceID)
	if err != nil {
		return nil, fmt.Errorf("autoscale: describe keeper %s: %w", req.KeeperInstanceID, err)
	}
	decision.From = self.InstanceType
	next, ok := req.Ladder.Next(self.InstanceType)
	if !ok {
		decision.Reason = fmt.Sprintf("keeper pressure breach but no ladder step from %s (floor/ceiling/unknown type)", self.InstanceType)
		return &AutoscaleResult{Decision: decision}, nil
	}
	decision.To = next
	decision.Action = "keeper-self"
	decision.Reason = fmt.Sprintf("keeper breach (latency %.0fms, outstanding %.0f, idle %.0f%%) — self-resize %s → %s",
		signals.KeeperMaxLatencyMs, signals.KeeperOutstanding, signals.KeeperCPUIdlePct, decision.From, decision.To)
	if err := s.keeper.ChangeInstanceType(ctx, port.InstanceTypeChange{
		InstanceID: req.KeeperInstanceID,
		TargetType: next,
	}); err != nil {
		return nil, fmt.Errorf("autoscale: keeper resize: %w", err)
	}
	s.markCooldown(ctx, port.CooldownRequest{Table: req.Cooldown.Table, Key: "autoscale-" + string(req.Coordinator)})
	return &AutoscaleResult{Decision: decision}, nil
}

// writerStep decides the writers' fleet roll: the busiest writer's
// idle at-or-below the threshold → one bounded launch-template step
// with MinHealthyPercentage 100 and AutoRollback armed (the ingest
// quorum never dips below healthy through the roll).
func (s *AutoscaleService) writerStep(ctx context.Context, req AutoscaleRequest, thresholds domain.ScaleThresholds) (*AutoscaleResult, error) {
	if s.fleet == nil {
		return &AutoscaleResult{Skipped: "provider has no fleet-resize capability"}, nil
	}
	signals, err := s.signals.ReadWriterSignals(ctx, req.ClusterName)
	if err != nil {
		return &AutoscaleResult{Skipped: "signals unreadable: " + err.Error()}, nil
	}
	decision := domain.ScaleDecision{Signals: signals}
	decision.Breach = thresholds.WriterBreach(signals)
	if !decision.Breach {
		decision.Reason = "writer fleet idle above threshold"
		return &AutoscaleResult{Decision: decision}, nil
	}
	if req.Fleet.AutoScalingGroupName == "" {
		return &AutoscaleResult{Skipped: "writer breach but the fleet identity is unconfigured (spec lacks writers_asg_name)"}, nil
	}
	// The template the fleet RUNS, resolved at decision time (the spec
	// names only the ASG — a template change between spec renders
	// must not wedge the roll).
	binding, err := s.fleet.DescribeFleetLaunchTemplate(ctx, req.Fleet.AutoScalingGroupName)
	if err != nil {
		return &AutoscaleResult{Skipped: "cannot resolve the ASG's launch template: " + err.Error()}, nil
	}
	// The current type: the coordinator IS a writer — its own type is
	// the fleet's (one type per fleet; the ASG holds the template).
	if s.keeper == nil {
		return &AutoscaleResult{Skipped: "writer breach but no instance-describe capability (cannot read the fleet's current type)"}, nil
	}
	self, err := s.keeper.DescribeInstance(ctx, req.KeeperInstanceID)
	if err != nil || self == nil {
		return &AutoscaleResult{Skipped: "cannot read the coordinator's own type (the fleet's type source)"}, nil
	}
	decision.From = self.InstanceType
	next, ok := req.Ladder.Next(self.InstanceType)
	if !ok {
		decision.Reason = fmt.Sprintf("writer fleet breach but no ladder step from %s (floor/ceiling/unknown type)", self.InstanceType)
		return &AutoscaleResult{Decision: decision}, nil
	}
	ver, err := s.fleet.CreateLaunchTemplateVersion(ctx, binding.LaunchTemplateID, next)
	if err != nil {
		return nil, fmt.Errorf("autoscale: launch template step: %w", err)
	}
	decision.To = next
	decision.Action = "writer-roll"
	decision.Reason = fmt.Sprintf("writer fleet breach (busiest idle %.0f%%) — rolling %s to LT v%d (%s)",
		signals.WriterCPUIdlePct, req.Fleet.AutoScalingGroupName, ver, next)
	roll := req.Fleet
	roll.Version = strconv.FormatInt(ver, 10)
	roll.MinHealthyPercentage = 100
	if roll.InstanceWarmup == 0 {
		roll.InstanceWarmup = 300
	}
	if err := s.fleet.StartFleetRefresh(ctx, roll); err != nil {
		return nil, fmt.Errorf("autoscale: fleet roll: %w", err)
	}
	s.markCooldown(ctx, port.CooldownRequest{Table: req.Cooldown.Table, Key: "autoscale-" + string(req.Coordinator)})
	return &AutoscaleResult{Decision: decision}, nil
}

// markCooldown paces the next step (best-effort: the ladder bound is
// the hard cap; a failed cooldown write logs through the caller's
// result, never fails the step that already ran). The record rides
// the caller's key unchanged — the peek and the mark MUST agree, and
// the caller (the CLI) owns the key's namespacing so a claims-table
// record can never collide with a hostname lease row.
func (s *AutoscaleService) markCooldown(ctx context.Context, coolReq port.CooldownRequest) {
	if coolReq.Minutes == 0 {
		coolReq.Minutes = 60
	}
	_ = s.cool.MarkCooldown(ctx, coolReq)
}
