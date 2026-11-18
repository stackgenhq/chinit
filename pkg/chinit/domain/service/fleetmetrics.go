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

// FleetTasks is a domain collection of FleetTask rows.
type FleetTasks []port.FleetTask

// phaseCounts tallies tasks by phase.
func (tasks FleetTasks) phaseCounts() map[port.FleetTaskPhase]int {
	counts := map[port.FleetTaskPhase]int{}
	for _, t := range tasks {
		counts[t.Phase]++
	}
	return counts
}

// NodeStatuses is a domain collection of NodeStatus rows.
type NodeStatuses []port.NodeStatus

// healthCounts tallies healthy, degraded, and cordoned nodes.
func (statuses NodeStatuses) healthCounts() (healthy, degraded, cordoned int) {
	for _, s := range statuses {
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
	return healthy, degraded, cordoned
}

// statusAgeSeconds returns the staleness age in seconds for hostname's
// status row, clamped at 0 for clock skew.
func (statuses NodeStatuses) statusAgeSeconds(hostname string, now time.Time) (int64, bool) {
	for _, s := range statuses {
		if s.Hostname == hostname {
			age := now.Unix() - s.UpdatedAt
			if age < 0 {
				age = 0
			}
			return age, true
		}
	}
	return 0, false
}

// CASMetrics carries Altinity Antalya Content-Addressable Storage (CAS)
// mount and garbage-collector health gauges probed from
// system.cas_mounts and system.cas_gc_log when CAS is enabled.
type CASMetrics struct {
	// Enabled reports whether CAS telemetry is active on this node.
	Enabled bool
	// LiveMounts is the count of CAS mounts reporting live state in
	// system.cas_mounts.
	LiveMounts int
	// WedgedNamespaces is sum(wedged_namespace_count) from
	// system.cas_mounts (> 0 indicates GC wedge needing investigation).
	WedgedNamespaces int
	// GCErrors is the count of recent Error entries in system.cas_gc_log.
	GCErrors int
}

// renderPrometheus appends the CAS health gauges when CAS is enabled.
func (c CASMetrics) renderPrometheus(b *strings.Builder, cluster, hostname string) {
	if !c.Enabled {
		return
	}
	b.WriteString("# HELP chinit_cas_mounts_live Live Content-Addressable Storage (CAS) mounts on this node.\n")
	b.WriteString("# TYPE chinit_cas_mounts_live gauge\n")
	fmt.Fprintf(b, "chinit_cas_mounts_live{cluster=%q,hostname=%q} %d\n", cluster, hostname, c.LiveMounts)
	b.WriteString("# HELP chinit_cas_wedged_namespaces Wedged CAS GC namespaces in system.cas_mounts (> 0 requires investigation).\n")
	b.WriteString("# TYPE chinit_cas_wedged_namespaces gauge\n")
	fmt.Fprintf(b, "chinit_cas_wedged_namespaces{cluster=%q,hostname=%q} %d\n", cluster, hostname, c.WedgedNamespaces)
	b.WriteString("# HELP chinit_cas_gc_errors_total Recent CAS garbage-collector error entries in system.cas_gc_log.\n")
	b.WriteString("# TYPE chinit_cas_gc_errors_total gauge\n")
	fmt.Fprintf(b, "chinit_cas_gc_errors_total{cluster=%q,hostname=%q} %d\n", cluster, hostname, c.GCErrors)
}

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
	Tasks FleetTasks
	// Statuses is the fleet's status rows (health + cordon counters).
	Statuses NodeStatuses
	// CAS carries optional Altinity Antalya CAS mount and GC metrics.
	CAS CASMetrics
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
//	chinit_cas_mounts_live            — live CAS mounts (when CAS enabled)
//	chinit_cas_wedged_namespaces      — wedged CAS GC namespaces
//	chinit_cas_gc_errors_total        — CAS GC errors in system.cas_gc_log
//	chinit_up                          — the exporter's own liveness
func RenderFleetMetrics(in FleetMetricsInput) string {
	var b strings.Builder
	// The HELP/TYPE lines document the contract at the scrape target.
	b.WriteString("# HELP chinit_up The chinit fleet exporter's liveness (1 = serving).\n")
	b.WriteString("# TYPE chinit_up gauge\n")
	fmt.Fprintf(&b, "chinit_up{cluster=%q,role=%q,hostname=%q} 1\n", in.ClusterName, in.Role, in.Hostname)

	phaseCounts := in.Tasks.phaseCounts()
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

	healthy, degraded, cordoned := in.Statuses.healthCounts()
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
	if age, ok := in.Statuses.statusAgeSeconds(in.Hostname, in.Now); ok {
		b.WriteString("# HELP chinit_fleet_status_age_seconds This node's status row age.\n")
		b.WriteString("# TYPE chinit_fleet_status_age_seconds gauge\n")
		fmt.Fprintf(&b, "chinit_fleet_status_age_seconds{cluster=%q,hostname=%q} %d\n", in.ClusterName, in.Hostname, age)
	}

	in.CAS.renderPrometheus(&b, in.ClusterName, in.Hostname)
	return b.String()
}
