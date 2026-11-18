// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// FleetTaskStore is the in-memory fleet-task store: the conformance
// suite's reference implementation and the unit tests' fake. The CAS
// semantics mirror the real stores exactly — a claim/heartbeat/verdict
// that loses a race takes the refused answer, never an error.
//
// The Now hook is the clock seam: the sweep-staleness scenarios inject
// a fixed or offset clock without sleeping.
type FleetTaskStore struct {
	mu  sync.Mutex
	now func() int64
	// rows maps TaskID to the row (a map, not a slice: the CAS is on
	// the row's fields, not the list order).
	rows map[string]*port.FleetTask
}

// NewFleetTaskStore builds the fake with the real clock.
func NewFleetTaskStore() *FleetTaskStore {
	return &FleetTaskStore{
		now:  func() int64 { return time.Now().Unix() },
		rows: map[string]*port.FleetTask{},
	}
}

// WithNow injects the clock (the fault-injection scenarios).
func (s *FleetTaskStore) WithNow(f func() int64) *FleetTaskStore {
	s.now = f
	return s
}

// SubmitTask implements port.FleetTaskStore: insert, replace terminal,
// refuse live duplicates.
func (s *FleetTaskStore) SubmitTask(_ context.Context, req port.FleetTaskSubmit) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, live := s.rows[req.TaskID]; live && !port.FleetTaskTerminal(*row) {
		return false, nil
	}
	maxRetries := req.MaxRetries
	switch {
	case maxRetries > 0:
	case maxRetries == -1:
		maxRetries = 0
	default:
		maxRetries = port.FleetTaskDefaultMaxRetries
	}
	s.rows[req.TaskID] = &port.FleetTask{
		TaskID:           req.TaskID,
		Type:             req.Type,
		Target:           req.Target,
		Phase:            port.TaskPending,
		Epoch:            1,
		HeartbeatAt:      s.now(),
		HeartbeatSeconds: port.FleetTaskDefaultHeartbeatSeconds,
		MaxRetries:       maxRetries,
		Payload:          req.Payload,
		UpdatedAt:        s.now(),
	}
	return true, nil
}

// ClaimTask implements port.FleetTaskStore: the target's own pending
// row moves to executing under the claimed epoch.
func (s *FleetTaskStore) ClaimTask(_ context.Context, req port.FleetTaskClaim) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[req.TaskID]
	if !ok || row.Phase != port.TaskPending || row.Target != req.Executor {
		return false, nil
	}
	hb := req.HeartbeatSeconds
	if hb <= 0 {
		hb = port.FleetTaskDefaultHeartbeatSeconds
	}
	row.Phase = port.TaskExecuting
	row.Epoch = req.Epoch
	row.HeartbeatSeconds = hb
	row.HeartbeatAt = s.now()
	row.UpdatedAt = s.now()
	return true, nil
}

// HeartbeatTask implements port.FleetTaskStore: epoch-fenced extension.
func (s *FleetTaskStore) HeartbeatTask(_ context.Context, req port.FleetTaskHeartbeat) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[req.TaskID]
	if !ok || row.Phase != port.TaskExecuting || row.Target != req.Executor || row.Epoch != req.Epoch {
		return false, nil
	}
	row.HeartbeatAt = s.now()
	row.UpdatedAt = s.now()
	return true, nil
}

// terminalWrite is the shared verdict path (executing/ack → terminal,
// epoch-fenced).
func (s *FleetTaskStore) terminalWrite(req port.FleetTaskVerdict, phase port.FleetTaskPhase) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[req.TaskID]
	if !ok || (row.Phase != port.TaskExecuting && row.Phase != port.TaskAck) ||
		row.Target != req.Executor || row.Epoch != req.Epoch {
		return false, nil
	}
	row.Phase = phase
	row.UpdatedAt = s.now()
	return true, nil
}

// CompleteTask implements port.FleetTaskStore.
func (s *FleetTaskStore) CompleteTask(_ context.Context, req port.FleetTaskVerdict) (bool, error) {
	return s.terminalWrite(req, port.TaskDone)
}

// FailTask implements port.FleetTaskStore.
func (s *FleetTaskStore) FailTask(_ context.Context, req port.FleetTaskVerdict) (bool, error) {
	return s.terminalWrite(req, port.TaskFailed)
}

// RetryTask implements port.FleetTaskStore: epoch+1, retries+1, back
// to pending; refused at the budget or when done.
func (s *FleetTaskStore) RetryTask(_ context.Context, req port.FleetTaskVerdict) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[req.TaskID]
	if !ok || row.Phase == port.TaskDone {
		return false, nil
	}
	if row.Retries >= row.MaxRetries {
		return false, nil
	}
	row.Phase = port.TaskPending
	row.Epoch++
	row.Retries++
	row.HeartbeatAt = s.now()
	row.UpdatedAt = s.now()
	return true, nil
}

// ListFleetTasks implements port.FleetTaskStore (Target filter,
// oldest first).
func (s *FleetTaskStore) ListFleetTasks(_ context.Context, req port.FleetTaskList) ([]port.FleetTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var tasks []port.FleetTask
	for _, row := range s.rows {
		if req.Target != "" && row.Target != req.Target {
			continue
		}
		tasks = append(tasks, *row)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].UpdatedAt < tasks[j].UpdatedAt })
	return tasks, nil
}

// Compile-time capability check.
var _ port.FleetTaskStore = (*FleetTaskStore)(nil)
