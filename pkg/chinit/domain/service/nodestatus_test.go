// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/fakes"
)

// The status layer's contract specs: publish/refuse semantics, the
// cordon flag's independence from the publish, and the TTL expiry —
// the scenarios the cloud adapters' suites mirror.
var _ = Describe("StatusPublisher (fakes reference)", func() {
	var (
		store *fakes.StatusPublisher
		ctx   context.Context
	)
	const table = "claims"

	BeforeEach(func() {
		store = fakes.NewStatusPublisher()
		ctx = context.Background()
	})

	It("publishes a new row and lists it", func() {
		err := store.PublishStatus(ctx, port.NodeStatusPublish{
			Table: table,
			Status: port.NodeStatus{
				Hostname: "writer-0", Role: "writer", InstanceID: "i-0",
				SpecHash: "abc123", ClickHouseVersion: "24.8.7.53", Health: port.NodeHealthy,
			},
		})
		Expect(err).NotTo(HaveOccurred())
		rows, err := store.ListStatus(ctx, port.NodeStatusList{Table: table})
		Expect(err).NotTo(HaveOccurred())
		Expect(rows).To(HaveLen(1))
		Expect(rows[0].Hostname).To(Equal("writer-0"))
		Expect(rows[0].ClickHouseVersion).To(Equal("24.8.7.53"))
		Expect(rows[0].Health).To(Equal(port.NodeHealthy))
	})

	It("refuses a foreign instance overwriting a fresh row (identity moved)", func() {
		err := store.PublishStatus(ctx, port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Health: port.NodeHealthy},
		})
		Expect(err).NotTo(HaveOccurred())
		err = store.PublishStatus(ctx, port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-1", Health: port.NodeHealthy},
		})
		Expect(err).To(HaveOccurred(), "a replacement claiming the name mid-lease must not silently own the status row")
	})

	It("lets the same instance refresh its own row", func() {
		publish := port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Health: port.NodeHealthy},
		}
		Expect(store.PublishStatus(ctx, publish)).To(Succeed())
		publish.Status.ClickHouseVersion = "25.3.1"
		Expect(store.PublishStatus(ctx, publish)).To(Succeed())
		rows, _ := store.ListStatus(ctx, port.NodeStatusList{Table: table})
		Expect(rows[0].ClickHouseVersion).To(Equal("25.3.1"))
	})

	It("keeps the cordon flag across a publish (the toggle owns the flag)", func() {
		Expect(store.PublishStatus(ctx, port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Cordoned: false, Health: port.NodeHealthy},
		})).To(Succeed())
		Expect(store.SetCordon(ctx, port.NodeCordonRequest{Table: table, Hostname: "writer-0", Cordoned: true})).To(Succeed())
		// The next publish must NOT clear the operator's flag.
		Expect(store.PublishStatus(ctx, port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Cordoned: false, Health: port.NodeHealthy},
		})).To(Succeed())
		rows, _ := store.ListStatus(ctx, port.NodeStatusList{Table: table})
		Expect(rows[0].Cordoned).To(BeTrue(), "a cordon survives the node's own publish")
	})

	It("errors on cordon of a node that never published", func() {
		err := store.SetCordon(ctx, port.NodeCordonRequest{Table: table, Hostname: "writer-9", Cordoned: true})
		Expect(err).To(HaveOccurred(), "cordon before first publish is a wiring mistake, loud by design")
	})

	It("filters by hostname and sorts", func() {
		for _, host := range []string{"writer-1", "writer-0", "reader-0"} {
			Expect(store.PublishStatus(ctx, port.NodeStatusPublish{
				Table:  table,
				Status: port.NodeStatus{Hostname: host, InstanceID: "i-" + host, Health: port.NodeHealthy},
			})).To(Succeed())
		}
		one, _ := store.ListStatus(ctx, port.NodeStatusList{Table: table, Hostname: "reader-0"})
		Expect(one).To(HaveLen(1))
		all, _ := store.ListStatus(ctx, port.NodeStatusList{Table: table})
		Expect(all).To(HaveLen(3))
		Expect(all[0].Hostname).To(Equal("reader-0"))
		Expect(all[1].Hostname).To(Equal("writer-0"))
		Expect(all[2].Hostname).To(Equal("writer-1"))
	})

	It("ages a row out with its node's lease window", func() {
		base := time.Now()
		clock := base
		store = fakes.NewStatusPublisher().WithNow(func() time.Time { return clock })
		Expect(store.PublishStatus(ctx, port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Health: port.NodeHealthy},
		})).To(Succeed())
		// Advance past the TTL: the foreign-instance guard no longer
		// applies — a replacement may take the row.
		clock = base.Add(20 * time.Minute)
		err := store.PublishStatus(ctx, port.NodeStatusPublish{
			Table:  table,
			Status: port.NodeStatus{Hostname: "writer-0", InstanceID: "i-1", Health: port.NodeHealthy},
		})
		Expect(err).NotTo(HaveOccurred(), "an aged-out row is reclaimable — the dead node's status does not wedge the name")
	})
})
