// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package compose_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Boot convergence", func() {
	// Both nodes must have completed chinit-boot (the healthchecks gate
	// on the marker too, but the suite asserts it directly so a
	// healthcheck regression cannot mask a boot that never finished),
	// and clickhouse must be serving SQL.
	DescribeTable("node finished chinit-boot and serves SQL",
		func(node string) {
			By("checking the boot-finished marker")
			out, err := env.run(env.args("exec", "-T", node, "test", "-f", "/var/lib/cloud/instance/boot-finished"))
			if err != nil {
				env.dumpNodeLogs(node)
				Fail(node + ": boot-finished marker missing — chinit-boot never completed:\n" + out)
			}

			By("pinging SQL over the node's clickhouse-client")
			ping := env.chQueryOK(node, "SELECT 1")
			Expect(ping).To(Equal("1"), node+": SQL ping returned %q", ping)
		},
		Entry("writer-0", "writer-0"),
		Entry("reader-0", "reader-0"),
	)
})
