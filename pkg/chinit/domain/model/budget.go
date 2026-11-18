// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

// DisruptionBudget is the fleet's voluntary-disruption policy — the
// VM-level PDB equivalent. The operator relies on Kubernetes
// PodDisruptionBudgets to bound concurrent replica outages during
// evictions; chinit's fleet has no scheduler to ask, so every
// voluntary termination path (drain, refresh, rollout) gates on this
// budget computed from the live claims + status rows.
//
// The budget bounds CONCURRENT outages, not total ones: a rollout may
// replace every writer, one at a time, waiting for each replacement
// to be healthy before the next.
type DisruptionBudget struct {
	// MinHealthyWriters is the floor of healthy writer nodes during
	// any voluntary disruption (0 = the default: 1 — a fleet with a
	// single live writer never voluntarily takes it down; a multi-
	// writer fleet always keeps one serving).
	MinHealthyWriters int `yaml:"min_healthy_writers,omitempty" json:"min_healthy_writers,omitempty"`
	// MinHealthyReaders is the floor of healthy reader nodes (0 = no
	// reader floor: readers are disposable; native ASG policies own
	// their horizontal scaling).
	MinHealthyReaders int `yaml:"min_healthy_readers,omitempty" json:"min_healthy_readers,omitempty"`
}

// DefaultMinHealthyWriters applies when the spec leaves the floor
// zero (the safe default for the writer path).
const DefaultMinHealthyWriters = 1
