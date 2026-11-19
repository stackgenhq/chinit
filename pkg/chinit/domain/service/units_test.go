// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/system"
)

// This suite pins the recurring-unit matrix for writer ASG (A5): which
// units install for which role/identity-mode combination, and the
// descriptions the timers log. chinitUnits is the pure assembler
// installUnits only writes what it returns, so the matrix specs need
// no filesystem.
//
// The contracts:
// - lease-renew is IDENTITY-mode gated, never role gated: lease
// writers and lease readers both install it (a role gate would
// let a leased writer's 15-min TTL expire and another instance
// claim its ordinal mid-flight).
// - the maintenance units install on EVERY writer: with ASG writers
// the ordinal-0 designation is a lease, not a machine, so the
// unit set must survive ordinal handoffs. The subcommands no-op
// on non-ordinal-0 holders (the app package's specs pin that
// gate); readers never install them.

// unitNames renders the assembler's answer as a name list.
func unitNames(units []system.UnitFile) []string {
	return unitFiles(units).names()
}

var _ = Describe("chinitUnits", func() {
	// baseSpec is the leased-writer shape; the matrix mutates role and
	// identity mode.
	baseSpec := func() model.BootSpec {
		spec := ceilingSpec()
		spec.IdentityMode = model.IdentityLease
		return spec
	}

	Context("writer, lease identity", func() {
		It("installs lease-renew and the maintenance units alongside the shared server units", func() {
			b := &Boot{Spec: &model.BootSpec{}}
			*b.Spec = baseSpec()
			Expect(b.Spec.Role).To(Equal(model.RoleWriter))

			names := unitNames(b.chinitUnits())
			Expect(names).To(ContainElements(
				"lease-renew.service",
				"lease-renew.timer",
				"dead-replica-gc.service",
				"dead-replica-gc.timer",
				"orphaned-data-gc.service",
				"orphaned-data-gc.timer",
				"queue-heal.service",
				"queue-heal.timer",
				// The vertical autoscale coordinator pair (the Lambda's
				// replacement): the subcommand gates on the ordinal-0
				// maintenance designation (with the vacancy election).
				"autoscale.service",
				"autoscale.timer",
				"schema-sync.service",
				"replica-sync.service",
			))
		})

		It("describes the maintenance jobs as ordinal-0-gated", func() {
			// The unit description is what `systemctl list-units` and
			// the boot log print — the operator's first clue why a
			// non-0 writer's timer ran without dropping anything.
			b := &Boot{Spec: &model.BootSpec{}}
			*b.Spec = baseSpec()
			for _, u := range b.chinitUnits() {
				switch u.Name {
				case "dead-replica-gc.service", "orphaned-data-gc.service":
					Expect(u.Content).To(ContainSubstring("ordinal-0 writer only"))
				case "queue-heal.service":
					// queue-heal is per-node self-healing (each writer
					// heals its OWN queue — the incident's stuck queue
					// sat on writer-1 while writer-0 held the ordinal-0
					// designation), so it must NOT carry the gate.
					Expect(u.Content).NotTo(ContainSubstring("ordinal-0"))
				case "autoscale.service":
					// The fleet roll is a ONE-DECISION action (two
					// holders stepping the ladder in one window is the
					// free-for-all the coordinator pattern forbids), so
					// the description names the gate.
					Expect(u.Content).To(ContainSubstring("ordinal-0 writer"))
				}
			}
		})
		It("installs backup-wake timer on writers when backup.on_demand is enabled", func() {
			spec := baseSpec()
			spec.Backup.BackupReplicaHostname = "backup-0"
			spec.Backup.OnDemand = true
			spec.Backup.Schedule = "0 2 * * *"
			b := &Boot{Spec: &spec}

			units := b.chinitUnits()
			names := unitNames(units)
			Expect(names).To(ContainElements("backup-wake.service", "backup-wake.timer"))
			for _, u := range units {
				if u.Name == "backup-wake.service" {
					Expect(u.Content).To(ContainSubstring("ordinal-0 writer only"))
					Expect(u.Content).To(ContainSubstring("ExecStart=/usr/local/bin/chinit backup-wake"))
				}
				if u.Name == "backup-wake.timer" {
					Expect(u.Content).To(ContainSubstring("OnCalendar=*-*-* 02:00:00"))
				}
			}
		})

		It("installs span-ship timer and renders VictoriaMetrics/VictoriaTraces otelcol pipelines when configured", func() {
			spec := baseSpec()
			spec.LogShipping = model.LogShipping{
				Agent:           "otel",
				LogsEndpoint:    "https://vl.example.com/insert/opentelemetry/v1/logs",
				MetricsEndpoint: "https://vm.example.com/api/v1/write",
				TracesEndpoint:  "https://vt.example.com/insert/opentelemetry/v1/traces",
				Headers:         "Authorization=Bearer tok",
			}
			b := &Boot{Spec: &spec}
			units := b.chinitUnits()
			Expect(unitNames(units)).To(ContainElements("span-ship.service", "span-ship.timer"))

			cfg := NewOTelCollectorRenderer(&spec, "writer-0", units).Render()
			Expect(cfg).To(ContainSubstring("logs_endpoint: https://vl.example.com/insert/opentelemetry/v1/logs"))
			Expect(cfg).To(ContainSubstring("prometheusremotewrite:\n    endpoint: https://vm.example.com/api/v1/write"))
			Expect(cfg).To(ContainSubstring("traces_endpoint: https://vt.example.com/insert/opentelemetry/v1/traces"))
			Expect(cfg).To(ContainSubstring("targets: [\"127.0.0.1:9363\"]"))
			Expect(cfg).To(ContainSubstring("targets: [\"127.0.0.1:9150\"]"))
		})
	})

	Context("CronToCalendar", func() {
		It("converts standard 5-field cron expressions to systemd OnCalendar format", func() {
			Expect(model.CronToCalendar("")).To(Equal("*-*-* 02:00:00"))
			Expect(model.CronToCalendar("0 2 * * *")).To(Equal("*-*-* 02:00:00"))
			Expect(model.CronToCalendar("30 4 * * 1-5")).To(Equal("Mon..Fri *-*-* 04:30:00"))
			Expect(model.CronToCalendar("*-*-* 03:15:00")).To(Equal("*-*-* 03:15:00"))
		})
	})

	Context("writer, static identity (the legacy writer path)", func() {
		It("installs the maintenance units but no lease-renew", func() {
			// Static writers keep the writer-0 machine contract (no
			// lease to renew) — the maintenance units still install
			// and the hostname gate still holds (writer-0 is the
			// machine's name in this mode).
			spec := baseSpec()
			spec.IdentityMode = model.IdentityStatic
			spec.WriterMax = 0 // static writers carry no ceiling
			b := &Boot{Spec: &spec}

			names := unitNames(b.chinitUnits())
			Expect(names).To(ContainElements("dead-replica-gc.service", "orphaned-data-gc.service"))
			Expect(names).NotTo(ContainElement("lease-renew.service"))
			Expect(names).NotTo(ContainElement("lease-renew.timer"))
			// The queue-heal unit travels with the maintenance set:
			// static writers run ingest too, their queues can stick.
			Expect(names).To(ContainElement("queue-heal.service"))
		})
	})

	Context("reader, lease identity", func() {
		It("installs lease-renew and disk-autoscale but never the maintenance units", func() {
			spec := baseSpec()
			spec.Role = model.RoleReader
			spec.WriterMax = 0
			spec.ReaderMax = 10
			spec.ReaderCount = 3
			b := &Boot{Spec: &spec}

			names := unitNames(b.chinitUnits())
			Expect(names).To(ContainElements("lease-renew.service", "lease-renew.timer"))
			// Self-healing contract: every server node with a data
			// disk keeps its own headroom (the merge stall
			// was a READER's disk).
			Expect(names).To(ContainElements("disk-autoscale.service", "disk-autoscale.timer"))
			Expect(names).NotTo(ContainElement("dead-replica-gc.service"))
			Expect(names).NotTo(ContainElement("orphaned-data-gc.service"))
			// Readers serve replicated parts, they never ingest —
			// their replication queues don't drive merges.
			Expect(names).NotTo(ContainElement("queue-heal.service"))
		})

		It("runs disk-autoscale every five minutes", func() {
			spec := baseSpec()
			spec.Role = model.RoleReader
			spec.WriterMax = 0
			spec.ReaderMax = 10
			b := &Boot{Spec: &spec}
			for _, u := range b.chinitUnits() {
				if u.Name == "disk-autoscale.timer" {
					Expect(u.Content).To(ContainSubstring("OnCalendar=*-*-* *:00/5:00"))
				}
			}
		})

		It("describes lease-renew as node-scoped, not reader-scoped", func() {
			// The writer-ASG rollout made the lease a both-roles
			// mechanism; the description must not claim reader-only
			// (the log would tell the operator the wrong story when a
			// writer's timer fires).
			spec := baseSpec()
			spec.Role = model.RoleReader
			spec.WriterMax = 0
			spec.ReaderMax = 10
			b := &Boot{Spec: &spec}
			for _, u := range b.chinitUnits() {
				if u.Name == "lease-renew.service" {
					Expect(u.Content).To(ContainSubstring("Description=Node ordinal lease renewal"))
				}
				if u.Name == "lease-renew.timer" {
					Expect(u.Content).To(ContainSubstring("Renew the ordinal lease and converge peers every minute"))
					Expect(u.Content).To(ContainSubstring("OnCalendar=*-*-* *:*:00"))
					Expect(u.Content).NotTo(ContainSubstring("reader ordinal lease"))
				}
			}
		})
	})

	Context("keeper", func() {
		It("installs the keeper-health probe AND its every-minute timer", func() {
			spec := baseSpec()
			spec.Role = model.RoleKeeper
			spec.IdentityMode = model.IdentityStatic
			b := &Boot{Spec: &spec}

			// Self-healing contract (the blind window): the
			// probe without a timer never ran again after boot, and
			// keeper_down sat INSUFFICIENT_DATA. The timer is the fix.
			// The autoscale pair rides the keeper too: keeper-0's
			// vertical coordinator (the Lambda's replacement) — the
			// subcommand itself gates on the ordinal-0 hostname.
			// The spec-sync pair is the keeper's config-drift
			// convergence (its role spec changes render its single-
			// file config — see the units matrix comment).
			names := unitNames(b.chinitUnits())
			Expect(names).To(Equal([]string{
				"keeper-health.service", "keeper-health.timer",
				"spec-sync.service", "spec-sync.timer",
				"autoscale.service", "autoscale.timer",
			}))

			for _, u := range b.chinitUnits() {
				if u.Name == "keeper-health.timer" {
					Expect(u.Content).To(ContainSubstring("OnCalendar=*-*-* *:*:00"))
					Expect(u.Content).To(ContainSubstring("keeper_up"))
				}
				if u.Name == "autoscale.timer" {
					Expect(u.Content).To(ContainSubstring("OnCalendar=*-*-* *:0/15:00"))
				}
			}
		})
	})

	Context("backup replica", func() {
		It("installs the schema-bootstrap, spec-sync, queue-heal, and static peer-convergence pairs", func() {
			spec := baseSpec()
			spec.Role = model.RoleBackup
			spec.IdentityMode = model.IdentityStatic
			b := &Boot{Spec: &spec}

			// The backup replica materializes the same replicated
			// tables, so it carries the DDL convergence unit; the
			// queue-heal pair heals its OWN read-only/stuck tables
			// (the 2026-11-17 finding: 3 summary tables sat readonly
			// with no unit on the role to issue the heal); the
			// lease-renew pair runs the STATIC peer convergence
			// (convergeStaticNodePeers heals /etc/hosts + DNS for a
			// lease-less role — the same finding: zero hosts entries
			// and DNS_ERROR part fetches for hours).
			Expect(unitNames(b.chinitUnits())).To(ConsistOf(
				"schema-bootstrap.service", "schema-bootstrap.timer",
				"spec-sync.service", "spec-sync.timer",
				"queue-heal.service", "queue-heal.timer",
				"lease-renew.service", "lease-renew.timer",
			))
			for _, u := range b.chinitUnits() {
				if u.Name == "queue-heal.timer" {
					Expect(u.Content).To(ContainSubstring("*-*-* *:00/5:00"))
				}
				if u.Name == "lease-renew.timer" {
					Expect(u.Content).To(ContainSubstring("*-*-* *:*:00"))
				}
			}
		})
	})
})

var _ = Describe("shutdownDrainUnit", func() {
	// The in-node shutdown drain unit (the fix for the
	// soak reader scale-ins left stale DNS + lease residue
	// because the SSM-dispatched drain races the agent's teardown, and
	// a health-check termination never fires the hook at all). These
	// specs pin the unit-content contract; the role gating lives in
	// installShutdownDrain's switch (writers + readers only).

	It("orders ExecStop after clickhouse-server so the drain runs against a live server", func() {
		// The ordering IS the design: on shutdown systemd stops units
		// in reverse dependency order, so After=clickhouse-server
		// means this unit's ExecStop runs BEFORE the server stops
		// the peer drop and SYSTEM STOP queries need a live server.
		u := (&Boot{Spec: &model.BootSpec{}}).shutdownDrainUnit()
		Expect(u.Name).To(Equal("chinit-shutdown-drain.service"))
		Expect(u.Content).To(ContainSubstring("After=network-online.target clickhouse-server.service"))
	})

	It("runs the drain-shutdown subcommand at ExecStop and stays quiescent at start", func() {
		// ExecStart=/bin/true + RemainAfterExit=yes: the unit exists
		// solely to be an active unit whose ExecStop systemd invokes
		// during the shutdown transaction.
		u := (&Boot{Spec: &model.BootSpec{}}).shutdownDrainUnit()
		Expect(u.Content).To(ContainSubstring("ExecStart=/bin/true"))
		Expect(u.Content).To(ContainSubstring("ExecStop=/usr/local/bin/chinit drain-shutdown"))
		Expect(u.Content).To(ContainSubstring("RemainAfterExit=yes"))
	})

	It("bounds the drain under EC2's shutdown window", func() {
		// The subcommand's own budget is 150s; the unit bound (170s)
		// must sit ABOVE it so the wrapper never cuts the drain short,
		// and both must stay under EC2's 180s API-driven window — a
		// drain that overruns the window gets the instance killed
		// mid-drain, the exact outcome the unit prevents.
		u := (&Boot{Spec: &model.BootSpec{}}).shutdownDrainUnit()
		Expect(u.Content).To(ContainSubstring("TimeoutStopSec=170"))
	})
})

var _ = Describe("spotWatchEligible", func() {
	// The 2026-11-19 writer-5/writer-7 finding: dev runs writer-1..N
	// 100% Spot (writer_spot_ratio = 1.0 above the on-demand base), and
	// the retired Spot writers left ghost replica registrations that
	// wedged the fleet's replication queues — the spot-watch daemon
	// (the notice-driven full drain) was reader-only. The gate now
	// covers every role whose departure must release a registration +
	// lease + DNS inside the 2-minute ITN window.

	It("includes writers and readers — the ASG roles with a registration to release", func() {
		Expect(spotWatchEligible(model.RoleWriter)).To(BeTrue())
		Expect(spotWatchEligible(model.RoleReader)).To(BeTrue())
	})

	It("excludes keepers and the backup replica", func() {
		// Keepers hold no lease and run no server; backup-0 is static
		// (never spot) and its identity must survive a notice.
		Expect(spotWatchEligible(model.RoleKeeper)).To(BeFalse())
		Expect(spotWatchEligible(model.RoleBackup)).To(BeFalse())
		Expect(spotWatchEligible(model.Role(""))).To(BeFalse())
	})
})
