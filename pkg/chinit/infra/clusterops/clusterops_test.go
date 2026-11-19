// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusterops

import "testing"

func TestPrioritizeRunOnHost(t *testing.T) {
	hosts := []Host{
		{Name: "01-0", Address: "writer-0.ch-int-dev.clickhouse.internal"},
		{Name: "01-1", Address: "writer-1.ch-int-dev.clickhouse.internal"},
		{Name: "01-4", Address: "writer-4.ch-int-dev.clickhouse.internal"},
	}
	got := prioritizeRunOnHost(hosts, "writer-4")
	if len(got) != 3 || got[0].Address != "writer-4.ch-int-dev.clickhouse.internal" {
		t.Fatalf("expected writer-4 first, got %+v", got)
	}
	if got[1].Name != "01-0" || got[2].Name != "01-1" {
		t.Fatalf("unexpected remaining order: %+v", got)
	}
	// Original slice must remain unmodified.
	if hosts[0].Name != "01-0" {
		t.Fatalf("input slice mutated: %+v", hosts)
	}
}
