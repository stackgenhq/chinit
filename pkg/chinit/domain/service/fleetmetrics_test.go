// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
)

// The fleet metrics renderer's contract: the exposition names, the
// per-phase task counters, the health/cordon counters, and the node's
// own staleness gauge — one source of truth the exporter and the
// CloudWatch bridge emit.
var _ = Describe("RenderFleetMetrics", func() {
	now := time.Unix(1794880000, 0)

	It("renders the liveness gauge with the identity labels", func() {
		out := service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname: "writer-0", Role: "writer", ClusterName: "demo", Now: now,
		})
		Expect(out).To(ContainSubstring(`chinit_up{cluster="demo",role="writer",hostname="writer-0"} 1`))
	})

	It("counts task rows by phase", func() {
		out := service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname: "writer-0", ClusterName: "demo", Now: now,
			Tasks: []port.FleetTask{
				{TaskID: "a", Phase: port.TaskPending},
				{TaskID: "b", Phase: port.TaskPending},
				{TaskID: "c", Phase: port.TaskExecuting},
				{TaskID: "d", Phase: port.TaskFailed},
			},
		})
		Expect(out).To(ContainSubstring(`chinit_fleet_tasks{cluster="demo",phase="pending"} 2`))
		Expect(out).To(ContainSubstring(`chinit_fleet_tasks{cluster="demo",phase="executing"} 1`))
		Expect(out).To(ContainSubstring(`chinit_fleet_tasks{cluster="demo",phase="failed"} 1`))
		Expect(out).To(ContainSubstring(`chinit_fleet_tasks{cluster="demo",phase="done"} 0`),
			"every phase renders — the alarm's zero is a value, not an absence")
	})

	It("counts healthy, degraded, and cordoned nodes", func() {
		out := service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname: "writer-0", ClusterName: "demo", Now: now,
			Statuses: []port.NodeStatus{
				{Hostname: "w0", Health: port.NodeHealthy},
				{Hostname: "w1", Health: port.NodeHealthy},
				{Hostname: "w2", Health: port.NodeDegraded},
				{Hostname: "w3", Health: port.NodeHealthy, Cordoned: true},
			},
		})
		Expect(out).To(ContainSubstring(`chinit_fleet_nodes_healthy{cluster="demo"} 2`))
		Expect(out).To(ContainSubstring(`chinit_fleet_nodes_degraded{cluster="demo"} 1`))
		Expect(out).To(ContainSubstring(`chinit_fleet_nodes_cordoned{cluster="demo"} 1`),
			"a cordoned node is counted cordoned, never degraded")
	})

	It("renders this node's status age gauge", func() {
		out := service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname: "writer-0", ClusterName: "demo", Now: now,
			Statuses: []port.NodeStatus{
				{Hostname: "writer-0", UpdatedAt: now.Unix() - 300},
			},
		})
		Expect(out).To(ContainSubstring(`chinit_fleet_status_age_seconds{cluster="demo",hostname="writer-0"} 300`))
	})

	It("clamps a future-dated row to zero (clock skew never alarms)", func() {
		out := service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname: "writer-0", ClusterName: "demo", Now: now,
			Statuses: []port.NodeStatus{
				{Hostname: "writer-0", UpdatedAt: now.Unix() + 60},
			},
		})
		Expect(out).To(ContainSubstring(`chinit_fleet_status_age_seconds{cluster="demo",hostname="writer-0"} 0`))
	})

	It("omits the age gauge when the node has no status row", func() {
		out := service.RenderFleetMetrics(service.FleetMetricsInput{
			Hostname: "writer-0", ClusterName: "demo", Now: now,
		})
		Expect(strings.Contains(out, "chinit_fleet_status_age_seconds")).To(BeFalse(),
			"no row = no gauge (the alarm keys on presence, not a zero)")
	})
})
