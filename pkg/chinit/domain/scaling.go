// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package domain

// The vertical-scaling entities: the bounded type ladder, the signal
// thresholds, and the coordinator's decision record. This file is the
// innermost layer — it imports nothing; the autoscale SERVICE
// orchestrates these entities over the PORT interfaces (the
// vertical-autoscaler Lambda's replacement), and the INFRA adapters
// implement the ports against AWS.
//
// Why a COORDINATOR and never per-node free-for-all: in a keeper
// quorum, every node independently deciding to resize is two failure
// classes at once — simultaneous stop/modify/start cycles take the
// quorum DOWN, and every node seeing "high CPU" steps the whole
// fleet N times in one window. Exactly ONE node decides and acts
// (keeper-0 for keepers — static identity, the hostname IS the
// ordinal; the ordinal-0 maintenance holder for writers). Horizontal
// scaling stays cloud-native (native ASG policies own it; the
// autoscale path never touches it) — vertical has no cloud-native
// primitive: stop/modify/start and launch-template rolls are the
// only mechanisms, and the coordinator issues exactly those calls.

// TypeLadder is the bounded vertical-scaling ladder for one role: the
// ascending instance types the coordinator may step through, with the
// floor and ceiling the steps respect.
type TypeLadder struct {
	// Types is the ascending instance-type list ("t4g.medium" ..).
	Types []string
	// Min is the ladder floor (a step below it is refused; the
	// dev-shape posture sets the floor above the smallest type).
	Min string
	// Max is the ladder ceiling (a step past it is refused — the
	// runaway cap; the cooldown paces, the ceiling bounds).
	Max string
}

// Next answers the step strictly above current, within bounds. ok is
// false at the ceiling, below the floor, or when current is not on
// the ladder (an unknown type never steps — the coordinator logs and
// stops rather than guessing).
func (l TypeLadder) Next(current string) (next string, ok bool) {
	idx := -1
	for i, t := range l.Types {
		if t == current {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(l.Types) {
		return "", false
	}
	candidate := idx + 1
	if !l.bounded(candidate) {
		return "", false
	}
	return l.Types[candidate], true
}

// pos answers the type's ladder index; an ABSENT type (or the unset
// "" bound) answers the NO-CONSTRAINT sentinel — the bound checks
// then constrain nothing (the explicit open-ladder shape).
func (l TypeLadder) pos(t string) int {
	if t == "" {
		return -1
	}
	for i, entry := range l.Types {
		if entry == t {
			return i
		}
	}
	return -1
}

// bounded answers whether the candidate index passes both bounds.
// The -1 sentinel (absent bound or off-ladder candidate) never
// blocks: an unset Min/Max is the explicit no-bound shape, and an
// off-ladder MAX that would otherwise block every step is the
// misconfigured-but-open shape the ceiling check reports.
func (l TypeLadder) bounded(candidate int) bool {
	minI := l.pos(l.Min)
	maxI := l.pos(l.Max)
	if minI >= 0 && candidate < minI {
		return false
	}
	if maxI >= 0 && candidate > maxI {
		return false
	}
	return true
}

// AtOrAboveMax answers whether current sits at (or past) the
// ceiling — the "already at max" decision input. An unset ceiling
// answers false only when current is the ladder's LAST entry (the
// end is always the ceiling; the Max field tightens it further).
func (l TypeLadder) AtOrAboveMax(current string) bool {
	pos := l.pos(current)
	if pos < 0 {
		return false
	}
	if maxI := l.pos(l.Max); maxI >= 0 {
		return pos >= maxI
	}
	return pos == len(l.Types)-1
}

// ScaleSignals is the autoscale decision input: the metric readings
// one tick observed, with the thresholds a breach is judged against.
// The values are the BREACH-SIDE aggregates (the least pressure seen
// in the window — every sample must have breached; one noisy minute
// never steps a fleet, the alarm's evaluation_periods parity).
type ScaleSignals struct {
	// KeeperMaxLatencyMs is the ensemble's max request latency (ms).
	KeeperMaxLatencyMs float64
	// KeeperOutstanding is the ensemble's outstanding-request count.
	KeeperOutstanding float64
	// KeeperCPUIdlePct is the coordinator's own CPU idle (100-idle
	// = busy; the keeper's own node is the ensemble's signal source).
	KeeperCPUIdlePct float64
	// WriterCPUIdlePct is the busiest writer's average CPU idle percentage.
	WriterCPUIdlePct float64
}

// ScaleThresholds is the breach line: a signal at-or-beyond (idle:
// at-or-below) its threshold is pressure.
type ScaleThresholds struct {
	// KeeperLatencyMs: at-or-beyond = breach.
	KeeperLatencyMs float64
	// KeeperOutstanding: at-or-beyond = breach.
	KeeperOutstanding float64
	// KeeperCPUIdlePct: at-or-below = breach (idle LOW = busy).
	KeeperCPUIdlePct float64
	// WriterCPUIdlePct: at-or-below = breach (the fleet signal).
	WriterCPUIdlePct float64
}

// DefaultScaleThresholds mirrors the CloudWatch alarms' defaults the
// autoscale path inherits (keeper-max-latency 500ms;
// keeper-outstanding-requests 10; the CPU idle lines at the fleet's
// own scale-out triggers).
func DefaultScaleThresholds() ScaleThresholds {
	return ScaleThresholds{
		KeeperLatencyMs:   500,
		KeeperOutstanding: 10,
		KeeperCPUIdlePct:  20,
		WriterCPUIdlePct:  35,
	}
}

// KeeperBreach answers whether any keeper signal breached.
func (t ScaleThresholds) KeeperBreach(s ScaleSignals) bool {
	return s.KeeperMaxLatencyMs >= t.KeeperLatencyMs ||
		s.KeeperOutstanding >= t.KeeperOutstanding ||
		s.KeeperCPUIdlePct <= t.KeeperCPUIdlePct
}

// WriterBreach answers whether the fleet CPU signal breached.
func (t ScaleThresholds) WriterBreach(s ScaleSignals) bool {
	return s.WriterCPUIdlePct <= t.WriterCPUIdlePct
}

// ScaleDecision is the coordinator's verdict for one tick: what it
// saw, what it concluded, and what it will (or will not) do. The
// record the CLI prints and the tests assert — a decision is data.
type ScaleDecision struct {
	// Signals is what the tick observed.
	Signals ScaleSignals
	// Breach is whether thresholds judged pressure.
	Breach bool
	// Action names the execution step: "" (no-op), "keeper-self",
	// "keeper-peer", "writer-roll".
	Action string
	// From/To are the ladder step when Action != "".
	From, To string
	// Reason is the human line (the journal contract).
	Reason string
}

// CoordinatorRole is which coordinator shape this node runs: keepers
// self-resize (the coordinator is keeper-0; peers first in the
// multi-keeper shape, self last), writers roll the ASG (the ordinal-0
// maintenance holder).
type CoordinatorRole string

const (
	// CoordinatorKeeper is the keeper-0 self-resize path.
	CoordinatorKeeper CoordinatorRole = "keeper"
	// CoordinatorWriter is the writers' ordinal-0 fleet-roll path.
	CoordinatorWriter CoordinatorRole = "writer"
	// CoordinatorNone: this node holds no coordinator designation
	// this tick (readers/backup scale horizontally; non-ordinal-0
	// writers skip — the timer's no-op exit).
	CoordinatorNone CoordinatorRole = "none"
)
