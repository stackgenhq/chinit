// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
)

// The disruption budget's contract specs: the writer floor, the
// missing-data defer, and the cordon exclusion — the PDB-equivalent
// rules every voluntary path gates on.
var _ = Describe("CheckDisruptionBudget", func() {
	It("allows a writer disruption when the floor survives", func() {
		v := service.CheckDisruptionBudget(service.BudgetInput{
			HealthyWriters: 3, TargetRole: model.RoleWriter,
		})
		Expect(v.Allowed).To(BeTrue())
	})

	It("defers the last healthy writer (the default floor is 1)", func() {
		v := service.CheckDisruptionBudget(service.BudgetInput{
			HealthyWriters: 1, TargetRole: model.RoleWriter,
		})
		Expect(v.Allowed).To(BeFalse())
		Expect(v.Reason).To(ContainSubstring("writer floor"))
	})

	It("defers when no status rows are visible (never guess)", func() {
		v := service.CheckDisruptionBudget(service.BudgetInput{
			HealthyWriters: 0, TargetRole: model.RoleWriter,
		})
		Expect(v.Allowed).To(BeFalse())
		Expect(v.Reason).To(ContainSubstring("status rows missing"))
	})

	It("honors the spec's explicit floor", func() {
		v := service.CheckDisruptionBudget(service.BudgetInput{
			Budget:         model.DisruptionBudget{MinHealthyWriters: 2},
			HealthyWriters: 2, TargetRole: model.RoleWriter,
		})
		Expect(v.Allowed).To(BeFalse(), "2 healthy - 1 = 1 < floor 2")
		v = service.CheckDisruptionBudget(service.BudgetInput{
			Budget:         model.DisruptionBudget{MinHealthyWriters: 2},
			HealthyWriters: 3, TargetRole: model.RoleWriter,
		})
		Expect(v.Allowed).To(BeTrue())
	})

	It("gates readers only when the spec sets a reader floor", func() {
		Expect(service.CheckDisruptionBudget(service.BudgetInput{
			HealthyReaders: 1, TargetRole: model.RoleReader,
		}).Allowed).To(BeTrue(), "no reader floor = readers are disposable")
		Expect(service.CheckDisruptionBudget(service.BudgetInput{
			Budget:         model.DisruptionBudget{MinHealthyReaders: 2},
			HealthyReaders: 2, TargetRole: model.RoleReader,
		}).Allowed).To(BeFalse())
	})

	It("never gates keepers or the backup (their paths own their own safety)", func() {
		Expect(service.CheckDisruptionBudget(service.BudgetInput{
			TargetRole: model.RoleKeeper,
		}).Allowed).To(BeTrue())
		Expect(service.CheckDisruptionBudget(service.BudgetInput{
			TargetRole: model.RoleBackup,
		}).Allowed).To(BeTrue())
	})
})

var _ = Describe("CountHealthyByRole", func() {
	It("counts healthy non-cordoned rows by role", func() {
		rows := []port.NodeStatus{
			{Hostname: "w0", Role: "writer", Health: port.NodeHealthy},
			{Hostname: "w1", Role: "writer", Health: port.NodeHealthy},
			{Hostname: "w2", Role: "writer", Health: port.NodeHealthy, Cordoned: true},
			{Hostname: "w3", Role: "writer", Health: port.NodeDegraded},
			{Hostname: "r0", Role: "reader", Health: port.NodeHealthy},
		}
		writers, readers := service.CountHealthyByRole(rows)
		Expect(writers).To(Equal(2), "cordoned and degraded nodes never count toward the floor")
		Expect(readers).To(Equal(1))
	})
})
