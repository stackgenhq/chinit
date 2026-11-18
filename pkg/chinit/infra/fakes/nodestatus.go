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

// StatusPublisher is the in-memory status store: the conformance
// suite's reference implementation and the unit tests' fake. The CAS
// semantics mirror the real stores — a foreign instance's live row is
// refused, an expired row is reclaimable.
type StatusPublisher struct {
	mu  sync.Mutex
	now func() time.Time
	// rows maps hostname to the row.
	rows map[string]*port.NodeStatus
}

// NewStatusPublisher builds the fake with the real clock.
func NewStatusPublisher() *StatusPublisher {
	return &StatusPublisher{
		now:  time.Now,
		rows: map[string]*port.NodeStatus{},
	}
}

// WithNow injects the clock (the TTL scenarios).
func (s *StatusPublisher) WithNow(f func() time.Time) *StatusPublisher {
	s.now = f
	return s
}

// PublishStatus implements port.StatusPublisher.
func (s *StatusPublisher) PublishStatus(_ context.Context, req port.NodeStatusPublish) error {
	if req.Status.Hostname == "" {
		return errStatus("publish requires Hostname")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, live := s.rows[req.Status.Hostname]; live {
		if row.InstanceID != req.Status.InstanceID && row.UpdatedAt > s.now().Add(-15*time.Minute).Unix() {
			return errStatus("another instance holds the " + req.Status.Hostname + " status row (identity moved?)")
		}
		// Preserve the cordoned flag a toggle set between publishes
		// (the publish owns the node's fields; the toggle owns the
		// flag — mirrors the cloud adapters' targeted update).
		cordoned := row.Cordoned
		row.Role = req.Status.Role
		row.InstanceID = req.Status.InstanceID
		row.SpecHash = req.Status.SpecHash
		row.ClickHouseVersion = req.Status.ClickHouseVersion
		row.Health = req.Status.Health
		row.KeeperQuorum = req.Status.KeeperQuorum
		row.Cordoned = cordoned
		row.UpdatedAt = s.now().Unix()
		return nil
	}
	row := req.Status
	row.UpdatedAt = s.now().Unix()
	s.rows[req.Status.Hostname] = &row
	return nil
}

// ListStatus implements port.StatusPublisher (Hostname filter, sorted).
func (s *StatusPublisher) ListStatus(_ context.Context, req port.NodeStatusList) ([]port.NodeStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var statuses []port.NodeStatus
	for _, row := range s.rows {
		if req.Hostname != "" && row.Hostname != req.Hostname {
			continue
		}
		statuses = append(statuses, *row)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Hostname < statuses[j].Hostname })
	return statuses, nil
}

// SetCordon implements port.StatusPublisher.
func (s *StatusPublisher) SetCordon(_ context.Context, req port.NodeCordonRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[req.Hostname]
	if !ok {
		return errStatus("cordon " + req.Hostname + ": no status row (the node has never published)")
	}
	row.Cordoned = req.Cordoned
	row.UpdatedAt = s.now().Unix()
	return nil
}

// errStatus is the fake's typed error (kept minimal: the scenarios
// assert on message content, not a sentinel).
type errStatus string

func (e errStatus) Error() string { return string(e) }

// Compile-time capability check.
var _ port.StatusPublisher = (*StatusPublisher)(nil)
