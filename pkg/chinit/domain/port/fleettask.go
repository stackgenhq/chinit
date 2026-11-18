// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"
	"time"
)

// FleetTaskType is the kind of cross-node operation a task sequences.
// One task type per maintenance motion; the executor dispatches on it.
type FleetTaskType string

const (
	// TaskRefresh is a config refresh of one node: withdraw DNS,
	// drain, restart, catch up, re-advertise (the sequenced form of
	// `chinit boot --refresh`).
	TaskRefresh FleetTaskType = "refresh"
	// TaskRollout replaces a node with the new AMI (the ASG owns the
	// replacement; the task owns the gate and the verdict).
	TaskRollout FleetTaskType = "rollout"
	// TaskReseed drops stale replica metadata for a storage-loss node
	// and lets the heal path re-sync it.
	TaskReseed FleetTaskType = "reseed"
)

// FleetTaskPhase is the task row's state-machine position. Transitions
// are guarded by epoch-fenced CAS — a stale-epoch writer can never move
// a phase.
type FleetTaskPhase string

const (
	// TaskPending: submitted, not yet claimed by its target node.
	TaskPending FleetTaskPhase = "pending"
	// TaskAck: the target node claimed it and is about to execute.
	TaskAck FleetTaskPhase = "ack"
	// TaskExecuting: the target node is running the task's motion.
	TaskExecuting FleetTaskPhase = "executing"
	// TaskDone: completed successfully (row is sweepable).
	TaskDone FleetTaskPhase = "done"
	// TaskFailed: exhausted its retry budget; the coordinator re-
	// dispatches only with a new epoch (a human or a new submit).
	TaskFailed FleetTaskPhase = "failed"
)

// FleetTask is one cross-node operation's durable record. The row lives
// in the same table as the ordinal claims (the lease table is already
// the fleet's coordination point — a second table would split the
// liveness and task views of the same node).
type FleetTask struct {
	// TaskID is the unique id (submit-generated: type-host-epoch).
	TaskID string
	// Type is the motion this task sequences.
	Type FleetTaskType
	// Target is the hostname of the node that executes it.
	Target string
	// Phase is the current state-machine position.
	Phase FleetTaskPhase
	// Epoch is the fencing token: incremented on every coordinator
	// re-dispatch. An executor holding an older epoch is fenced off —
	// its Complete/Fail writes are refused by the CAS.
	Epoch int64
	// HeartbeatAt is the executor's last executing-phase heartbeat
	// (unix seconds; the coordinator re-dispatches when it goes stale).
	HeartbeatAt int64
	// HeartbeatSeconds is the executing heartbeat interval the
	// executor was told to keep.
	HeartbeatSeconds int
	// Retries counts re-dispatches so far.
	Retries int
	// MaxRetries is the re-dispatch budget before failed is terminal.
	MaxRetries int
	// Payload is the task-type-specific argument (e.g. the bundle SHA
	// a refresh should converge onto). Never a secret.
	Payload string
	// UpdatedAt is the row's last write time (unix seconds).
	UpdatedAt int64
}

// FleetTaskSubmit is the input to FleetTaskStore.SubmitTask.
type FleetTaskSubmit struct {
	// Table is the provider lease/task table (same table as claims).
	Table string
	// TaskID is the caller-chosen unique id. A resubmit with the same
	// TaskID while the row is live is refused (the pending task is
	// already queued); a resubmit over done/failed is a fresh epoch.
	TaskID string
	// Type is the motion to sequence.
	Type FleetTaskType
	// Target is the executing hostname.
	Target string
	// MaxRetries is the re-dispatch budget. Zero means "apply the
	// default" (adapters and the fake agree via
	// FleetTaskDefaultMaxRetries); -1 is the explicit zero-retry
	// budget (fail on the first staleness — the submit that wants NO
	// re-dispatch).
	MaxRetries int
	// Payload is the opaque, non-secret argument for the executor.
	Payload string
}

// FleetTaskClaim is the input to FleetTaskStore.ClaimTask: the target
// node's attempt to move its own pending task into executing.
type FleetTaskClaim struct {
	// Table is the provider lease/task table.
	Table string
	// TaskID identifies the row.
	TaskID string
	// Executor is the claiming hostname (must equal the row's Target —
	// the CAS refuses a foreign claim).
	Executor string
	// Epoch is the epoch the claim locks in; Complete/Fail must carry
	// the same epoch or they are fenced off.
	Epoch int64
	// HeartbeatSeconds is the executing heartbeat cadence the claim
	// sets on the row.
	HeartbeatSeconds int
}

// FleetTaskHeartbeat is the input to FleetTaskStore.HeartbeatTask: the
// executing node's liveness signal (epoch-fenced like every write).
type FleetTaskHeartbeat struct {
	// Table is the provider lease/task table.
	Table string
	// TaskID identifies the row.
	TaskID string
	// Executor must equal the row's Target and its last claimer.
	Executor string
	// Epoch fences stale executors.
	Epoch int64
}

// FleetTaskVerdict is the input to FleetTaskStore.CompleteTask and
// FailTask: the executor's terminal write (epoch-fenced).
type FleetTaskVerdict struct {
	// Table is the provider lease/task table.
	Table string
	// TaskID identifies the row.
	TaskID string
	// Executor must equal the row's Target.
	Executor string
	// Epoch fences stale executors.
	Epoch int64
	// Reason is the terminal note (failure cause or completion note;
	// never a secret).
	Reason string
}

// FleetTaskList is the input to ListFleetTasks.
type FleetTaskList struct {
	// Table is the provider lease/task table.
	Table string
	// Target filters by hostname when non-empty (the executor's poll
	// path); empty lists the whole fleet (the coordinator's sweep).
	Target string
}

// FleetTaskStore is the durable state machine for cross-node tasks.
//
// Why a port (and why the claims table): the operator sequences
// cross-node motions from one control loop; chinit has no control loop
// — only the claims table spans the fleet. The task rows live beside
// the claims (hostname-keyed rows, the same conditional-write
// primitives the claims already use) so liveness and work-intent share
// one table, one clock, and one failure domain. The coordinator (the
// ordinal-0 vacancy election — the same gate dead-replica-gc and
// autoscale use) sweeps and re-dispatches; every node's task-poll
// timer claims and executes its OWN row only.
//
// Fencing contract (fail-safe): every phase transition is a
// compare-and-swap on epoch. The coordinator increments the epoch on
// re-dispatch; an executor that comes back late (its heartbeat went
// stale mid-motion — a network partition, a paused clock) finds its
// terminal write refused and STOPS: it never acts on an epoch it no
// longer holds. A stale coordinator (it lost its own lease between the
// sweep and the dispatch) re-reads the row before any transition.
//
// Fault-tolerance contract: heartbeats carry deadlines; a dead
// executor's row is re-dispatched by the sweep once its heartbeat
// expires and retries remain. Exhausted rows go failed and stay put
// until a NEW submit (fresh epoch) — the system never invents retries.
//
// Self-healing contract: the coordinator's sweep is idempotent — it
// re-derives the pending/executing inventory from the table on every
// tick and acts only on stale rows (expired heartbeat with retries
// left, or done/failed rows past their sweep deadline).
type FleetTaskStore interface {
	// SubmitTask creates a pending task row. A live row with the same
	// TaskID is a refused duplicate (returns false, no error — the
	// "already queued" answer); a done/failed row is replaced with a
	// fresh epoch.
	SubmitTask(ctx context.Context, req FleetTaskSubmit) (bool, error)
	// ClaimTask moves the target's OWN pending task to executing with
	// the claiming epoch (refused for any other hostname, and for a
	// row already executing under a live heartbeat).
	ClaimTask(ctx context.Context, req FleetTaskClaim) (bool, error)
	// HeartbeatTask extends the executing row's liveness deadline.
	// Refused (false, no error) when the epoch is stale — the caller
	// must abort its motion.
	HeartbeatTask(ctx context.Context, req FleetTaskHeartbeat) (bool, error)
	// CompleteTask writes the done verdict (epoch-fenced; a stale
	// epoch is refused, not an error).
	CompleteTask(ctx context.Context, req FleetTaskVerdict) (bool, error)
	// FailTask writes the failed verdict (epoch-fenced).
	FailTask(ctx context.Context, req FleetTaskVerdict) (bool, error)
	// ListFleetTasks returns the fleet's task rows (Target filter
	// when non-empty), oldest first.
	ListFleetTasks(ctx context.Context, req FleetTaskList) ([]FleetTask, error)
	// RetryTask re-dispatches a stale or failed row: heartbeat reset,
	// retries+1, epoch+1. Refused when retries are exhausted (the
	// terminal failed state is the caller's answer) or the row is
	// done. The claimant must be the coordinator's identity (the
	// adapter does not verify the caller — the sweep runs under the
	// maintenance gate; the CAS on epoch is the atomicity primitive).
	RetryTask(ctx context.Context, req FleetTaskVerdict) (bool, error)
}

// AsFleetTaskStore returns the fleet-task capability, or nil.
func AsFleetTaskStore(p Provider) FleetTaskStore {
	if c, ok := p.(FleetTaskStore); ok {
		return c
	}
	return nil
}

// MotionRunner executes one claimed task's motion — the side-effecting
// half of the fleet task layer. The executor service owns sequencing
// (claim, heartbeat, verdict); the runner owns WHAT the motion does.
//
// Why a port: the motion is the layer's only side-effecting seam.
// `boot --refresh` shells a subprocess today; a drain-aware refresh
// (withdraw DNS → drain → restart → catch up → re-advertise) replaces
// the implementation, never the orchestration contract. Tests inject
// a fake runner and assert the sequencing, not the subprocess.
type MotionRunner interface {
	// RunMotion executes the task's motion under ctx. The runner must
	// be idempotent (a re-dispatch re-claims the same motion after a
	// crash) and must respect ctx (the executor's poll deadline).
	RunMotion(ctx context.Context, task FleetTask) error
}

// DisruptionGate answers whether ONE voluntary disruption of the
// asking role fits the fleet's disruption budget (the operator's
// PodDisruptionBudget equivalent). The executor consults it before
// claiming a motion; a deferred verdict leaves the task row pending —
// the next tick re-asks.
//
// Why a port: the gate needs the fleet view (live claims + status
// rows) — cloud calls the domain never makes. The app implementation
// lists the status rows and folds them through the pure budget logic
// (service.CheckDisruptionBudget); tests inject a canned verdict.
type DisruptionGate interface {
	// CheckDisruption returns the verdict for one voluntary
	// disruption. An error is a transport failure (the caller defers
	// the motion, never allows on error).
	CheckDisruption(ctx context.Context, req DisruptionCheckRequest) (DisruptionVerdict, error)
}

// DisruptionCheckRequest is the input to DisruptionGate.
type DisruptionCheckRequest struct {
	// Role is the asking node's role (the floor applies per role).
	Role string
}

// DisruptionVerdict is the gate's answer (Allowed false defers; Reason
// names the cause — the caller logs it, never overrides it).
type DisruptionVerdict struct {
	Allowed bool
	Reason  string
}

// FleetTaskDefaults are the task-row defaults adapters apply when the
// request leaves a field zero (kept here so both cloud adapters and
// the fake agree on the same contract).
const (
	// FleetTaskDefaultMaxRetries bounds re-dispatches per task.
	FleetTaskDefaultMaxRetries = 3
	// FleetTaskDefaultHeartbeatSeconds is the executing liveness
	// cadence (the coordinator re-dispatches at 2x — one missed
	// heartbeat is network noise, two is a dead executor).
	FleetTaskDefaultHeartbeatSeconds = 60
	// FleetTaskHeartbeatStaleFactor: a heartbeat older than
	// HeartbeatSeconds * this factor marks the row stale.
	FleetTaskHeartbeatStaleFactor = 3
)

// FleetTaskStale reports whether an executing row's heartbeat is past
// its deadline — the coordinator's re-dispatch trigger. Pure function
// over the row (the sweep and the tests share it).
func FleetTaskStale(t FleetTask, now time.Time) bool {
	if t.Phase != TaskExecuting && t.Phase != TaskAck {
		return false
	}
	deadline := t.HeartbeatAt + int64(t.HeartbeatSeconds*FleetTaskHeartbeatStaleFactor)
	return now.Unix() > deadline
}

// FleetTaskTerminal reports whether the row is in a terminal phase the
// sweep can retire (the done/failed residue a submit replaces).
func FleetTaskTerminal(t FleetTask) bool {
	return t.Phase == TaskDone || t.Phase == TaskFailed
}
