// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
)

// fakeHookRunner records the hooks it ran and fails on cue.
type fakeHookRunner struct {
	calls   []string
	failOn  string
	lastEnv []string
}

func (f *fakeHookRunner) RunHook(_ context.Context, hook model.Hook, identity service.HookIdentity) (string, error) {
	f.calls = append(f.calls, hook.Name)
	f.lastEnv = identity.Env()
	if hook.Name == f.failOn {
		return "hook failed on cue", fmt.Errorf("exit status 1")
	}
	return "ok", nil
}

// The lifecycle hook executor's contract: the ordered boundary
// execution, the criticality split, and the drain's never-blocking
// rule — the operator's spec.reconcile.hooks parity at the VM level.
var _ = Describe("RunHooks", func() {
	var (
		runner *fakeHookRunner
		ctx    context.Context
		id     service.HookIdentity
	)
	const hookName = "register"

	BeforeEach(func() {
		runner = &fakeHookRunner{}
		ctx = context.Background()
		id = service.HookIdentity{Role: "writer", Hostname: "writer-0", Cluster: "demo", InstanceID: "i-1"}
	})

	hooksWith := func(event model.HookEvent, critical bool) *model.Hooks {
		return &model.Hooks{Items: []model.Hook{
			{Event: event, Name: hookName, Command: "true", Critical: critical},
		}}
	}

	It("runs the boundary's hooks in order and carries the identity env", func() {
		hooks := &model.Hooks{Items: []model.Hook{
			{Event: model.HookPostBoot, Name: "warm-cache", Command: "warm.sh"},
			{Event: model.HookPostBoot, Name: "notify", Command: "notify.sh"},
			{Event: model.HookPreBoot, Name: "other-boundary", Command: "x.sh"},
		}}
		err := service.RunHooks(ctx, runner, hooks, model.HookPostBoot, id, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.calls).To(Equal([]string{"warm-cache", "notify"}), "only the boundary's hooks, in list order")
		Expect(runner.lastEnv).To(ContainElement("CHINIT_ROLE=writer"))
		Expect(runner.lastEnv).To(ContainElement("CHINIT_HOSTNAME=writer-0"))
		Expect(runner.lastEnv).To(ContainElement("CHINIT_CLUSTER=demo"))
	})

	It("logs and continues past a non-critical failure", func() {
		runner.failOn = hookName
		err := service.RunHooks(ctx, runner, hooksWith(model.HookPostBoot, false), model.HookPostBoot, id, false)
		Expect(err).NotTo(HaveOccurred(), "a best-effort hook never aborts the boundary")
	})

	It("aborts the boot boundary on a critical failure", func() {
		runner.failOn = hookName
		err := service.RunHooks(ctx, runner, hooksWith(model.HookPreBoot, true), model.HookPreBoot, id, false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(hookName), "the error names the hook")
	})

	It("NEVER blocks a drain boundary, even on a critical failure", func() {
		runner.failOn = hookName
		err := service.RunHooks(ctx, runner, hooksWith(model.HookPreDrain, true), model.HookPreDrain, id, true)
		Expect(err).NotTo(HaveOccurred(), "the node is already going down — a hook may not block the drain")
	})

	It("is a no-op without hooks for the boundary", func() {
		err := service.RunHooks(ctx, runner, &model.Hooks{}, model.HookPostDrain, id, false)
		Expect(err).NotTo(HaveOccurred())
		Expect(runner.calls).To(BeEmpty())
		Expect(service.RunHooks(ctx, runner, nil, model.HookPostBoot, id, false)).NotTo(HaveOccurred())
	})
})

var _ = Describe("HooksFor", func() {
	It("filters by event, preserving list order", func() {
		hooks := &model.Hooks{Items: []model.Hook{
			{Event: model.HookPostBoot, Name: "a"},
			{Event: model.HookPreDrain, Name: "b"},
			{Event: model.HookPostBoot, Name: "c"},
		}}
		post := hooks.HooksFor(model.HookPostBoot)
		Expect(post).To(HaveLen(2))
		Expect(post[0].Name).To(Equal("a"))
		Expect(post[1].Name).To(Equal("c"))
	})

	It("answers nil for a nil section", func() {
		var hooks *model.Hooks
		Expect(hooks.HooksFor(model.HookPreBoot)).To(BeNil())
	})
})
