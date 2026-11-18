// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package compose_test

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Keeper ensemble", func() {
	It("answers ruok with imok on keeper-0", func() {
		// 4lw over the keeper client port. nc ships in the clickhouse
		// base image; the pipe half-closes the socket, which keeper's
		// 4lw needs to answer.
		out, err := env.run(env.args(
			"exec", "-T", "keeper-0", "bash", "-c",
			"echo ruok | timeout 5 nc 127.0.0.1 9181",
		))
		Expect(err).NotTo(HaveOccurred(), "keeper 4lw probe failed:\n%s", out)
		Expect(strings.TrimSpace(out)).To(Equal("imok"), "keeper ruok should answer imok, got %q", out)
	})
})
