// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package compose_test

import (
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Cluster topology", func() {
	// The compose-demo cluster must be registered on BOTH nodes with
	// both replicas — the remote_servers rendering, lease-claimed
	// identity and DNS aliases all have to be right for ClickHouse to
	// see writer-0.ch.local and reader-0.ch.local as one cluster.
	DescribeTable("node lists the cluster with both replicas",
		func(node string) {
			query := fmt.Sprintf(
				"SELECT count(), countIf(is_local) FROM system.clusters WHERE cluster = '%s'",
				env.clusterName)
			out := env.chQueryOK(node, query)
			fields := strings.Fields(out)
			Expect(fields).To(HaveLen(2), "%s: expected 2 columns, got %q", node, out)
			if fields[0] != "2" || fields[1] != "1" {
				env.dumpDiagnostics()
			}
			Expect(fields[0]).To(Equal("2"),
				"%s: cluster %q should list 2 replicas; got %q", node, env.clusterName, out)
			Expect(fields[1]).To(Equal("1"),
				"%s: exactly one replica should be local; got %q", node, out)
		},
		Entry("writer-0", "writer-0"),
		Entry("reader-0", "reader-0"),
	)
})
