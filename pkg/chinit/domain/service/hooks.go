// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// HookRunner runs one hook command. The production implementation
// shells `sh -c` with the identity environment; tests record.
type HookRunner interface {
	// RunHook executes one hook's command under ctx and answers the
	// combined output. A non-nil error is the hook's failure.
	RunHook(ctx context.Context, hook model.Hook, identity HookIdentity) (string, error)
}

// HookIdentity is the environment the hook runs with: the node's own
// settled facts, never secrets (the hook resolves its own secrets the
// same way every other chinit component does — by name, over the
// instance role).
type HookIdentity struct {
	Role       string
	Hostname   string
	Cluster    string
	InstanceID string
}

// Env renders the identity as the hook's environment block.
func (h HookIdentity) Env() []string {
	return []string{
		"CHINIT_ROLE=" + h.Role,
		"CHINIT_HOSTNAME=" + h.Hostname,
		"CHINIT_CLUSTER=" + h.Cluster,
		"CHINIT_INSTANCE_ID=" + h.InstanceID,
	}
}

// ShellHookRunner is the production runner: `sh -c` with the identity
// env, bounded by the hook's timeout.
type ShellHookRunner struct{}

// RunHook implements HookRunner.
func (ShellHookRunner) RunHook(ctx context.Context, hook model.Hook, identity HookIdentity) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, hook.HookTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", hook.Command)
	cmd.Env = append(cmd.Environ(), identity.Env()...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// RunHooks executes one boundary's hooks in list order. The rules
// (the criticality contract):
//   - a non-critical hook's failure is logged and the boundary
//     continues (the operator's best-effort hook semantics);
//   - a CRITICAL hook's failure aborts the boundary with the error —
//     EXCEPT on drain boundaries, where the node is already going
//     down and no hook may block it: the failure is logged and the
//     drain continues (the drain's own contract).
//
// An empty boundary (no hooks for the event) is a no-op success.
func RunHooks(ctx context.Context, runner HookRunner, hooks *model.Hooks, event model.HookEvent, identity HookIdentity, drainBoundary bool) error {
	log := logger.GetLogger(ctx)
	list := hooks.HooksFor(event)
	if len(list) == 0 {
		return nil
	}
	for _, hook := range list {
		out, err := runner.RunHook(ctx, hook, identity)
		if err != nil {
			if hook.Critical && !drainBoundary {
				return fmt.Errorf("hook %s (%s): %w: %s", hook.Name, event, err, trimHookLog(out))
			}
			log.Warn("hook failed (continuing)",
				"hook", hook.Name, "event", string(event),
				"critical", hook.Critical, "drain_boundary", drainBoundary,
				"err", err.Error(), "out", trimHookLog(out))
			continue
		}
		log.Info("hook complete", "hook", hook.Name, "event", string(event), "out", trimHookLog(out))
	}
	return nil
}

// trimHookLog bounds a hook's captured output (the journal line must
// not carry a whole build log).
func trimHookLog(s string) string {
	const n = 1000
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// HookEvents is the ordered boundary list (the docs and the specs share it).
var HookEvents = []model.HookEvent{
	model.HookPreBoot,
	model.HookPostBoot,
	model.HookPreDrain,
	model.HookPostDrain,
}

// hookIdentity answers the boot's settled identity facts for the hook
// environment (the Boot method the steps call; keepers' facts render
// the same way — the hook reads the role env and adapts).
func (b *Boot) hookIdentity() HookIdentity {
	role := string(b.Spec.Role)
	hostname := b.Identity.Hostname
	if hostname == "" {
		hostname = b.Spec.ClusterName
	}
	return HookIdentity{
		Role:       role,
		Hostname:   hostname,
		Cluster:    b.Identity.ClusterName,
		InstanceID: b.Identity.InstanceID,
	}
}
