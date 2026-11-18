// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// FleetTaskSweepResult reports one sweep tick's decisions (the CLI
// prints it; the exporter counts it).
type FleetTaskSweepResult struct {
	// Rows is the inventory the sweep saw (all phases).
	Rows int
	// Redispatched counts rows the sweep re-dispatched (stale
	// heartbeat, retries remaining).
	Redispatched int
	// Failed counts rows the sweep moved to failed (stale heartbeat,
	// retries exhausted).
	Failed int
	// LeftPending counts rows awaiting their first claim (untouched).
	LeftPending int
	// LiveExecuting counts rows with a fresh heartbeat (untouched).
	LiveExecuting int
}

// FleetTaskService runs the coordinator sweep over the task store.
type FleetTaskService struct {
	store port.FleetTaskStore
	// now is the clock seam (the staleness scenarios inject a fixed
	// or offset clock without sleeping).
	now func() time.Time
}

// NewFleetTaskService builds the service over its port. Without it the
// fleet has no coordinator — the timer logs the missing capability and
// exits 0 (degradation is the factory's designed behavior, the same
// rule as the lease lister).
func NewFleetTaskService(store port.FleetTaskStore) (*FleetTaskService, error) {
	if store == nil {
		return nil, fmt.Errorf("fleet-task service: store is nil")
	}
	return &FleetTaskService{store: store, now: time.Now}, nil
}

// WithNow injects the clock (the staleness specs).
func (s *FleetTaskService) WithNow(f func() time.Time) *FleetTaskService {
	s.now = f
	return s
}

// Sweep is one coordinator tick: list every task row, re-dispatch the
// stale ones, fail the exhausted ones. Idempotent — it re-derives the
// inventory from the table every tick and only acts on stale rows.
//
// The fail-safe rules, per row:
//   - a live-executing row (fresh heartbeat) is never touched;
//   - a stale executing/ack row with retries remaining is re-
//     dispatched (epoch+1 — the old executor is fenced off on its next
//     write);
//   - a stale row with NO retries remaining is failed (terminal; a
//     human or a fresh submit decides the next move — the sweep never
//     invents retries);
//   - a transition refused by the CAS (another coordinator raced and
//     won) is recorded in the result, never an error — the next tick
//     sees the winner's decision.
func (s *FleetTaskService) Sweep(ctx context.Context, table string) (*FleetTaskSweepResult, error) {
	rows, err := s.store.ListFleetTasks(ctx, port.FleetTaskList{Table: table})
	if err != nil {
		return nil, fmt.Errorf("fleet-task sweep list: %w", err)
	}
	now := s.now()
	res := &FleetTaskSweepResult{Rows: len(rows)}
	for _, row := range rows {
		switch row.Phase {
		case port.TaskPending:
			res.LeftPending++
			continue
		case port.TaskExecuting, port.TaskAck:
			if !port.FleetTaskStale(row, now) {
				res.LiveExecuting++
				continue
			}
			verdict := port.FleetTaskVerdict{
				Table:    table,
				TaskID:   row.TaskID,
				Executor: row.Target,
				Epoch:    row.Epoch,
				Reason:   "stale heartbeat re-dispatch",
			}
			if row.Retries < row.MaxRetries {
				ok, err := s.store.RetryTask(ctx, verdict)
				if err != nil {
					return nil, fmt.Errorf("fleet-task sweep retry %s: %w", row.TaskID, err)
				}
				if ok {
					res.Redispatched++
				}
				continue
			}
			ok, err := s.store.FailTask(ctx, verdict)
			if err != nil {
				return nil, fmt.Errorf("fleet-task sweep fail %s: %w", row.TaskID, err)
			}
			if ok {
				res.Failed++
			}
		default:
			// done/failed rows are terminal residue; the submit path
			// replaces them. Nothing to do this tick.
			continue
		}
	}
	return res, nil
}

// FleetTaskExecutorResult reports one poll tick (the timer's journal
// line and the CLI's exit text).
type FleetTaskExecutorResult struct {
	// Claimed counts rows this poll claimed (0 = nothing to do).
	Claimed int
	// Completed counts motions that finished and wrote done.
	Completed int
	// Failed counts motions that failed and wrote failed.
	Failed int
}

// FleetTaskExecutor is the node-side half of the task layer: poll this
// node's OWN rows, claim a pending one, run its motion under a
// heartbeat, and write the epoch-fenced verdict. Pure orchestration
// over the store and the MotionRunner port — the subprocess shell, the
// drain-aware refresh, and every future motion are runner concerns.
type FleetTaskExecutor struct {
	store  port.FleetTaskStore
	runner port.MotionRunner
	// gate is the optional disruption budget (nil = the motion runs
	// ungated — a fleet without the status capability degrades loudly
	// at the app layer, and the executor's contract stays pure).
	gate port.DisruptionGate
	// role is the asking node's role (the gate's floor input).
	role string
	// hostname is the executor's identity — the claim's CAS refuses any
	// row whose Target differs (the node never executes another
	// node's task).
	hostname string
	now      func() time.Time
}

// NewFleetTaskExecutor builds the executor over its ports.
func NewFleetTaskExecutor(store port.FleetTaskStore, runner port.MotionRunner, hostname string) (*FleetTaskExecutor, error) {
	if store == nil {
		return nil, fmt.Errorf("fleet-task executor: store is nil")
	}
	if runner == nil {
		return nil, fmt.Errorf("fleet-task executor: runner is nil")
	}
	if hostname == "" {
		return nil, fmt.Errorf("fleet-task executor: hostname is empty (the claim needs the settled identity)")
	}
	return &FleetTaskExecutor{store: store, runner: runner, hostname: hostname, now: time.Now}, nil
}

// WithDisruptionGate wires the budget gate (optional: nil-gated is the
// degraded fleet's answer; the app layer logs the missing capability).
// The gate is consulted before the claim; a deferred verdict leaves
// the row pending — the next tick re-asks.
func (e *FleetTaskExecutor) WithDisruptionGate(gate port.DisruptionGate, role string) *FleetTaskExecutor {
	e.gate = gate
	e.role = role
	return e
}

// WithNow injects the clock (the fencing specs).
func (e *FleetTaskExecutor) WithNow(f func() time.Time) *FleetTaskExecutor {
	e.now = f
	return e
}

// Poll is one tick: claim and run this node's pending task rows (one
// motion per tick — the verdict closes the row before the next).
//
// The fail-safe rules:
//   - a row in executing with a LIVE heartbeat is another process's
//     motion — never touched (a stale one is the sweep's call, not
//     the executor's);
//   - a motion whose heartbeat is refused (the epoch moved) aborts:
//     the stale executor never writes a verdict.
func (e *FleetTaskExecutor) Poll(ctx context.Context, table string) (*FleetTaskExecutorResult, error) {
	rows, err := e.store.ListFleetTasks(ctx, port.FleetTaskList{Table: table, Target: e.hostname})
	if err != nil {
		return nil, fmt.Errorf("fleet-task poll list: %w", err)
	}
	res := &FleetTaskExecutorResult{}
	for _, row := range rows {
		if port.FleetTaskTerminal(row) {
			continue
		}
		if row.Phase != port.TaskPending {
			// Executing rows (live or stale) are untouched here: live
			// is another motion's row; stale is the coordinator's
			// re-dispatch decision.
			continue
		}
		if e.gate != nil {
			// The disruption budget: one voluntary disruption must fit
			// the fleet's healthy floors BEFORE the claim (a claimed
			// motion is a commitment; a deferred one stays pending —
			// the next tick re-asks).
			verdict, err := e.gate.CheckDisruption(ctx, port.DisruptionCheckRequest{Role: e.role})
			if err != nil {
				// A gate transport failure defers, never allows (the
				// same fail-safe as every "no data" answer).
				continue
			}
			if !verdict.Allowed {
				continue
			}
		}
		claimed, err := e.store.ClaimTask(ctx, port.FleetTaskClaim{
			Table:            table,
			TaskID:           row.TaskID,
			Executor:         e.hostname,
			Epoch:            row.Epoch,
			HeartbeatSeconds: port.FleetTaskDefaultHeartbeatSeconds,
		})
		if err != nil {
			return nil, fmt.Errorf("fleet-task poll claim %s: %w", row.TaskID, err)
		}
		if !claimed {
			continue
		}
		res.Claimed++
		completed, err := e.executeMotion(ctx, table, row)
		if err != nil {
			return res, err
		}
		if completed {
			res.Completed++
		} else {
			res.Failed++
		}
	}
	return res, nil
}

// executeMotion runs one claimed row's motion: the heartbeat loop
// extends the lease while the motion runs; the epoch-fenced verdict
// closes the row. A refused heartbeat means the sweep re-dispatched —
// the motion aborts and the row is left for the new claim (the motion
// itself must be idempotent; the re-claim re-runs it cleanly).
//
// The motion and its heartbeat run as one errgroup: the heartbeat's
// refusal cancels the motion's context (the runner must observe ctx —
// the port contract), and the motion's return cancels the heartbeat.
// The FIRST error wins — a fenced heartbeat outranks a motion error
// (the fence means the verdict write would be refused anyway).
//
// Returns (completed, err): completed=false with err=nil only on a
// fenced motion (the caller counts it failed; the sweep owns the row).
func (e *FleetTaskExecutor) executeMotion(ctx context.Context, table string, row port.FleetTask) (bool, error) {
	verdict := port.FleetTaskVerdict{
		Table:    table,
		TaskID:   row.TaskID,
		Executor: e.hostname,
		Epoch:    row.Epoch,
	}
	// fenced reports the heartbeat goroutine's refusal (the epoch
	// moved — the sweep re-dispatched the row).
	var fenced bool
	// motionDone unblocks the heartbeat the moment the motion returns
	// (a ticker-only select would park until the NEXT tick — up to a
	// whole cadence after the motion is finished).
	motionDone := make(chan struct{})
	g, gctx := errgroup.WithContext(ctx)
	var motionErr error
	g.Go(func() error {
		defer close(motionDone)
		motionErr = e.runner.RunMotion(gctx, row)
		return motionErr
	})
	g.Go(func() error {
		ticker := time.NewTicker(time.Duration(row.HeartbeatSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-motionDone:
				return nil
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				ok, err := e.store.HeartbeatTask(gctx, port.FleetTaskHeartbeat{
					Table:    table,
					TaskID:   row.TaskID,
					Executor: e.hostname,
					Epoch:    row.Epoch,
				})
				if err != nil {
					// A transport error never aborts the motion: the
					// sweep's staleness deadline is the backstop.
					continue
				}
				if !ok {
					// Fenced: the epoch moved. Cancel the motion —
					// the runner's ctx observation is the abort.
					fenced = true
					return errFenced{task: row.TaskID}
				}
			}
		}
	})
	_ = g.Wait()
	if fenced {
		// The task was taken away mid-motion: never write the verdict
		// (the CAS would refuse it); the sweep owns the row.
		return false, nil
	}
	if motionErr != nil {
		verdict.Reason = motionErr.Error()
		if _, err := e.store.FailTask(ctx, verdict); err != nil {
			return false, fmt.Errorf("fleet-task fail %s: %w", row.TaskID, err)
		}
		return false, fmt.Errorf("fleet-task motion %s failed: %w", row.TaskID, motionErr)
	}
	verdict.Reason = "motion complete"
	if _, err := e.store.CompleteTask(ctx, verdict); err != nil {
		return false, fmt.Errorf("fleet-task complete %s: %w", row.TaskID, err)
	}
	return true, nil
}

// errFenced marks the heartbeat-goroutine refusal (the first-error
// carrier that outranks a motion error in the errgroup).
type errFenced struct{ task string }

func (e errFenced) Error() string {
	return "fleet task " + e.task + " was fenced (re-dispatched while executing)"
}
