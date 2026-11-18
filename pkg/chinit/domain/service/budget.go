// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// BudgetVerdict is the gate's answer: allow, or defer with the reason
// (the caller NEVER forces — the same rule dead-replica-gc's writer
// pass applies: without data, defer).
type BudgetVerdict struct {
	// Allowed is true when the voluntary disruption may proceed.
	Allowed bool
	// Reason carries the defer cause (diagnostics; empty on allow).
	Reason string
}

// BudgetInput is the fleet view the gate computes over: the live
// claims (liveness) and the status rows (health + cordon), already
// joined by the caller (the claims carry no role; the status rows
// do).
type BudgetInput struct {
	// Budget is the spec's policy (zero floors get the safe
	// defaults).
	Budget model.DisruptionBudget
	// HealthyWriters counts writer-role status rows with healthy
	// probes, excluding cordoned ones (a cordoned node is never
	// counted toward the floor — it may be mid-maintenance).
	HealthyWriters int
	// HealthyReaders counts reader-role healthy non-cordoned rows.
	HealthyReaders int
	// TargetRole is the role of the node asking to disrupt.
	TargetRole model.Role
}

// CheckDisruptionBudget answers whether ONE more voluntary
// disruption of TargetRole is within the budget. The counting rule:
// a node asking to drain itself is currently healthy (its own status
// row counts toward the floor; the disruption removes it — the gate
// checks the AFTER picture: healthy - 1 >= floor).
//
// The fail-safe contract: an uncountable fleet (no status rows for
// the role) defers, never allows — the same rule as dead-replica-gc's
// writer pass. The one exception is the LAST healthy writer asking to
// drain at shutdown: a termination is not voluntary deferrable work —
// the shutdown drain does not consult the budget (the OS is already
// going down); only voluntary paths (refresh, rollout, scale-in
// hooks) ask.
func CheckDisruptionBudget(in BudgetInput) BudgetVerdict {
	switch in.TargetRole {
	case model.RoleWriter:
		floor := in.Budget.MinHealthyWriters
		if floor <= 0 {
			floor = model.DefaultMinHealthyWriters
		}
		after := in.HealthyWriters - 1
		if in.HealthyWriters <= 0 {
			return BudgetVerdict{
				Allowed: false,
				Reason:  "no healthy writer status rows visible (status rows missing — defer, never guess)",
			}
		}
		if after < floor {
			return BudgetVerdict{
				Allowed: false,
				Reason:  fmt.Sprintf("writer floor: %d healthy - 1 disruption < floor %d", in.HealthyWriters, floor),
			}
		}
		return BudgetVerdict{Allowed: true}
	case model.RoleReader:
		if in.Budget.MinHealthyReaders > 0 {
			after := in.HealthyReaders - 1
			if in.HealthyReaders <= 0 {
				return BudgetVerdict{
					Allowed: false,
					Reason:  "no healthy reader status rows visible (status rows missing — defer)",
				}
			}
			if after < in.Budget.MinHealthyReaders {
				return BudgetVerdict{
					Allowed: false,
					Reason: fmt.Sprintf("reader floor: %d healthy - 1 disruption < floor %d",
						in.HealthyReaders, in.Budget.MinHealthyReaders),
				}
			}
		}
		return BudgetVerdict{Allowed: true}
	default:
		// Keepers and the backup are not gated (keeper rollouts have
		// their own ensemble-health refusal; the backup is a
		// durability node with no serving floor).
		return BudgetVerdict{Allowed: true}
	}
}

// CountHealthyByRole folds the status rows into the budget input's
// counts (healthy, non-cordoned, by role). Claims-joined by the
// caller; pure over the rows.
func CountHealthyByRole(rows []port.NodeStatus) (writers, readers int) {
	for _, row := range rows {
		if row.Cordoned || row.Health != port.NodeHealthy {
			continue
		}
		switch model.Role(row.Role) {
		case model.RoleWriter:
			writers++
		case model.RoleReader:
			readers++
		}
	}
	return writers, readers
}
