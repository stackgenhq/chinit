// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import "time"

// HookEvent is the lifecycle boundary a hook fires at.
type HookEvent string

const (
	// HookPreBoot fires before the boot pipeline's first step (the
	// operator's pre-create moment: register the node with external
	// systems).
	HookPreBoot HookEvent = "pre_boot"
	// HookPostBoot fires after the last step (the operator's
	// HostCreate POST hook: notify, validate, warm caches).
	HookPostBoot HookEvent = "post_boot"
	// HookPreDrain fires before the drain's first step (the
	// operator's HostShutdown moment).
	HookPreDrain HookEvent = "pre_drain"
	// HookPostDrain fires after the drain completes (the operator's
	// HostDelete hook).
	HookPostDrain HookEvent = "post_drain"
)

// Hook is one lifecycle command: the operator's spec.reconcile.hooks
// parity at the VM level. The command runs under `sh -c` with the
// node's identity in the environment (CHINIT_ROLE, CHINIT_HOSTNAME,
// CHINIT_CLUSTER).
type Hook struct {
	// Event is the boundary this hook fires at.
	Event HookEvent `yaml:"event" json:"event"`
	// Name labels the hook in the journal (required: the journal line
	// must name WHAT ran, not just that something did).
	Name string `yaml:"name" json:"name"`
	// Command is the shell command (`sh -c`).
	Command string `yaml:"command" json:"command"`
	// TimeoutSeconds bounds the command (0 = 30s default).
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
	// Critical: true = a failure aborts the boundary (the boot fails,
	// the drain proceeds anyway — a drain can never be blocked by a
	// hook, the node is already going down); false = the failure is
	// logged and the boundary continues.
	Critical bool `yaml:"critical,omitempty" json:"critical,omitempty"`
}

// Hooks is the spec's `hooks:` section.
type Hooks struct {
	// Items is the ordered hook list (the events fire in list order).
	Items []Hook `yaml:"items,omitempty" json:"items,omitempty"`
}

// HookTimeout applies the hook's default.
func (h Hook) HookTimeout() time.Duration {
	if h.TimeoutSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(h.TimeoutSeconds) * time.Second
}

// HooksFor answers the hooks bound to one event, in list order.
func (hs *Hooks) HooksFor(event HookEvent) []Hook {
	if hs == nil {
		return nil
	}
	var out []Hook
	for _, h := range hs.Items {
		if h.Event == event {
			out = append(out, h)
		}
	}
	return out
}
