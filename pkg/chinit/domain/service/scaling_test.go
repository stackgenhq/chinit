// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// The autoscale service suite pins the coordinator contract over
// pure fakes (the DDD boundary: no cloud, no clock — the service is
// the decision, the fakes are the world).
//
// The contract under test:
//   - ONE step per invocation, bounded by the ladder (min/max), paced
//     by the cooldown store — never a free-for-all.
//   - Signals are judged by the domain thresholds; a breach with no
//     ladder step is a logged no-op, never a crash.
//   - The keeper coordinator resizes ITSELF (the static-identity
//     single-step path); the writer coordinator rolls the fleet.

// fakeSignals drives the service's decision input.
type fakeSignals struct {
	keeper    domain.ScaleSignals
	writer    domain.ScaleSignals
	keeperErr error
	writerErr error
}

func (f *fakeSignals) ReadKeeperSignals(ctx context.Context, cluster string) (domain.ScaleSignals, error) {
	return f.keeper, f.keeperErr
}

func (f *fakeSignals) ReadWriterSignals(ctx context.Context, cluster string) (domain.ScaleSignals, error) {
	return f.writer, f.writerErr
}

// fakeResizer records the resize steps the coordinator took.
type fakeResizer struct {
	describe  *port.InstanceState
	descErr   error
	changes   []port.InstanceTypeChange
	starts    []string
	changeErr error
}

func (f *fakeResizer) DescribeInstance(ctx context.Context, id string) (*port.InstanceState, error) {
	return f.describe, f.descErr
}

func (f *fakeResizer) ChangeInstanceType(ctx context.Context, req port.InstanceTypeChange) error {
	if f.changeErr != nil {
		return f.changeErr
	}
	f.changes = append(f.changes, req)
	return nil
}

func (f *fakeResizer) StartInstance(ctx context.Context, id string) error {
	f.starts = append(f.starts, id)
	return nil
}

// fakeFleet records the launch-template rolls.
type fakeFleet struct {
	versions map[string]int64
	created  []string
	rolls    []port.FleetRefreshRequest
	binding  *port.FleetLaunchTemplate
}

func (f *fakeFleet) DescribeFleetLaunchTemplate(ctx context.Context, asgName string) (*port.FleetLaunchTemplate, error) {
	if f.binding == nil {
		f.binding = &port.FleetLaunchTemplate{LaunchTemplateID: "lt-live", Version: "$Latest"}
	}
	return f.binding, nil
}

func (f *fakeFleet) CreateLaunchTemplateVersion(ctx context.Context, id, typ string) (int64, error) {
	f.created = append(f.created, typ)
	f.versions[id] = f.versions[id] + 1
	return f.versions[id], nil
}

func (f *fakeFleet) StartFleetRefresh(ctx context.Context, req port.FleetRefreshRequest) error {
	f.rolls = append(f.rolls, req)
	return nil
}

// fakeCooldown paces the steps.
type fakeCooldown struct {
	records map[string]int64
}

func (f *fakeCooldown) PeekCooldown(ctx context.Context, req port.CooldownRequest) (int64, bool) {
	exp, ok := f.records[req.Key]
	return exp, ok
}

func (f *fakeCooldown) MarkCooldown(ctx context.Context, req port.CooldownRequest) error {
	f.records[req.Key] = time.Now().Add(time.Duration(req.Minutes) * time.Minute).Unix()
	return nil
}

func newScalingFakes() (*fakeSignals, *fakeResizer, *fakeFleet, *fakeCooldown) {
	return &fakeSignals{},
		&fakeResizer{describe: &port.InstanceState{InstanceType: "t4g.large", State: "running"}},
		&fakeFleet{versions: map[string]int64{}},
		&fakeCooldown{records: map[string]int64{}}
}

func keeperLadderFixture() domain.TypeLadder {
	return domain.TypeLadder{
		Types: []string{"t4g.small", "t4g.medium", "t4g.large", "t4g.xlarge"},
		Min:   "t4g.medium",
		Max:   "t4g.xlarge",
	}
}

var _ = Describe("AutoscaleService", func() {
	var signals *fakeSignals
	var resizer *fakeResizer
	var fleet *fakeFleet
	var cool *fakeCooldown

	BeforeEach(func() {
		signals, resizer, fleet, cool = newScalingFakes()
	})

	keeperRequest := func() AutoscaleRequest {
		return AutoscaleRequest{
			ClusterName:      "demo",
			Coordinator:      domain.CoordinatorKeeper,
			Cooldown:         port.CooldownRequest{Table: "claims", Key: string(domain.CoordinatorKeeper)},
			KeeperInstanceID: "i-keeper0",
			Ladder:           keeperLadderFixture(),
		}
	}

	writerRequest := func() AutoscaleRequest {
		return AutoscaleRequest{
			ClusterName:      "demo",
			Coordinator:      domain.CoordinatorWriter,
			Cooldown:         port.CooldownRequest{Table: "claims", Key: string(domain.CoordinatorWriter)},
			KeeperInstanceID: "i-writer0",
			Ladder:           keeperLadderFixture(),
			Fleet: port.FleetRefreshRequest{
				AutoScalingGroupName: "writers",
			},
		}
	}

	Context("the keeper coordinator", func() {
		It("self-resizes one ladder step on a latency breach", func() {
			signals.keeper = domain.ScaleSignals{KeeperMaxLatencyMs: 6000, KeeperOutstanding: 0, KeeperCPUIdlePct: 80}
			svc, err := NewAutoscaleService(signals, resizer, fleet, cool)
			Expect(err).NotTo(HaveOccurred())

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Skipped).To(BeEmpty())
			Expect(res.Decision.Action).To(Equal("keeper-self"))
			Expect(res.Decision.From).To(Equal("t4g.large"))
			Expect(res.Decision.To).To(Equal("t4g.xlarge"))
			Expect(resizer.changes).To(HaveLen(1), "exactly ONE step per invocation")
			Expect(cool.records).To(HaveKey("autoscale-keeper"), "the step paces the next one")
		})

		It("skips when the cooldown is active", func() {
			signals.keeper = domain.ScaleSignals{KeeperMaxLatencyMs: 6000}
			cool.records["autoscale-keeper"] = time.Now().Add(time.Hour).Unix()
			svc, err := NewAutoscaleService(signals, resizer, fleet, cool)
			Expect(err).NotTo(HaveOccurred())

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Skipped).To(ContainSubstring("cooldown active"))
			Expect(resizer.changes).To(BeEmpty(), "a paced coordinator never steps")
		})

		It("no-ops (decision, not error) when signals sit within thresholds", func() {
			signals.keeper = domain.ScaleSignals{KeeperMaxLatencyMs: 12, KeeperOutstanding: 1, KeeperCPUIdlePct: 80}
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Decision.Breach).To(BeFalse())
			Expect(res.Decision.Action).To(BeEmpty())
			Expect(resizer.changes).To(BeEmpty())
		})

		It("self-resizes one ladder step DOWN on sustained idle above floor", func() {
			signals.keeper = domain.ScaleSignals{KeeperMaxLatencyMs: 10, KeeperOutstanding: 0, KeeperCPUIdlePct: 95}
			resizer.describe.InstanceType = "t4g.large"
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Decision.Breach).To(BeFalse())
			Expect(res.Decision.Action).To(Equal("keeper-self-down"))
			Expect(res.Decision.From).To(Equal("t4g.large"))
			Expect(res.Decision.To).To(Equal("t4g.medium"))
			Expect(resizer.changes).To(HaveLen(1))
			Expect(cool.records).To(HaveKey("autoscale-keeper"))
		})

		It("no-ops at the ladder floor on sustained idle", func() {
			signals.keeper = domain.ScaleSignals{KeeperMaxLatencyMs: 10, KeeperOutstanding: 0, KeeperCPUIdlePct: 95}
			resizer.describe.InstanceType = "t4g.medium"
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Decision.Action).To(BeEmpty())
			Expect(res.Decision.Reason).To(ContainSubstring("at ladder floor"))
			Expect(resizer.changes).To(BeEmpty())
		})

		It("no-ops at the ladder ceiling (the runaway bound)", func() {
			signals.keeper = domain.ScaleSignals{KeeperMaxLatencyMs: 5000}
			resizer.describe.InstanceType = "t4g.xlarge"
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Decision.Breach).To(BeTrue())
			Expect(res.Decision.Action).To(BeEmpty(), "ceiling: breach reported, step refused")
			Expect(resizer.changes).To(BeEmpty())
		})

		It("skips (not errors) when the signals read fails", func() {
			signals.keeperErr = errors.New("cloudwatch down")
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), keeperRequest())
			Expect(err).NotTo(HaveOccurred(), "an unreadable tick is a skip, never a crash loop")
			Expect(res.Skipped).To(ContainSubstring("signals unreadable"))
		})

		It("refuses construction without the mandatory ports", func() {
			_, err := NewAutoscaleService(nil, resizer, fleet, cool)
			Expect(err).To(MatchError(ContainSubstring("signals reader is nil")))
			_, err = NewAutoscaleService(signals, resizer, fleet, nil)
			Expect(err).To(MatchError(ContainSubstring("cooldown store is nil")))
		})
	})

	Context("the writer coordinator", func() {
		It("steps the launch template and rolls the fleet on a CPU breach", func() {
			signals.writer = domain.ScaleSignals{WriterCPUIdlePct: 10}
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), writerRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Skipped).To(BeEmpty())
			Expect(res.Decision.Action).To(Equal("writer-roll"))
			Expect(res.Decision.To).To(Equal("t4g.xlarge"))
			Expect(fleet.created).To(Equal([]string{"t4g.xlarge"}), "one numbered LT version")
			Expect(fleet.rolls).To(HaveLen(1))
			Expect(fleet.rolls[0].MinHealthyPercentage).To(Equal(int32(100)), "the ingest quorum holds through the roll")
			Expect(fleet.created).To(Equal([]string{"t4g.xlarge"}), "one numbered LT version")
			Expect(cool.records).To(HaveKey("autoscale-writer"))
		})

		It("no-ops when the fleet idle sits within the normal operating band", func() {
			signals.writer = domain.ScaleSignals{WriterCPUIdlePct: 60}
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), writerRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Decision.Action).To(BeEmpty())
			Expect(fleet.rolls).To(BeEmpty())
		})

		It("steps the launch template DOWN and rolls the fleet on sustained idle above floor", func() {
			signals.writer = domain.ScaleSignals{WriterCPUIdlePct: 90}
			resizer.describe.InstanceType = "t4g.xlarge"
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), writerRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Skipped).To(BeEmpty())
			Expect(res.Decision.Action).To(Equal("writer-roll-down"))
			Expect(res.Decision.From).To(Equal("t4g.xlarge"))
			Expect(res.Decision.To).To(Equal("t4g.large"))
			Expect(fleet.created).To(Equal([]string{"t4g.large"}))
			Expect(fleet.rolls).To(HaveLen(1))
			Expect(fleet.rolls[0].MinHealthyPercentage).To(Equal(int32(100)))
			Expect(cool.records).To(HaveKey("autoscale-writer"))
		})

		It("no-ops when sustained idle hits the ladder floor", func() {
			signals.writer = domain.ScaleSignals{WriterCPUIdlePct: 92}
			resizer.describe.InstanceType = "t4g.medium"
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)

			res, err := svc.Autoscale(context.Background(), writerRequest())
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Decision.Action).To(BeEmpty())
			Expect(res.Decision.Reason).To(ContainSubstring("at ladder floor"))
			Expect(fleet.rolls).To(BeEmpty())
		})

		It("skips when the fleet identity is unconfigured", func() {
			signals.writer = domain.ScaleSignals{WriterCPUIdlePct: 5}
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)
			req := writerRequest()
			req.Fleet = port.FleetRefreshRequest{}

			res, err := svc.Autoscale(context.Background(), req)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Skipped).To(ContainSubstring("fleet identity is unconfigured"))
		})
	})

	Context("CoordinatorNone is a caller bug", func() {
		It("refuses to run", func() {
			svc, _ := NewAutoscaleService(signals, resizer, fleet, cool)
			req := keeperRequest()
			req.Coordinator = domain.CoordinatorNone
			_, err := svc.Autoscale(context.Background(), req)
			Expect(err).To(MatchError(ContainSubstring("CoordinatorNone")), "the gate belongs to the caller; the service never guesses")
		})
	})
})

var _ = Describe("TypeLadder", func() {
	ladder := domain.TypeLadder{
		Types: []string{"t4g.small", "t4g.medium", "t4g.large", "t4g.xlarge"},
		Min:   "t4g.medium",
		Max:   "t4g.xlarge",
	}

	DescribeTable("the bounded step",
		func(current, wantNext string, wantOK bool) {
			next, ok := ladder.Next(current)
			Expect(next).To(Equal(wantNext))
			Expect(ok).To(Equal(wantOK))
		},
		Entry("steps medium → large", "t4g.medium", "t4g.large", true),
		Entry("steps large → xlarge", "t4g.large", "t4g.xlarge", true),
		Entry("ceiling: xlarge has no step", "t4g.xlarge", "", false),
		Entry("floor: the min type steps normally", "t4g.medium", "t4g.large", true),
		Entry("below floor steps INTO the ladder (min gates the DECISION floor, not the first step)", "t4g.small", "t4g.medium", true),
		Entry("an unknown type never steps (no guessing)", "m8g.2xlarge", "", false),
	)

	It("answers the ceiling and floor checks", func() {
		Expect(ladder.AtOrAboveMax("t4g.xlarge")).To(BeTrue())
		Expect(ladder.AtOrAboveMax("t4g.large")).To(BeFalse())
		Expect(ladder.AtOrBelowMin("t4g.medium")).To(BeTrue())
		Expect(ladder.AtOrBelowMin("t4g.small")).To(BeTrue())
		Expect(ladder.AtOrBelowMin("t4g.large")).To(BeFalse())
	})

	DescribeTable("the bounded step down (Prev)",
		func(current, wantPrev string, wantOK bool) {
			prev, ok := ladder.Prev(current)
			Expect(prev).To(Equal(wantPrev))
			Expect(ok).To(Equal(wantOK))
		},
		Entry("steps xlarge → large", "t4g.xlarge", "t4g.large", true),
		Entry("steps large → medium", "t4g.large", "t4g.medium", true),
		Entry("floor: medium refuses to step below Min", "t4g.medium", "", false),
		Entry("below floor refuses to step further down", "t4g.small", "", false),
		Entry("an unknown type never steps", "r8g.2xlarge", "", false),
	)

	It("treats an unset bound as no ceiling", func() {
		open := domain.TypeLadder{Types: []string{"a", "b", "c"}}
		_, ok := open.Next("c")
		Expect(ok).To(BeFalse(), "the ladder's end is always the ceiling")
		_, ok = open.Next("a")
		Expect(ok).To(BeTrue())
		_, ok = open.Prev("a")
		Expect(ok).To(BeFalse(), "the ladder's start is always the floor")
		prev, ok := open.Prev("c")
		Expect(ok).To(BeTrue())
		Expect(prev).To(Equal("b"))
	})
})

var _ = Describe("ScaleThresholds", func() {
	t := domain.DefaultScaleThresholds()

	DescribeTable("keeper breach",
		func(s domain.ScaleSignals, want bool) {
			Expect(t.KeeperBreach(s)).To(Equal(want))
		},
		Entry("latency breach (5000ms cumulative max)", domain.ScaleSignals{KeeperMaxLatencyMs: 5000}, true),
		Entry("latency below (all healthy, including snapshot churn <5s)", domain.ScaleSignals{KeeperMaxLatencyMs: 4999, KeeperOutstanding: 1, KeeperCPUIdlePct: 90}, false),
		Entry("outstanding breach", domain.ScaleSignals{KeeperOutstanding: 10}, true),
		Entry("outstanding below (all healthy)", domain.ScaleSignals{KeeperMaxLatencyMs: 12, KeeperOutstanding: 9, KeeperCPUIdlePct: 90}, false),
		Entry("cpu idle breach (low idle = busy)", domain.ScaleSignals{KeeperCPUIdlePct: 20}, true),
		Entry("cpu idle healthy", domain.ScaleSignals{KeeperCPUIdlePct: 21}, false),
		Entry("no signal breached", domain.ScaleSignals{KeeperMaxLatencyMs: 1, KeeperOutstanding: 1, KeeperCPUIdlePct: 99}, false),
	)

	DescribeTable("writer breach",
		func(idle float64, want bool) {
			Expect(t.WriterBreach(domain.ScaleSignals{WriterCPUIdlePct: idle})).To(Equal(want))
		},
		Entry("idle at threshold", 35.0, true),
		Entry("idle above", 36.0, false),
		Entry("idle below (saturated)", 5.0, true),
	)

	DescribeTable("writer idle (scale-down)",
		func(idle float64, want bool) {
			Expect(t.WriterIdle(domain.ScaleSignals{WriterCPUIdlePct: idle})).To(Equal(want))
		},
		Entry("idle at scale-down threshold (85%)", 85.0, true),
		Entry("idle above (92%)", 92.0, true),
		Entry("idle in normal band (60%)", 60.0, false),
	)
})

var _ = fmt.Stringer(nil) // keep fmt imported for the fixtures' error shapes
