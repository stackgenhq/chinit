// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package compose_test

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// marker is the per-run row source: each suite run writes its own row,
// so a re-run on a reused cluster can never pass on a previous run's
// data.
var marker = fmt.Sprintf("e2e-%d", time.Now().UnixNano())

var _ = Describe("Write on writer, read from reader", func() {
	It("replicates a row written on writer-0 to reader-0", func() {
		By("writing the marker row on writer-0")
		insert := fmt.Sprintf(
			"INSERT INTO default.events (source, message) VALUES ('%s', 'e2e write-read acceptance')",
			marker)
		env.chQueryOK("writer-0", insert)

		By("reading the marker back on reader-0 (replication lag tolerance)")
		selectQ := fmt.Sprintf("SELECT message FROM default.events WHERE source = '%s'", marker)
		// Replication is asynchronous; 90s covers a slow local machine.
		var readerSaw string
		Eventually(func() string {
			out, err := env.chQuery("reader-0", selectQ)
			if err != nil {
				return "" // transient: the reader may still be converging
			}
			readerSaw = out
			return out
		}, 90*time.Second, 2*time.Second).Should(ContainSubstring("e2e write-read acceptance"),
			"reader-0 never saw marker %q — replication from writer-0 broken", marker)
		Expect(readerSaw).NotTo(BeEmpty())
	})

	It("reports the events replica writable with all replicas active", func() {
		// The fleet's long-run posture, not just this run's row: the
		// events table on the reader must be writable (keeper
		// connectivity intact) and see every replica as active.
		out := env.chQueryOK("reader-0",
			"SELECT count(), countIf(is_readonly), countIf(active_replicas != total_replicas) "+
				"FROM system.replicas WHERE database = 'default' AND table = 'events'")
		fields := strings.Fields(out)
		Expect(fields).To(HaveLen(3), "replica health query returned %q", out)
		if fields[0] == "0" || fields[1] != "0" || fields[2] != "0" {
			env.dumpDiagnostics()
		}
		Expect(fields[0]).NotTo(Equal("0"), "no healthy events replica on reader-0 (out %q)", out)
		Expect(fields[1]).To(Equal("0"), "events replica is READONLY on reader-0 — keeper connectivity broken (out %q)", out)
		Expect(fields[2]).To(Equal("0"), "not all replicas active on reader-0 (out %q)", out)
	})
})
