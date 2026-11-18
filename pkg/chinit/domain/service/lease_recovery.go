// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// LeaseRecovery handles lease renewal failures by attempting to re-claim
// the node's own ordinal slot with ExactOrdinal enabled, classifying the
// outcome as Reclaimed, Contested (held by a live replacement), or
// Renumbered.
type LeaseRecovery struct {
	// claimer takes ordinals (the ExactOrdinal shape for recovery).
	claimer port.OrdinalClaimer
	// table is the lease table the recovery works in.
	table string
}

// NewLeaseRecovery builds the recovery over a claimer and the node's
// lease table.
func NewLeaseRecovery(claimer port.OrdinalClaimer, table string) *LeaseRecovery {
	return &LeaseRecovery{claimer: claimer, table: table}
}

// RecoveryOutcome is the verdict for one failed renewal.
type RecoveryOutcome int

const (
	// OutcomeReclaimed is the good ending: the node re-took its own name.
	OutcomeReclaimed RecoveryOutcome = iota
	// OutcomeContested is the split-brain ending: a live replacement
	// owns the name — the caller must fence.
	OutcomeContested
	// OutcomeRenumbered is the unrecoverable ending: the identity
	// would CHANGE — the caller must fail loudly.
	OutcomeRenumbered
	// OutcomeUnrecoverable is the input-problem ending (no ordinal in
	// the hostname, or the claim itself failed for an API reason) —
	// the caller surfaces the original error.
	OutcomeUnrecoverable
)

// Recover attempts the exact re-claim after a failed renewal. prefix
// and ordinal describe the node's own name; the ExactOrdinal claim
// never walks to another ordinal.
func (r *LeaseRecovery) Recover(ctx context.Context, req port.ClaimRequest, ordinal int) (RecoveryOutcome, error) {
	log := logger.GetLogger(ctx)
	newOrdinal, err := r.claimer.ClaimOrdinal(ctx, req)
	if err != nil {
		// Another LIVE instance holds this node's name: the fleet
		// has two nodes believing they are the same replica.
		log.Error("lease-recovery: own ordinal held by a live instance — the node must fence",
			"hostname", req.Prefix+fmt.Sprintf("%d", ordinal), "err", err)
		return OutcomeContested, err
	}
	if newOrdinal != ordinal {
		// Defensive: the ExactOrdinal shape cannot answer another
		// ordinal, but the identity contract is too important to
		// trust shape alone.
		return OutcomeRenumbered, fmt.Errorf("lease-recovery: re-claim took ordinal %d, identity is %s%d (node must re-image)", newOrdinal, req.Prefix, ordinal)
	}
	log.Info("lease-recovery: ordinal re-claimed", "hostname", req.Prefix+fmt.Sprintf("%d", ordinal), "ordinal", ordinal)
	return OutcomeReclaimed, nil
}
