// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

// collect drains the manual reader into a name → metric lookup.
func collect(r *metric.ManualReader) map[string]metricdata.Metrics {
	var rm metricdata.ResourceMetrics
	Expect(r.Collect(context.Background(), &rm)).To(Succeed())
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

// attrValue reads one attribute (the empty string when absent — the
// specs only ask for keys they emitted).
func attrValue(attrs attribute.Set, key string) string {
	v, _ := attrs.Value(attribute.Key(key))
	return v.AsString()
}

var _ = Describe("Telemetry", func() {
	var reader *metric.ManualReader

	BeforeEach(func() {
		Expect(telemetry.Shutdown(context.Background())).To(Succeed())
		reader = metric.NewManualReader()
		Expect(telemetry.InitWithReader(telemetry.Config{
			ServiceVersion: "test",
			Cluster:        "sg-prod",
			Role:           "writer",
			Cloud:          "aws",
			Region:         "us-east-1",
		}, reader)).To(Succeed())
	})

	AfterEach(func() {
		Expect(telemetry.Shutdown(context.Background())).To(Succeed())
	})

	Context("disabled state", func() {
		It("Init with an empty endpoint leaves emits as no-ops", func() {
			Expect(telemetry.Shutdown(context.Background())).To(Succeed())
			Expect(telemetry.Init(context.Background(), telemetry.Config{})).To(Succeed())
			Expect(telemetry.Enabled()).To(BeFalse())

			// Every emit must be safe on the disabled singleton: no
			// panic, no error, nothing recorded.
			telemetry.JobRun(context.Background(), telemetry.JobRunRequest{Job: "schema-sync", Outcome: telemetry.OutcomeCompleted})
			telemetry.BootStep(context.Background(), telemetry.BootStepRequest{Step: "render_node_config"})
			telemetry.FleetSweep(context.Background(), telemetry.FleetSweepRequest{Rows: 3})
			telemetry.FleetPoll(context.Background(), telemetry.FleetPollRequest{Claimed: 1})
			telemetry.KeeperUp(context.Background(), telemetry.KeeperUpRequest{Up: 1})
			telemetry.DiskPressure(context.Background(), telemetry.DiskPressureRequest{FreeGB: 12.5})
			Expect(telemetry.Shutdown(context.Background())).To(Succeed())
		})

		It("Init with a nil reader is a no-op (the InitWithReader disabled seam)", func() {
			Expect(telemetry.Shutdown(context.Background())).To(Succeed())
			Expect(telemetry.InitWithReader(telemetry.Config{}, nil)).To(Succeed())
			Expect(telemetry.Enabled()).To(BeFalse())
		})
	})

	Context("job outcomes", func() {
		It("records the run count and duration with job and outcome attrs", func() {
			telemetry.JobRun(context.Background(), telemetry.JobRunRequest{
				Job:             "schema-sync",
				Outcome:         telemetry.OutcomeCompleted,
				DurationSeconds: 2.5,
			})
			telemetry.JobRun(context.Background(), telemetry.JobRunRequest{
				Job:             "keeper-health",
				Outcome:         telemetry.OutcomeFailed,
				DurationSeconds: 1.0,
			})
			// Collect BEFORE Shutdown: the manual reader dies with the
			// provider (Shutdown's teardown closes it — the production
			// flush happens inside Shutdown; the spec reads the
			// accumulated datapoints while the reader is live).
			metrics := collect(reader)

			Expect(metrics).To(HaveKey("chinit_job_runs"))
			Expect(metrics).To(HaveKey("chinit_job_duration_seconds"))

			sum := metrics["chinit_job_runs"].Data.(metricdata.Sum[int64])
			Expect(sum.DataPoints).To(HaveLen(2))
			byOutcome := map[string]int64{}
			for _, dp := range sum.DataPoints {
				Expect(attrValue(dp.Attributes, "chinit.cluster")).To(Equal("sg-prod"))
				Expect(attrValue(dp.Attributes, "chinit.role")).To(Equal("writer"))
				byOutcome[attrValue(dp.Attributes, "chinit.job")+":"+attrValue(dp.Attributes, "chinit.outcome")] = dp.Value
			}
			Expect(byOutcome).To(HaveKeyWithValue("schema-sync:"+telemetry.OutcomeCompleted, int64(1)))
			Expect(byOutcome).To(HaveKeyWithValue("keeper-health:"+telemetry.OutcomeFailed, int64(1)))
		})
	})

	Context("boot steps", func() {
		It("records step durations with outcome attrs, skipped mapped to skipped", func() {
			telemetry.BootStep(context.Background(), telemetry.BootStepRequest{
				Step: "mount_data_disks", DurationSeconds: 4.2,
			})
			telemetry.BootStep(context.Background(), telemetry.BootStepRequest{
				Step: "configure_keeper", DurationSeconds: 1.1, Skipped: true,
			})
			telemetry.BootOutcome(context.Background(), telemetry.BootOutcomeRequest{Refresh: true})
			metrics := collect(reader)
			Expect(metrics).To(HaveKey("chinit_boot_step_duration_seconds"))
			hist := metrics["chinit_boot_step_duration_seconds"].Data.(metricdata.Histogram[float64])
			Expect(hist.DataPoints).To(HaveLen(2))
			outcomes := map[string]bool{}
			for _, dp := range hist.DataPoints {
				outcomes[attrValue(dp.Attributes, "chinit.step")+":"+attrValue(dp.Attributes, "chinit.outcome")] = true
			}
			Expect(outcomes).To(HaveKey("mount_data_disks:" + telemetry.OutcomeCompleted))
			Expect(outcomes).To(HaveKey("configure_keeper:" + telemetry.OutcomeSkipped))

			Expect(metrics).To(HaveKey("chinit_boot_outcomes"))
			sum := metrics["chinit_boot_outcomes"].Data.(metricdata.Sum[int64])
			Expect(sum.DataPoints).To(HaveLen(1))
			Expect(sum.DataPoints[0].Attributes.HasValue("chinit.refresh")).To(BeTrue())
			Expect(attrValue(sum.DataPoints[0].Attributes, "chinit.outcome")).To(Equal(telemetry.OutcomeCompleted))
		})
	})

	Context("fleet sweep and poll", func() {
		It("records the sweep inventory gauges and transition counters", func() {
			telemetry.FleetSweep(context.Background(), telemetry.FleetSweepRequest{
				Rows: 5, Redispatched: 1, Failed: 2, LeftPending: 1, LiveExecuting: 1,
			})
			metrics := collect(reader)
			Expect(metrics).To(HaveKey("chinit_fleet_sweep_rows"))
			gauge := metrics["chinit_fleet_sweep_rows"].Data.(metricdata.Gauge[int64])
			byPhase := map[string]int64{}
			for _, dp := range gauge.DataPoints {
				byPhase[attrValue(dp.Attributes, "chinit.fleet.phase")] = dp.Value
			}
			Expect(byPhase).To(HaveKeyWithValue("total", int64(5)))
			Expect(byPhase).To(HaveKeyWithValue("pending", int64(1)))
			Expect(byPhase).To(HaveKeyWithValue("live_executing", int64(1)))

			Expect(metrics).To(HaveKey("chinit_fleet_sweep_redispatched"))
			sum := metrics["chinit_fleet_sweep_redispatched"].Data.(metricdata.Sum[int64])
			Expect(sum.DataPoints[0].Value).To(Equal(int64(1)))
			Expect(metrics).To(HaveKey("chinit_fleet_sweep_failed"))
			failedSum := metrics["chinit_fleet_sweep_failed"].Data.(metricdata.Sum[int64])
			Expect(failedSum.DataPoints[0].Value).To(Equal(int64(2)))
		})

		It("records the poll claims and verdicts", func() {
			telemetry.FleetPoll(context.Background(), telemetry.FleetPollRequest{
				Claimed: 2, Completed: 1, Failed: 1,
			})
			metrics := collect(reader)
			Expect(metrics).To(HaveKey("chinit_fleet_task_claimed"))
			Expect(metrics["chinit_fleet_task_claimed"].Data.(metricdata.Sum[int64]).DataPoints[0].Value).To(Equal(int64(2)))
			Expect(metrics).To(HaveKey("chinit_fleet_task_completed"))
			Expect(metrics["chinit_fleet_task_completed"].Data.(metricdata.Sum[int64]).DataPoints[0].Value).To(Equal(int64(1)))
			Expect(metrics).To(HaveKey("chinit_fleet_task_failed"))
			Expect(metrics["chinit_fleet_task_failed"].Data.(metricdata.Sum[int64]).DataPoints[0].Value).To(Equal(int64(1)))
		})
	})

	Context("self-heal gauges", func() {
		It("records keeper_up and the disk probe", func() {
			telemetry.KeeperUp(context.Background(), telemetry.KeeperUpRequest{Up: 1.0})
			telemetry.DiskPressure(context.Background(), telemetry.DiskPressureRequest{
				FreeGB: 42.5, Grew: true, Host: "writer-0",
			})
			metrics := collect(reader)
			Expect(metrics).To(HaveKey("chinit_keeper_up"))
			keeper := metrics["chinit_keeper_up"].Data.(metricdata.Gauge[float64])
			Expect(keeper.DataPoints[0].Value).To(Equal(1.0))

			Expect(metrics).To(HaveKey("chinit_disk_effective_free_gb"))
			disk := metrics["chinit_disk_effective_free_gb"].Data.(metricdata.Gauge[float64])
			Expect(disk.DataPoints[0].Value).To(Equal(42.5))
			Expect(attrValue(disk.DataPoints[0].Attributes, "chinit.host")).To(Equal("writer-0"))

			Expect(metrics).To(HaveKey("chinit_disk_autoscaled"))
			grew := metrics["chinit_disk_autoscaled"].Data.(metricdata.Sum[int64])
			Expect(grew.DataPoints[0].Value).To(Equal(int64(1)))
		})
	})

	Context("lifecycle", func() {
		It("Shutdown tears down so a second Init can install a fresh provider", func() {
			Expect(telemetry.Enabled()).To(BeTrue())
			Expect(telemetry.Shutdown(context.Background())).To(Succeed())
			Expect(telemetry.Enabled()).To(BeFalse())

			// Re-init over a NEW reader: the singleton must accept it (the
			// process re-entry path — each CLI run inits once, the next
			// process starts clean).
			r2 := metric.NewManualReader()
			Expect(telemetry.InitWithReader(telemetry.Config{Cluster: "next"}, r2)).To(Succeed())
			Expect(telemetry.Enabled()).To(BeTrue())
			telemetry.JobRun(context.Background(), telemetry.JobRunRequest{Job: "boot", Outcome: telemetry.OutcomeCompleted})

			// Collect while the second reader is live, then flush.
			metrics := collect(r2)
			Expect(metrics).To(HaveKey("chinit_job_runs"))
		})
	})
})
