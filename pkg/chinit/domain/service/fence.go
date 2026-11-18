// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// ReplacementGate checks whether another live instance already holds this
// node's ordinal slot in the claims table so a terminating instance's
// drain does not delete a replacement node's DNS record.
type ReplacementGate struct {
	// table is the ordinal-lease table the gate scans.
	table string
	// lister enumerates the table's live claims.
	lister port.LeaseLister
}

// NewReplacementGate builds the gate over a live-claims lister. A nil
// lister (or empty table) is the documented degradation: the gate
// answers NOT-replaced and the ExactOnly delete remains the only
// guard.
func NewReplacementGate(table string, lister port.LeaseLister) *ReplacementGate {
	return &ReplacementGate{table: table, lister: lister}
}

// ReplacedBy answers whether another live instance holds the named
// slot. An unreadable claims table answers NOT-replaced: the drain
// proceeds (the record delete is ExactOnly — a live replacement's
// value survives either way).
func (g *ReplacementGate) ReplacedBy(ctx context.Context, hostname, ownInstanceID string) (string, bool) {
	if g == nil || g.lister == nil || g.table == "" {
		return "", false
	}
	claims, err := g.lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: g.table})
	if err != nil {
		logger.GetLogger(ctx).Warn("drain gate: claims scan failed; proceeding (the exact-only delete remains the guard)", "err", err)
		return "", false
	}
	for _, claim := range claims {
		if claim.Hostname == hostname && claim.InstanceID != "" && claim.InstanceID != ownInstanceID {
			return claim.InstanceID, true
		}
	}
	return "", false
}

// SelfFenceService retires an identity-contested node when another live
// instance holds its ordinal slot: it quiesces local replication and
// marks the instance unhealthy so the Auto Scaling Group replaces it.
type SelfFenceService struct {
	// quiescer stops the node's replication serving.
	quiescer port.NodeQuiescer
	// fencer marks the instance unhealthy.
	fencer port.InstanceFencer
	// clock is optional; nil means "now".
}

// NewSelfFenceService builds the fence over its two capabilities.
func NewSelfFenceService(quiescer port.NodeQuiescer, fencer port.InstanceFencer) *SelfFenceService {
	return &SelfFenceService{quiescer: quiescer, fencer: fencer}
}

// FenceResult reports the fence outcome: quiesced status and the
// terminal error (nil = the ASG will replace the instance).
type FenceResult struct {
	// QuiesceFailed marks the stop-serving statements as failed (the
	// fence still marks unhealthy — the node is exiting either way).
	QuiesceFailed bool
	// MarkedUnhealthy is true when the ASG accepted the health flip.
	MarkedUnhealthy bool
}

// Fence runs the retire sequence: quiesce, then mark unhealthy.
func (s *SelfFenceService) Fence(ctx context.Context, hostname, instanceID string) (FenceResult, error) {
	log := logger.GetLogger(ctx)
	result := FenceResult{}
	if err := s.quiescer.Quiesce(); err != nil {
		// Best-effort by contract: a live duplicate's statements may
		// fail for the same contested reasons the fence exists for.
		// The unhealthy mark proceeds — the ASG teardown stops the
		// server far more definitively than the statements can.
		result.QuiesceFailed = true
		log.Warn("fence: quiesce failed (continuing to the unhealthy mark)",
			"hostname", hostname, "err", err)
	}
	if s.fencer == nil {
		return result, &FenceCapabilityError{Hostname: hostname}
	}
	if err := s.fencer.SetSelfUnhealthy(ctx, instanceID); err != nil {
		return result, err
	}
	result.MarkedUnhealthy = true
	log.Info("fence: instance marked unhealthy — the ASG will replace this duplicate",
		"hostname", hostname,
		"instance_id", instanceID)
	return result, nil
}

// FenceCapabilityError names the missing-fence degradation: the node
// is quiesced but a human must replace it (no capability to mark the
// instance unhealthy).
type FenceCapabilityError struct {
	Hostname string
}

func (e *FenceCapabilityError) Error() string {
	return "fence: no instance-health capability (node quiesced; manual replacement needed for " + e.Hostname + ")"
}
