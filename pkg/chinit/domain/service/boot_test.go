// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This file pins StaticHostname — the fixed-offset octet map the
// STATIC identity path (keepers, backup, and the interim static-writer
// path) derives its hostname from. Spec validation lives in the model
// suite (pkg/chinit/domain/model/spec_test.go, the single home of the
// Validate table) and the CR-name arithmetic in ceiling_test.go;
// neither is re-asserted here.

var _ = Describe("StaticHostname", func() {
	// The keeper server_id garbage bug lives here if the map drifts:
	// an ip-* name parses garbage into the raft identity.
	DescribeTable("maps the private IP's last octet to the role bands",
		func(ip, hostname string, ordinal int) {
			name, ord := StaticHostname(ip)
			Expect(name).To(Equal(hostname))
			Expect(ord).To(Equal(ordinal))
		},
		Entry("writer band starts at +10", "10.0.1.10", "writer-0", 0),
		Entry("writer band ends at +19", "10.0.1.19", "writer-9", 9),
		Entry("keeper band starts at +20", "10.0.1.20", "keeper-0", 0),
		Entry("keeper band ends at +29", "10.0.1.29", "keeper-9", 9),
		Entry("backup sits at +30", "10.0.1.30", "backup-0", 0),
		Entry("below every band is unmapped", "10.0.1.5", "", -1),
		Entry("beyond backup is unmapped", "10.0.1.31", "", -1),
		Entry("an unparseable IP is unmapped", "not-an-ip", "", -1),
	)
})
