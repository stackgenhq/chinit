// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/fakes"
)

// The fleet task layer's contract specs: the fault-tolerance scenarios
// (dead-holder takeover, CAS conflicts, retry exhaustion, epoch
// fencing) run against the in-memory reference store — the same
// scenarios the cloud adapters' own suites run against their fakes.
var _ = Describe("FleetTaskService", func() {
	var (
		store *fakes.FleetTaskStore
		svc   *service.FleetTaskService
		ctx   context.Context
	)
	const table = "claims"

	BeforeEach(func() {
		store = fakes.NewFleetTaskStore()
		var err error
		svc, err = service.NewFleetTaskService(store)
		Expect(err).NotTo(HaveOccurred())
		ctx = context.Background()
	})

	Describe("SubmitTask", func() {
		It("creates a pending row with a fresh epoch", func() {
			ok, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(1))
			Expect(rows[0].Phase).To(Equal(port.TaskPending))
			Expect(rows[0].Epoch).To(Equal(int64(1)))
			Expect(rows[0].MaxRetries).To(Equal(port.FleetTaskDefaultMaxRetries), "the default budget applies when the submit leaves it zero")
		})

		It("refuses a live duplicate (already queued)", func() {
			submit := port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			}
			_, err := store.SubmitTask(ctx, submit)
			Expect(err).NotTo(HaveOccurred())
			ok, err := store.SubmitTask(ctx, submit)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse(), "a live duplicate is the benign refusal, never an error")
		})

		It("replaces a terminal row with a fresh epoch", func() {
			submit := port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			}
			_, err := store.SubmitTask(ctx, submit)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClaimTask(ctx, port.FleetTaskClaim{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.CompleteTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			ok, err := store.SubmitTask(ctx, submit)
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue(), "a resubmit over a done row is a fresh epoch")
		})
	})

	Describe("ClaimTask and the epoch fence", func() {
		It("refuses a foreign executor's claim", func() {
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			ok, err := store.ClaimTask(ctx, port.FleetTaskClaim{
				Table: table, TaskID: "refresh-writer-0", Executor: "writer-1", Epoch: 1,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse(), "a task row belongs to its target; a foreign claim is refused")
		})

		It("fences the stale executor's verdict after a re-dispatch", func() {
			// The core fail-safe: a motion cut off mid-flight (its
			// heartbeat went stale, the sweep re-dispatched with
			// epoch+1) must never complete under its old epoch.
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClaimTask(ctx, port.FleetTaskClaim{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			// The sweep re-dispatches: epoch moves to 2.
			ok, err := store.RetryTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())
			// The stale executor's terminal write is refused.
			ok, err = store.CompleteTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse(), "the stale epoch is fenced off")
			// Its heartbeat is refused too (the executor aborts).
			ok, err = store.HeartbeatTask(ctx, port.FleetTaskHeartbeat{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})
	})

	Describe("RetryTask budget", func() {
		It("refuses re-dispatch at the exhausted budget", func() {
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0", MaxRetries: 1,
			})
			Expect(err).NotTo(HaveOccurred())
			ok, err := store.RetryTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue(), "the first re-dispatch is within the budget")
			ok, err = store.RetryTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Epoch: 2})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse(), "the budget is spent; the sweep never invents retries")
		})

		It("refuses a re-dispatch of a done row", func() {
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClaimTask(ctx, port.FleetTaskClaim{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.CompleteTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			ok, err := store.RetryTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Epoch: 1})
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse(), "a done row is terminal")
		})
	})

	Describe("Sweep (the coordinator tick)", func() {
		It("re-dispatches a stale executing row within its budget", func() {
			// Dead-holder takeover: the executor died mid-motion (its
			// heartbeat is long past), the sweep re-dispatches.
			base := time.Now()
			clock := base
			store = fakes.NewFleetTaskStore().WithNow(func() int64 { return clock.Unix() })
			var err error
			svc, err = service.NewFleetTaskService(store)
			Expect(err).NotTo(HaveOccurred())
			svc = svc.WithNow(func() time.Time { return clock })
			_, err = store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClaimTask(ctx, port.FleetTaskClaim{
				Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1,
				HeartbeatSeconds: 60,
			})
			Expect(err).NotTo(HaveOccurred())
			// The clock advances past the stale deadline (3x cadence).
			clock = base.Add(10 * time.Minute)
			res, err := svc.Sweep(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Redispatched).To(Equal(1), "the dead executor's row is re-dispatched")
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0].Phase).To(Equal(port.TaskPending))
			Expect(rows[0].Epoch).To(Equal(int64(2)), "the re-dispatch moved the epoch — the old executor is fenced")
			Expect(rows[0].Retries).To(Equal(1))
		})

		It("fails a stale executing row at the exhausted budget", func() {
			base := time.Now()
			clock := base
			store = fakes.NewFleetTaskStore().WithNow(func() int64 { return clock.Unix() })
			var err error
			svc, err = service.NewFleetTaskService(store)
			Expect(err).NotTo(HaveOccurred())
			svc = svc.WithNow(func() time.Time { return clock })
			_, err = store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0", MaxRetries: -1,
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClaimTask(ctx, port.FleetTaskClaim{
				Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1, HeartbeatSeconds: 60,
			})
			Expect(err).NotTo(HaveOccurred())
			clock = base.Add(10 * time.Minute)
			res, err := svc.Sweep(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Failed).To(Equal(1), "the budget is spent; terminal failed is the sweep's answer")
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0].Phase).To(Equal(port.TaskFailed))
		})

		It("never touches a live executing row", func() {
			now := time.Now()
			store = fakes.NewFleetTaskStore().WithNow(func() int64 { return now.Unix() })
			var err error
			svc, err = service.NewFleetTaskService(store)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.ClaimTask(ctx, port.FleetTaskClaim{
				Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1, HeartbeatSeconds: 60,
			})
			Expect(err).NotTo(HaveOccurred())
			// Fresh heartbeat: the sweep must not re-dispatch.
			store.WithNow(func() int64 { return now.Add(1 * time.Minute).Unix() })
			res, err := svc.Sweep(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.LiveExecuting).To(Equal(1))
			Expect(res.Redispatched).To(Equal(0))
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0].Epoch).To(Equal(int64(1)), "a live motion is never fenced")
		})

		It("counts pending rows and leaves them for their target's poll", func() {
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: "writer-0",
			})
			Expect(err).NotTo(HaveOccurred())
			res, err := svc.Sweep(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.LeftPending).To(Equal(1))
			Expect(res.Redispatched).To(Equal(0))
		})
	})

	Describe("NewFleetTaskService", func() {
		It("refuses a nil store", func() {
			_, err := service.NewFleetTaskService(nil)
			Expect(err).To(HaveOccurred(), "a nil store is a wiring bug, not a degradation")
		})
	})
})

// fakeMotionRunner is the runner seam's test double: it records the
// motions it ran and fails on cue (the sequencing specs assert over
// the calls, not the subprocess).
type fakeMotionRunner struct {
	calls   []port.FleetTaskType
	failOn  port.FleetTaskType
	hang    bool
	hangFor time.Duration
}

func (f *fakeMotionRunner) RunMotion(ctx context.Context, task port.FleetTask) error {
	f.calls = append(f.calls, task.Type)
	if f.failOn == task.Type {
		return fmt.Errorf("motion failed on cue")
	}
	if f.hang {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.hangFor):
			return nil
		}
	}
	return nil
}

var _ = Describe("FleetTaskExecutor", func() {
	var (
		store  *fakes.FleetTaskStore
		runner *fakeMotionRunner
		ctx    context.Context
	)
	const table = "claims"
	const self = "writer-0"

	BeforeEach(func() {
		store = fakes.NewFleetTaskStore()
		runner = &fakeMotionRunner{}
		ctx = context.Background()
	})

	Describe("NewFleetTaskExecutor", func() {
		It("refuses nil ports and an empty hostname", func() {
			_, err := service.NewFleetTaskExecutor(nil, runner, self)
			Expect(err).To(HaveOccurred())
			_, err = service.NewFleetTaskExecutor(store, nil, self)
			Expect(err).To(HaveOccurred())
			_, err = service.NewFleetTaskExecutor(store, runner, "")
			Expect(err).To(HaveOccurred(), "an empty hostname would claim another node's rows")
		})
	})

	Describe("Poll", func() {
		It("claims its own pending row and completes it", func() {
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: self,
			})
			Expect(err).NotTo(HaveOccurred())
			exec, err := service.NewFleetTaskExecutor(store, runner, self)
			Expect(err).NotTo(HaveOccurred())
			res, err := exec.Poll(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Claimed).To(Equal(1))
			Expect(res.Completed).To(Equal(1))
			Expect(runner.calls).To(Equal([]port.FleetTaskType{port.TaskRefresh}))
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0].Phase).To(Equal(port.TaskDone))
		})

		It("never touches another node's row", func() {
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-1", Type: port.TaskRefresh, Target: "writer-1",
			})
			Expect(err).NotTo(HaveOccurred())
			exec, err := service.NewFleetTaskExecutor(store, runner, self)
			Expect(err).NotTo(HaveOccurred())
			res, err := exec.Poll(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Claimed).To(Equal(0), "the executor only polls its own rows")
			Expect(runner.calls).To(BeEmpty())
		})

		It("writes the failed verdict when the motion fails", func() {
			runner.failOn = port.TaskRefresh
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: self,
			})
			Expect(err).NotTo(HaveOccurred())
			exec, err := service.NewFleetTaskExecutor(store, runner, self)
			Expect(err).NotTo(HaveOccurred())
			res, err := exec.Poll(ctx, table)
			Expect(err).To(HaveOccurred(), "a failed motion surfaces the error to the timer")
			Expect(res.Claimed).To(Equal(1))
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0].Phase).To(Equal(port.TaskFailed))
		})

		It("never writes a verdict when fenced mid-motion", func() {
			// The fencing contract: the sweep re-dispatches the row
			// (epoch+1) while the motion runs; the refused heartbeat
			// aborts the motion and NO verdict is written — the
			// stale executor's write is never the row's outcome.
			_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
				Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: self,
			})
			Expect(err).NotTo(HaveOccurred())
			// A runner that sweeps the row mid-motion: the heartbeat
			// refusal is what it observes.
			runner.hang = true
			runner.hangFor = 2 * port.FleetTaskDefaultHeartbeatSeconds * time.Second
			exec, err := service.NewFleetTaskExecutor(store, runner, self)
			Expect(err).NotTo(HaveOccurred())
			// Pre-claim so the hang's first heartbeat races the
			// sweep's re-dispatch: claim under epoch 1.
			go func() {
				time.Sleep(500 * time.Millisecond)
				_, _ = store.RetryTask(ctx, port.FleetTaskVerdict{Table: table, TaskID: "refresh-writer-0", Epoch: 1})
			}()
			res, err := exec.Poll(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Claimed).To(Equal(1))
			rows, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
			Expect(err).NotTo(HaveOccurred())
			Expect(rows[0].Phase).NotTo(Equal(port.TaskDone), "the fenced executor's verdict never lands")
		})

		It("runs one motion per tick (the verdict closes the row first)", func() {
			for _, target := range []string{self, self} {
				_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
					Table: table, TaskID: "refresh-" + target + "-" + fmt.Sprint(time.Now().UnixNano()), Type: port.TaskRefresh, Target: target,
				})
				Expect(err).NotTo(HaveOccurred())
			}
			exec, err := service.NewFleetTaskExecutor(store, runner, self)
			Expect(err).NotTo(HaveOccurred())
			res, err := exec.Poll(ctx, table)
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Claimed).To(Equal(2), "both own-rows run in one tick — the poll is a loop over its own rows")
		})
	})
})

// fakeDisruptionGate is the budget seam's test double: canned verdicts.
type fakeDisruptionGate struct {
	verdict port.DisruptionVerdict
	err     error
	calls   int
}

func (f *fakeDisruptionGate) CheckDisruption(_ context.Context, _ port.DisruptionCheckRequest) (port.DisruptionVerdict, error) {
	f.calls++
	return f.verdict, f.err
}

var _ = Describe("FleetTaskExecutor disruption gate", func() {
	var (
		store  *fakes.FleetTaskStore
		runner *fakeMotionRunner
		gate   *fakeDisruptionGate
		ctx    context.Context
	)
	const table = "claims"
	const self = "writer-0"

	BeforeEach(func() {
		store = fakes.NewFleetTaskStore()
		runner = &fakeMotionRunner{}
		gate = &fakeDisruptionGate{}
		ctx = context.Background()
	})

	submit := func() {
		_, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
			Table: table, TaskID: "refresh-writer-0", Type: port.TaskRefresh, Target: self,
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("defers the claim when the budget refuses", func() {
		submit()
		gate.verdict = port.DisruptionVerdict{Allowed: false, Reason: "writer floor"}
		exec, err := service.NewFleetTaskExecutor(store, runner, self)
		Expect(err).NotTo(HaveOccurred())
		exec = exec.WithDisruptionGate(gate, "writer")
		res, err := exec.Poll(ctx, table)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Claimed).To(Equal(0), "the task stays pending — the next tick re-asks")
		Expect(gate.calls).To(Equal(1))
		rows, _ := store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
		Expect(rows[0].Phase).To(Equal(port.TaskPending))
	})

	It("claims when the budget allows", func() {
		submit()
		gate.verdict = port.DisruptionVerdict{Allowed: true}
		exec, err := service.NewFleetTaskExecutor(store, runner, self)
		Expect(err).NotTo(HaveOccurred())
		exec = exec.WithDisruptionGate(gate, "writer")
		res, err := exec.Poll(ctx, table)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Claimed).To(Equal(1))
		Expect(res.Completed).To(Equal(1))
	})

	It("defers on a gate transport error (never allows on no data)", func() {
		submit()
		gate.err = fmt.Errorf("status table unreachable")
		exec, err := service.NewFleetTaskExecutor(store, runner, self)
		Expect(err).NotTo(HaveOccurred())
		exec = exec.WithDisruptionGate(gate, "writer")
		res, err := exec.Poll(ctx, table)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Claimed).To(Equal(0), "a gate failure defers the motion, it never allows it")
	})

	It("runs ungated when no gate is wired (the degraded fleet)", func() {
		submit()
		exec, err := service.NewFleetTaskExecutor(store, runner, self)
		Expect(err).NotTo(HaveOccurred())
		res, err := exec.Poll(ctx, table)
		Expect(err).NotTo(HaveOccurred())
		Expect(res.Claimed).To(Equal(1))
	})
})
