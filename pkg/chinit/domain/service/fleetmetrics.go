// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// FleetMetricsInput is the renderer's data: the fleet view one render
// call consumes (the exporter service gathers it on each scrape; the
// CloudWatch bridge gathers it on its tick).
type FleetMetricsInput struct {
	// Hostname labels every metric with the node's identity.
	Hostname string
	// Role is the node's role label.
	Role string
	// ClusterName is the cluster label (the alerting dimension).
	ClusterName string
	// Tasks is the fleet's task rows (all phases; the counters derive
	// per-phase).
	Tasks []port.FleetTask
	// Statuses is the fleet's status rows (health + cordon counters).
	Statuses []port.NodeStatus
	// Now is the render clock (staleness gauges).
	Now time.Time
}

// RenderFleetMetrics produces the Prometheus exposition text. Pure
// over the input — the exporter service and the tests share it.
//
// The metric set (one source of truth; the CloudWatch bridge emits
// the same core counters):
//
//	chinit_fleet_tasks{phase=...}      — the task rows, by phase
//	chinit_fleet_nodes_healthy        — healthy status rows
//	chinit_fleet_nodes_degraded        — degraded status rows
//	chinit_fleet_nodes_cordoned       — cordoned status rows
//	chinit_fleet_status_age_seconds   — this node's row age (staleness)
//	chinit_up                          — the exporter's own liveness
func RenderFleetMetrics(in FleetMetricsInput) string {
	var b strings.Builder
	// The HELP/TYPE lines document the contract at the scrape target.
	b.WriteString("# HELP chinit_up The chinit fleet exporter's liveness (1 = serving).\n")
	b.WriteString("# TYPE chinit_up gauge\n")
	fmt.Fprintf(&b, "chinit_up{cluster=%q,role=%q,hostname=%q} 1\n", in.ClusterName, in.Role, in.Hostname)

	phaseCounts := map[port.FleetTaskPhase]int{}
	for _, t := range in.Tasks {
		phaseCounts[t.Phase]++
	}
	phases := make([]string, 0, len(phaseCounts))
	for p := range phaseCounts {
		phases = append(phases, string(p))
	}
	sort.Strings(phases)
	b.WriteString("# HELP chinit_fleet_tasks The fleet task rows, by phase.\n")
	b.WriteString("# TYPE chinit_fleet_tasks gauge\n")
	for _, p := range []string{"pending", "ack", "executing", "done", "failed"} {
		fmt.Fprintf(&b, "chinit_fleet_tasks{cluster=%q,phase=%q} %d\n", in.ClusterName, p, phaseCounts[port.FleetTaskPhase(p)])
	}

	var healthy, degraded, cordoned int
	for _, s := range in.Statuses {
		if s.Cordoned {
			cordoned++
			continue
		}
		if s.Health == port.NodeHealthy {
			healthy++
		} else {
			degraded++
		}
	}
	b.WriteString("# HELP chinit_fleet_nodes_healthy Healthy (non-cordoned) node status rows.\n")
	b.WriteString("# TYPE chinit_fleet_nodes_healthy gauge\n")
	fmt.Fprintf(&b, "chinit_fleet_nodes_healthy{cluster=%q} %d\n", in.ClusterName, healthy)
	b.WriteString("# HELP chinit_fleet_nodes_degraded Degraded node status rows.\n")
	b.WriteString("# TYPE chinit_fleet_nodes_degraded gauge\n")
	fmt.Fprintf(&b, "chinit_fleet_nodes_degraded{cluster=%q} %d\n", in.ClusterName, degraded)
	b.WriteString("# HELP chinit_fleet_nodes_cordoned Cordoned node status rows.\n")
	b.WriteString("# TYPE chinit_fleet_nodes_cordoned gauge\n")
	fmt.Fprintf(&b, "chinit_fleet_nodes_cordoned{cluster=%q} %d\n", in.ClusterName, cordoned)

	// This node's own status age (the staleness alarm's input).
	for _, s := range in.Statuses {
		if s.Hostname == in.Hostname {
			age := in.Now.Unix() - s.UpdatedAt
			if age < 0 {
				age = 0
			}
			b.WriteString("# HELP chinit_fleet_status_age_seconds This node's status row age.\n")
			b.WriteString("# TYPE chinit_fleet_status_age_seconds gauge\n")
			fmt.Fprintf(&b, "chinit_fleet_status_age_seconds{cluster=%q,hostname=%q} %d\n", in.ClusterName, in.Hostname, age)
			break
		}
	}
	return b.String()
}
