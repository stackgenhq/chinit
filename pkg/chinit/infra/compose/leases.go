// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	chinitsvc "github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

const defaultLeaseFile = "/var/lib/chinit-cluster/leases.json"

type leaseRecord struct {
	Table      string `json:"table"`
	Hostname   string `json:"hostname"`
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
	LeaseUntil int64  `json:"lease_until"`
}

type statusRecord struct {
	Table             string          `json:"table"`
	Hostname          string          `json:"hostname"`
	Role              string          `json:"role"`
	InstanceID        string          `json:"instance_id"`
	SpecHash          string          `json:"spec_hash,omitempty"`
	ClickHouseVersion string          `json:"clickhouse_version,omitempty"`
	Health            port.NodeHealth `json:"health"`
	KeeperQuorum      bool            `json:"keeper_quorum"`
	Cordoned          bool            `json:"cordoned"`
	UpdatedAt         int64           `json:"updated_at"`
}

type lockRecord struct {
	Table     string `json:"table"`
	Key       string `json:"key"`
	Holder    string `json:"holder"`
	ExpiresAt int64  `json:"expires_at"`
}

type taskRecord struct {
	Table string         `json:"table"`
	Task  port.FleetTask `json:"task"`
}

type clusterStateFile struct {
	Leases   map[string]leaseRecord  `json:"leases"`
	Statuses map[string]statusRecord `json:"statuses"`
	Locks    map[string]lockRecord   `json:"locks"`
	Tasks    map[string]taskRecord   `json:"tasks"`
}

// FileLeaseStore coordinates ordinal leases, node statuses, and refresh
// locks across Docker Compose containers via a flock-guarded JSON file.
type FileLeaseStore struct {
	path string
	now  func() time.Time
}

// NewFileLeaseStore constructs a lease store backed by CHINIT_COMPOSE_LEASE_FILE
// or /var/lib/chinit-cluster/leases.json.
func NewFileLeaseStore() *FileLeaseStore {
	return &FileLeaseStore{
		path: firstNonEmpty(os.Getenv("CHINIT_COMPOSE_LEASE_FILE"), defaultLeaseFile),
		now:  time.Now,
	}
}

// WithPath overrides the lease file path (tests).
func (s *FileLeaseStore) WithPath(path string) *FileLeaseStore {
	s.path = path
	return s
}

func (s *FileLeaseStore) withLock(fn func(state *clusterStateFile) (bool, error)) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("create lease dir: %w", err)
	}
	lockPath := s.path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("open lease lock %s: %w", lockPath, err)
	}
	defer func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("flock %s: %w", lockPath, err)
	}

	state := &clusterStateFile{
		Leases:   map[string]leaseRecord{},
		Statuses: map[string]statusRecord{},
		Locks:    map[string]lockRecord{},
		Tasks:    map[string]taskRecord{},
	}
	if raw, err := os.ReadFile(s.path); err == nil && len(raw) > 0 {
		if uerr := json.Unmarshal(raw, state); uerr != nil {
			return fmt.Errorf("parse %s: %w", s.path, uerr)
		}
		if state.Leases == nil {
			state.Leases = map[string]leaseRecord{}
		}
		if state.Statuses == nil {
			state.Statuses = map[string]statusRecord{}
		}
		if state.Locks == nil {
			state.Locks = map[string]lockRecord{}
		}
		if state.Tasks == nil {
			state.Tasks = map[string]taskRecord{}
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.path, err)
	}

	dirty, err := fn(state)
	if err != nil {
		return err
	}
	if !dirty {
		return nil
	}

	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", s.path, err)
	}
	tmp := fmt.Sprintf("%s.tmp.%d", s.path, os.Getpid())
	if err := os.WriteFile(tmp, encoded, 0644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}

func recordKey(table, name string) string {
	return table + "\x00" + name
}

// ClaimOrdinal claims the first free or expired ordinal in [0, req.Count)
// (or the exact ordinal req.Count-1 when req.ExactOrdinal is true).
func (s *FileLeaseStore) ClaimOrdinal(ctx context.Context, req port.ClaimRequest) (int, error) {
	if req.Count <= 0 {
		return -1, chinitsvc.Fatal(fmt.Errorf("compose claim: count must be > 0 (got %d)", req.Count))
	}
	leaseSec := req.LeaseSeconds
	if leaseSec <= 0 {
		leaseSec = 900
	}
	deadline := time.Now().Add(time.Duration(maxInt(req.DeadlineSeconds, 30)) * time.Second)

	startOrd, endOrd := 0, req.Count-1
	if req.ExactOrdinal {
		startOrd, endOrd = req.Count-1, req.Count-1
	}

	for {
		claimed := -1
		err := s.withLock(func(state *clusterStateFile) (bool, error) {
			nowUnix := s.now().Unix()
			for ord := startOrd; ord <= endOrd; ord++ {
				hostname := fmt.Sprintf("%s%d", req.Prefix, ord)
				key := recordKey(req.Table, hostname)
				cur, exists := state.Leases[key]
				if !exists || cur.LeaseUntil <= nowUnix || cur.InstanceID == req.InstanceID {
					state.Leases[key] = leaseRecord{
						Table:      req.Table,
						Hostname:   hostname,
						InstanceID: req.InstanceID,
						IP:         req.PrivateIP,
						LeaseUntil: nowUnix + int64(leaseSec),
					}
					claimed = ord
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return -1, err
		}
		if claimed >= 0 {
			return claimed, nil
		}
		if time.Now().After(deadline) {
			return -1, fmt.Errorf("compose claim: no free ordinal in %s[0..%d) before deadline", req.Prefix, req.Count)
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// RenewLease extends the lease if still owned by req.InstanceID.
func (s *FileLeaseStore) RenewLease(_ context.Context, req port.RenewRequest) error {
	leaseSec := req.LeaseSeconds
	if leaseSec <= 0 {
		leaseSec = 900
	}
	return s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.Hostname)
		cur, exists := state.Leases[key]
		if !exists || cur.InstanceID != req.InstanceID {
			return false, fmt.Errorf("%w: %s (holder=%q, want=%q)", port.ErrLeaseStolen, req.Hostname, cur.InstanceID, req.InstanceID)
		}
		cur.LeaseUntil = s.now().Unix() + int64(leaseSec)
		state.Leases[key] = cur
		return true, nil
	})
}

// ReleaseLease removes the lease row when owned by req.InstanceID.
func (s *FileLeaseStore) ReleaseLease(_ context.Context, req port.ReleaseRequest) error {
	return s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.Hostname)
		cur, exists := state.Leases[key]
		if !exists {
			return false, nil
		}
		if req.InstanceID != "" && cur.InstanceID != req.InstanceID {
			return false, nil
		}
		delete(state.Leases, key)
		return true, nil
	})
}

// ListActiveClaims returns every non-expired lease row in req.Table.
func (s *FileLeaseStore) ListActiveClaims(_ context.Context, req port.ListClaimsRequest) ([]port.Claim, error) {
	var claims []port.Claim
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		nowUnix := s.now().Unix()
		for _, rec := range state.Leases {
			if rec.Table != req.Table || rec.LeaseUntil <= nowUnix {
				continue
			}
			claims = append(claims, port.Claim{
				Hostname:   rec.Hostname,
				InstanceID: rec.InstanceID,
				IP:         rec.IP,
			})
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Hostname < claims[j].Hostname })
	return claims, nil
}

// PublishStatus writes or updates a node's fleet status row while preserving its cordoned flag.
func (s *FileLeaseStore) PublishStatus(_ context.Context, req port.NodeStatusPublish) error {
	if req.Status.Hostname == "" {
		return fmt.Errorf("compose status: hostname is required")
	}
	return s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.Status.Hostname)
		existing := state.Statuses[key]
		updatedAt := req.Status.UpdatedAt
		if updatedAt == 0 {
			updatedAt = s.now().Unix()
		}
		state.Statuses[key] = statusRecord{
			Table:             req.Table,
			Hostname:          req.Status.Hostname,
			Role:              req.Status.Role,
			InstanceID:        req.Status.InstanceID,
			SpecHash:          req.Status.SpecHash,
			ClickHouseVersion: req.Status.ClickHouseVersion,
			Health:            req.Status.Health,
			KeeperQuorum:      req.Status.KeeperQuorum,
			Cordoned:          existing.Cordoned || req.Status.Cordoned,
			UpdatedAt:         updatedAt,
		}
		return true, nil
	})
}

// ListStatus returns node status rows from req.Table.
func (s *FileLeaseStore) ListStatus(_ context.Context, req port.NodeStatusList) ([]port.NodeStatus, error) {
	var out []port.NodeStatus
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		for _, rec := range state.Statuses {
			if rec.Table != req.Table {
				continue
			}
			if req.Hostname != "" && rec.Hostname != req.Hostname {
				continue
			}
			out = append(out, port.NodeStatus{
				Hostname:          rec.Hostname,
				Role:              rec.Role,
				InstanceID:        rec.InstanceID,
				SpecHash:          rec.SpecHash,
				ClickHouseVersion: rec.ClickHouseVersion,
				Health:            rec.Health,
				KeeperQuorum:      rec.KeeperQuorum,
				Cordoned:          rec.Cordoned,
				UpdatedAt:         rec.UpdatedAt,
			})
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

// SetCordon sets or clears the cordoned flag on a node status row.
func (s *FileLeaseStore) SetCordon(_ context.Context, req port.NodeCordonRequest) error {
	if req.Hostname == "" {
		return fmt.Errorf("compose cordon: hostname is required")
	}
	return s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.Hostname)
		rec := state.Statuses[key]
		rec.Table = req.Table
		rec.Hostname = req.Hostname
		rec.Cordoned = req.Cordoned
		rec.UpdatedAt = s.now().Unix()
		state.Statuses[key] = rec
		return true, nil
	})
}

// AcquireRefreshLock takes a per-role refresh lock if unheld or expired.
func (s *FileLeaseStore) AcquireRefreshLock(_ context.Context, req port.RefreshLockRequest) (bool, error) {
	acquired := false
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		nowUnix := s.now().Unix()
		key := recordKey(req.Table, req.Key)
		cur, exists := state.Locks[key]
		if !exists || cur.ExpiresAt <= nowUnix || cur.Holder == req.Holder {
			ttl := req.Minutes
			if ttl <= 0 {
				ttl = 10
			}
			state.Locks[key] = lockRecord{
				Table:     req.Table,
				Key:       req.Key,
				Holder:    req.Holder,
				ExpiresAt: nowUnix + int64(ttl*60),
			}
			acquired = true
			return true, nil
		}
		return false, nil
	})
	return acquired, err
}

// ReleaseRefreshLock releases the per-role refresh lock held by req.Holder.
func (s *FileLeaseStore) ReleaseRefreshLock(_ context.Context, req port.RefreshLockRequest) error {
	return s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.Key)
		cur, exists := state.Locks[key]
		if !exists || cur.Holder != req.Holder {
			return false, nil
		}
		delete(state.Locks, key)
		return true, nil
	})
}

// SubmitTask creates or re-submits a fleet task row.
func (s *FileLeaseStore) SubmitTask(_ context.Context, req port.FleetTaskSubmit) (bool, error) {
	submitted := false
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.TaskID)
		nowUnix := s.now().Unix()
		var nextEpoch int64 = 1
		if cur, exists := state.Tasks[key]; exists {
			if !port.FleetTaskTerminal(cur.Task) {
				return false, nil
			}
			nextEpoch = cur.Task.Epoch + 1
		}
		maxRetries := req.MaxRetries
		if maxRetries == 0 {
			maxRetries = port.FleetTaskDefaultMaxRetries
		} else if maxRetries < 0 {
			maxRetries = 0
		}
		state.Tasks[key] = taskRecord{
			Table: req.Table,
			Task: port.FleetTask{
				TaskID:           req.TaskID,
				Type:             req.Type,
				Target:           req.Target,
				Phase:            port.TaskPending,
				Epoch:            nextEpoch,
				HeartbeatSeconds: port.FleetTaskDefaultHeartbeatSeconds,
				MaxRetries:       maxRetries,
				Payload:          req.Payload,
				UpdatedAt:        nowUnix,
			},
		}
		submitted = true
		return true, nil
	})
	return submitted, err
}

// ClaimTask transitions a pending task owned by req.Executor to executing.
func (s *FileLeaseStore) ClaimTask(_ context.Context, req port.FleetTaskClaim) (bool, error) {
	claimed := false
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.TaskID)
		cur, exists := state.Tasks[key]
		if !exists || cur.Task.Target != req.Executor || cur.Task.Epoch != req.Epoch {
			return false, nil
		}
		if cur.Task.Phase != port.TaskPending {
			return false, nil
		}
		hb := req.HeartbeatSeconds
		if hb <= 0 {
			hb = port.FleetTaskDefaultHeartbeatSeconds
		}
		nowUnix := s.now().Unix()
		cur.Task.Phase = port.TaskExecuting
		cur.Task.HeartbeatAt = nowUnix
		cur.Task.HeartbeatSeconds = hb
		cur.Task.UpdatedAt = nowUnix
		state.Tasks[key] = cur
		claimed = true
		return true, nil
	})
	return claimed, err
}

// HeartbeatTask updates the executing task's heartbeat if epoch and target match.
func (s *FileLeaseStore) HeartbeatTask(_ context.Context, req port.FleetTaskHeartbeat) (bool, error) {
	ok := false
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.TaskID)
		cur, exists := state.Tasks[key]
		if !exists || cur.Task.Target != req.Executor || cur.Task.Epoch != req.Epoch || cur.Task.Phase != port.TaskExecuting {
			return false, nil
		}
		nowUnix := s.now().Unix()
		cur.Task.HeartbeatAt = nowUnix
		cur.Task.UpdatedAt = nowUnix
		state.Tasks[key] = cur
		ok = true
		return true, nil
	})
	return ok, err
}

// CompleteTask marks an executing task as done if epoch and target match.
func (s *FileLeaseStore) CompleteTask(_ context.Context, req port.FleetTaskVerdict) (bool, error) {
	return s.finishTask(req, port.TaskDone)
}

// FailTask marks an executing task as failed if epoch and target match.
func (s *FileLeaseStore) FailTask(_ context.Context, req port.FleetTaskVerdict) (bool, error) {
	return s.finishTask(req, port.TaskFailed)
}

func (s *FileLeaseStore) finishTask(req port.FleetTaskVerdict, phase port.FleetTaskPhase) (bool, error) {
	ok := false
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.TaskID)
		cur, exists := state.Tasks[key]
		if !exists || cur.Task.Target != req.Executor || cur.Task.Epoch != req.Epoch {
			return false, nil
		}
		if port.FleetTaskTerminal(cur.Task) {
			return false, nil
		}
		cur.Task.Phase = phase
		cur.Task.UpdatedAt = s.now().Unix()
		state.Tasks[key] = cur
		ok = true
		return true, nil
	})
	return ok, err
}

// ListFleetTasks returns fleet tasks filtered by table and optional target.
func (s *FileLeaseStore) ListFleetTasks(_ context.Context, req port.FleetTaskList) ([]port.FleetTask, error) {
	var out []port.FleetTask
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		for _, rec := range state.Tasks {
			if rec.Table != req.Table {
				continue
			}
			if req.Target != "" && rec.Task.Target != req.Target {
				continue
			}
			out = append(out, rec.Task)
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt < out[j].UpdatedAt
		}
		return out[i].TaskID < out[j].TaskID
	})
	return out, nil
}

// RetryTask re-dispatches a stale or failed task if retries remain.
func (s *FileLeaseStore) RetryTask(_ context.Context, req port.FleetTaskVerdict) (bool, error) {
	ok := false
	err := s.withLock(func(state *clusterStateFile) (bool, error) {
		key := recordKey(req.Table, req.TaskID)
		cur, exists := state.Tasks[key]
		if !exists || cur.Task.Epoch != req.Epoch || cur.Task.Phase == port.TaskDone {
			return false, nil
		}
		if cur.Task.Retries >= cur.Task.MaxRetries {
			return false, nil
		}
		nowUnix := s.now().Unix()
		cur.Task.Phase = port.TaskPending
		cur.Task.Retries++
		cur.Task.Epoch++
		cur.Task.HeartbeatAt = 0
		cur.Task.UpdatedAt = nowUnix
		state.Tasks[key] = cur
		ok = true
		return true, nil
	})
	return ok, err
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
