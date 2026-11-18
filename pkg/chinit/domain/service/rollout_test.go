// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
)

// The AMI rollout coordinator's contract specs: the version gate, the
// one-outstanding-task rule, the cordon skip, and the ordinal
// ordering — the operator's rollout parity at the VM level.
var _ = Describe("EvaluateRollout", func() {
	status := func(host, version string, cordoned bool) port.NodeStatus {
		return port.NodeStatus{Hostname: host, Role: "writer", ClickHouseVersion: version, Cordoned: cordoned, Health: port.NodeHealthy}
	}

	It("reports complete when every non-cordoned node reports the target", func() {
		state := service.EvaluateRollout(service.RolloutInput{
			TargetVersion: "25.3.1",
			Statuses: []port.NodeStatus{
				status("writer-0", "25.3.1", false),
				status("writer-1", "25.3.1", false),
				status("writer-2", "23.8.1", true), // cordoned: does not block
			},
		})
		Expect(state.Complete).To(BeTrue(), "a cordoned node never blocks the rollout")
	})

	It("reports incomplete while any non-cordoned node is off-target", func() {
		state := service.EvaluateRollout(service.RolloutInput{
			TargetVersion: "25.3.1",
			Statuses: []port.NodeStatus{
				status("writer-0", "25.3.1", false),
				status("writer-1", "24.8.7", false),
			},
		})
		Expect(state.Complete).To(BeFalse())
	})

	It("ignores other roles' rows when the role filter is set", func() {
		state := service.EvaluateRollout(service.RolloutInput{
			TargetVersion: "25.3.1",
			Role:          "writer",
			Statuses: []port.NodeStatus{
				status("writer-0", "25.3.1", false),
				{Hostname: "reader-0", Role: "reader", ClickHouseVersion: "23.8.1", Health: port.NodeHealthy},
			},
		})
		Expect(state.Nodes).To(HaveLen(1))
		Expect(state.Complete).To(BeTrue())
	})

	It("marks nodes with outstanding rollout tasks", func() {
		state := service.EvaluateRollout(service.RolloutInput{
			TargetVersion: "25.3.1",
			Statuses:      []port.NodeStatus{status("writer-0", "24.8.7", false)},
			Tasks: []port.FleetTask{
				{TaskID: "rollout-writer-0", Type: port.TaskRollout, Target: "writer-0", Phase: port.TaskExecuting},
			},
		})
		Expect(state.Nodes[0].HasTask).To(BeTrue())
	})

	It("ignores terminal task rows (the residue a submit replaces)", func() {
		state := service.EvaluateRollout(service.RolloutInput{
			TargetVersion: "25.3.1",
			Statuses:      []port.NodeStatus{status("writer-0", "24.8.7", false)},
			Tasks: []port.FleetTask{
				{TaskID: "rollout-old", Type: port.TaskRollout, Target: "writer-0", Phase: port.TaskDone},
			},
		})
		Expect(state.Nodes[0].HasTask).To(BeFalse(), "a done rollout task gates nothing")
	})
})

var _ = Describe("NextRolloutDispatch", func() {
	It("answers the lowest-ordinal off-target node", func() {
		state := service.RolloutState{Nodes: []service.RolloutProgress{
			{Hostname: "writer-10", OnTarget: false},
			{Hostname: "writer-2", OnTarget: false},
			{Hostname: "writer-0", OnTarget: true},
		}}
		Expect(service.NextRolloutDispatch(state)).To(Equal("writer-2"),
			"numeric ordinal order: writer-2 before writer-10")
	})

	It("answers nothing while a task is outstanding (one at a time)", func() {
		state := service.RolloutState{Nodes: []service.RolloutProgress{
			{Hostname: "writer-0", OnTarget: true},
			{Hostname: "writer-1", OnTarget: false, HasTask: true},
			{Hostname: "writer-2", OnTarget: false},
		}}
		Expect(service.NextRolloutDispatch(state)).To(BeEmpty(),
			"the previous node's verdict gates the next dispatch")
	})

	It("skips cordoned nodes", func() {
		state := service.RolloutState{Nodes: []service.RolloutProgress{
			{Hostname: "writer-0", OnTarget: false, Cordoned: true},
			{Hostname: "writer-1", OnTarget: false},
		}}
		Expect(service.NextRolloutDispatch(state)).To(Equal("writer-1"))
	})

	It("answers nothing when the rollout is complete", func() {
		state := service.RolloutState{
			Complete: true,
			Nodes: []service.RolloutProgress{
				{Hostname: "writer-0", OnTarget: true},
			},
		}
		Expect(service.NextRolloutDispatch(state)).To(BeEmpty())
	})
})

var _ = Describe("KeeperRolloutGate", func() {
	It("allows a healthy 3-keeper ensemble", func() {
		err := service.KeeperRolloutGate(3, 2, true)
		Expect(err).NotTo(HaveOccurred(), "3 voting members, leader + 2 synced: quorum survives the removal")
	})

	It("refuses when no leader is reported (degraded ensemble)", func() {
		err := service.KeeperRolloutGate(3, 2, false)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no leader"))
	})

	It("refuses a sub-quorum healthy set", func() {
		err := service.KeeperRolloutGate(5, 1, true)
		Expect(err).To(HaveOccurred(), "leader + 1 synced < quorum 3: the ensemble is degraded")
		Expect(err.Error()).To(ContainSubstring("degraded"))
	})

	It("refuses when the post-removal quorum would fail", func() {
		// 4 voting members: removing one leaves 3, quorum 2 — healthy
		// set after removal (leader+synced-1) must still be ≥ 2.
		err := service.KeeperRolloutGate(4, 1, true)
		Expect(err).To(HaveOccurred(), "leader + 1 synced: healthy 2 = quorum 3 is already short")
		Expect(err.Error()).To(ContainSubstring("quorum"))
	})

	It("NEVER rolls a 1- or 2-keeper ensemble", func() {
		err := service.KeeperRolloutGate(1, 0, true)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot survive"))
		err = service.KeeperRolloutGate(2, 1, true)
		Expect(err).To(HaveOccurred(), "a 2-keeper roll is a standing outage")
	})
})
