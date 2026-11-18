// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// The keeper-quorum status bit (the 2026-11-17 soak's cascade
// finding): the status row must distinguish "readonly because the
// keeper ensemble is unreachable" from "the node itself failed" —
// the ASG replaced healthy nodes whose only symptom was the keeper
// outage's fleet-wide readonly. The probe's contracts:
//   - a keeper probes LOCALHOST (its own mntr up IS its vote);
//   - a server node probes keeper-0's DNS address from the spec;
//   - an unreadable spec answers false WITHOUT probing (no known
//     keeper address);
//   - a probe failure answers false, never an error — the false row
//     IS the finding.
var _ = Describe("the keeper-quorum status bit", func() {
	var fake *fakeClusterOps

	BeforeEach(func() {
		fake = &fakeClusterOps{}
	})

	It("probes localhost on a keeper node", func() {
		st := &nodeState{}
		st.Identity.Role = model.RoleKeeper
		st.Identity.Hostname = "keeper-0"

		Expect(probeKeeperQuorumWith(context.Background(), st, fake)).To(BeTrue())
		Expect(fake.mntrCalls).To(HaveLen(1))
		Expect(fake.mntrCalls[0].Address).To(Equal("localhost:9181"))
	})

	It("answers false when the probe fails", func() {
		fake.mntrErr = errors.New("keeper unreachable")
		st := &nodeState{}
		st.Identity.Role = model.RoleKeeper

		Expect(probeKeeperQuorumWith(context.Background(), st, fake)).To(BeFalse())
	})

	It("answers false on a server node when the spec is unreadable (no known keeper address)", func() {
		st := &nodeState{}
		st.Identity.Role = model.RoleWriter
		st.Identity.Hostname = "writer-1"
		// loadInventorySpec reads the default bundle path; on a test
		// host it is absent — the unreadable-spec contract answers
		// false without probing.
		Expect(probeKeeperQuorumWith(context.Background(), st, fake)).To(BeFalse())
		Expect(fake.mntrCalls).To(BeEmpty())
	})
})
