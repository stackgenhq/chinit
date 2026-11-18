// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// runAutoscale is `chinit autoscale` (the timer on keepers and
// writers): one coordinator per fleet decides and acts.
func runAutoscale(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	role, skip := coordinatorOf(ctx, st)
	if skip != "" {
		logger.GetLogger(ctx).Info("autoscale: no coordinator designation this tick", "reason", skip)
		return nil
	}

	svc, req, err := buildAutoscaleRequest(st, role)
	if err != nil {
		return fmt.Errorf("autoscale: %w", err)
	}
	result, err := svc.Autoscale(ctx, req)
	if err != nil {
		return fmt.Errorf("autoscale: %w", err)
	}
	logAutoscaleResult(ctx, result)
	return nil
}

// coordinatorOf answers this tick's coordinator designation and the
// skip reason when this node holds none.
func coordinatorOf(ctx context.Context, st *nodeState) (domain.CoordinatorRole, string) {
	switch st.Identity.Role {
	case model.RoleKeeper:
		if st.Identity.Hostname == keeperOrdinalZero() {
			return domain.CoordinatorKeeper, ""
		}
		return domain.CoordinatorNone,
			fmt.Sprintf("the keeper coordinator is %s; this node is %s", keeperOrdinalZero(), st.Identity.Hostname)
	case model.RoleWriter:
		claims, cerr := listLiveClaims(ctx, st, autoscaleLivelinessFrom(st))
		if cerr != nil {
			logger.GetLogger(ctx).Warn("autoscale: live-claims listing failed; treating as no claims", "error", cerr.Error())
			claims = nil
		}
		if gateMaintenanceJobWithClaims(ctx, st, claims) {
			return domain.CoordinatorNone, "the ordinal-0 maintenance holder decides (this node is not it)"
		}
		return domain.CoordinatorWriter, ""
	case model.RoleReader, model.RoleBackup:
		return domain.CoordinatorNone,
			"role has no vertical path (native ASG policies own horizontal scaling)"
	default:
		return domain.CoordinatorNone, fmt.Sprintf("role %q not recognized", st.Identity.Role)
	}
}

// buildAutoscaleRequest assembles the service call: the ports (from
// the provider), the ladder (from the spec), the cooldown table, and
// the identity the coordinator acts on.
func buildAutoscaleRequest(st *nodeState, role domain.CoordinatorRole) (*service.AutoscaleService, service.AutoscaleRequest, error) {
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return nil, service.AutoscaleRequest{}, fmt.Errorf("build provider: %w", err)
	}
	signals := port.AsScaleSignalsReader(provider)
	resizer := port.AsInstanceResizer(provider)
	roller := port.AsFleetResizer(provider)
	cooldown := port.AsCooldownStore(provider)
	if signals == nil || cooldown == nil {
		return nil, service.AutoscaleRequest{},
			fmt.Errorf("provider %s lacks the metric-read or cooldown capability", providerNameOf(st))
	}
	svc, err := service.NewAutoscaleService(signals, resizer, roller, cooldown)
	if err != nil {
		return nil, service.AutoscaleRequest{}, err
	}

	var ladder domain.TypeLadder
	var keeperID string
	var fleet port.FleetRefreshRequest
	switch role {
	case domain.CoordinatorKeeper:
		ladder = ladderForRole("keeper")
		keeperID = st.Identity.InstanceID
	case domain.CoordinatorWriter:
		ladder = ladderForRole("writer")
		keeperID = st.Identity.InstanceID // the coordinator IS a writer: its own type is the fleet's
		// The launch template is NOT spec-rendered: the service
		// resolves the ASG's CURRENT template at decision time (a
		// template change between spec renders must not wedge the
		// roll).
		fleet = port.FleetRefreshRequest{
			AutoScalingGroupName: writersASG(),
			InstanceWarmup:       300,
			MinHealthyPercentage: 100,
		}
	}
	if len(ladder.Types) == 0 {
		return nil, service.AutoscaleRequest{},
			fmt.Errorf("the spec carries no %s type ladder (autoscaling_config)", role)
	}
	return svc, service.AutoscaleRequest{
		ClusterName:      st.clusterName(),
		Coordinator:      role,
		Cooldown:         port.CooldownRequest{Table: cooldownTableOf(st)},
		KeeperInstanceID: keeperID,
		Ladder:           ladder,
		Fleet:            fleet,
	}, nil
}

// logAutoscaleResult records the outcome (the journal contract).
func logAutoscaleResult(ctx context.Context, r *service.AutoscaleResult) {
	if r == nil {
		return
	}
	if r.Skipped != "" {
		logger.GetLogger(ctx).Info("autoscale: skipped", "reason", r.Skipped)
		return
	}
	d := r.Decision
	if d.Action == "" {
		logger.GetLogger(ctx).Info("autoscale: no action this tick", "reason", d.Reason)
		return
	}
	logger.GetLogger(ctx).Info("autoscale: decision", "action", d.Action, "reason", d.Reason)
}

// The spec projections the CLI owns (the bundle's chinit.yaml — the
// SAME fields Terraform renders for the Lambda env today, one source
// of truth; the inventorySpec projection lives in nodestate.go).

// keeperOrdinalZero answers the spec's keeper-0 hostname (the keeper
// coordinator's static identity).
func keeperOrdinalZero() string {
	if spec, err := loadInventorySpec(); err == nil && spec != nil {
		for _, name := range spec.KeeperNodes {
			if strings.HasSuffix(name, "-0") {
				return name
			}
		}
		if len(spec.KeeperNodes) > 0 {
			return spec.KeeperNodes[0]
		}
	}
	return "keeper-0"
}

// ladderForRole reads the role's bounded ladder from the spec.
func ladderForRole(role string) domain.TypeLadder {
	spec, err := loadInventorySpec()
	if err != nil || spec == nil {
		return domain.TypeLadder{}
	}
	types, minT, maxT := spec.Autoscaling.ladder(role)
	return domain.TypeLadder{Types: types, Min: minT, Max: maxT}
}

// writersASG answers the writers ASG name from the spec. The launch
// template is deliberately NOT spec-rendered: the coordinator
// resolves the ASG's CURRENT template at decision time (a template
// change between spec renders must not wedge the roll).
func writersASG() string {
	spec, err := loadInventorySpec()
	if err != nil || spec == nil {
		return ""
	}
	return spec.Autoscaling.WritersASG
}

// cooldownTableOf resolves the claims table the cooldown rides.
func cooldownTableOf(st *nodeState) string {
	if st.Identity.LeaseTable != "" {
		return st.Identity.LeaseTable
	}
	if spec, err := loadInventorySpec(); err == nil && spec != nil {
		return spec.ClaimerTableName
	}
	return ""
}

// autoscaleLivelinessFrom mirrors gcLivelinessFrom: the provider's
// lease lister through the registry.
func autoscaleLivelinessFrom(st *nodeState) port.LeaseLister {
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return nil
	}
	return port.AsLeaseLister(provider)
}
