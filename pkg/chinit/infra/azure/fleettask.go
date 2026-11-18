// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// fleetTaskEntity is the task row: PartitionKey "fleet-tasks", RowKey
// the task id. A separate partition from the lease rows ("readers") —
// same table, disjoint partitions, the same ETag-guarded
// read-compare-write the claims use (Azure has no conditional
// expressions on properties; the ETag is the CAS).
type fleetTaskEntity struct {
	PartitionKey     string `json:"PartitionKey"`
	RowKey           string `json:"RowKey"`
	TaskID           string `json:"task_id"`
	Type             string `json:"type"`
	Target           string `json:"target"`
	Phase            string `json:"phase"`
	Epoch            int64  `json:"epoch"`
	InstanceID       string `json:"instance_id,omitempty"`
	IP               string `json:"ip,omitempty"`
	LeaseUntil       int64  `json:"lease_until"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	Retries          int    `json:"retries"`
	MaxRetries       int    `json:"max_retries"`
	Payload          string `json:"payload,omitempty"`
	Reason           string `json:"reason,omitempty"`
	UpdatedAt        int64  `json:"updated_at"`
}

// TableFleetTaskStore implements port.FleetTaskStore over Azure Table
// Storage — the same REST surface and ETag optimistic concurrency the
// lease claimer holds. Every transition is a read (ETag) + conditional
// write (If-Match); a lost race is a 412, the benign "refused" answer.
type TableFleetTaskStore struct {
	client      httpClient
	accountName string
	TokenSource func(ctx context.Context, resource string) (string, error)

	// now is the clock seam (the sweep-staleness tests inject it).
	now func() time.Time
}

// NewTableFleetTaskStore builds the store over the same account the
// claimer uses (CHINIT_AZURE_STORAGE_ACCOUNT).
func NewTableFleetTaskStore() *TableFleetTaskStore {
	return &TableFleetTaskStore{
		client:      http.DefaultClient,
		accountName: strings.TrimSpace(os.Getenv("CHINIT_AZURE_STORAGE_ACCOUNT")),
		TokenSource: func(ctx context.Context, resource string) (string, error) {
			return managedIdentityToken(ctx, http.DefaultClient, resource)
		},
		now: time.Now,
	}
}

// WithClient injects the HTTP client (tests).
func (s *TableFleetTaskStore) WithClient(c httpClient) *TableFleetTaskStore {
	s.client = c
	return s
}

// WithNow injects the clock (the staleness tests).
func (s *TableFleetTaskStore) WithNow(f func() time.Time) *TableFleetTaskStore {
	s.now = f
	return s
}

func (s *TableFleetTaskStore) taskURL(table, taskID string) string {
	return fmt.Sprintf("https://%s.table.core.windows.net/%s(PartitionKey='fleet-tasks',RowKey='%s')",
		s.accountName, table, taskID)
}

func (s *TableFleetTaskStore) partitionURL(table string) string {
	return fmt.Sprintf("https://%s.table.core.windows.net/%s()",
		s.accountName, table)
}

// read fetches one task entity with its ETag. (false, "", nil) when the
// row is absent — the submit path's "fresh insert" answer.
func (s *TableFleetTaskStore) read(ctx context.Context, table, taskID string) (*fleetTaskEntity, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.taskURL(table, taskID), nil)
	if err != nil {
		return nil, "", false, err
	}
	token, err := s.TokenSource(ctx, "https://storage.azure.com/")
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json;odata=nometadata")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	etag := strings.Trim(resp.Header.Get("ETag"), `"`)
	switch resp.StatusCode {
	case http.StatusOK:
		var e fleetTaskEntity
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, "", false, fmt.Errorf("unmarshal fleet task %s: %w", taskID, err)
		}
		return &e, etag, true, nil
	case http.StatusNotFound:
		return nil, "", false, nil
	default:
		return nil, "", false, fmt.Errorf("read fleet task %s: status %d", taskID, resp.StatusCode)
	}
}

// write is the conditional MERGE: the If-Match ETag is the CAS. An
// empty etag inserts (If-None-Match: *). Returns refused=true on 412
// Precondition Failed — the "lost the race" answer, never an error.
func (s *TableFleetTaskStore) write(ctx context.Context, table, taskID string, etag string, entity fleetTaskEntity) (bool, bool, error) {
	body, err := json.Marshal(entity)
	if err != nil {
		return false, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.taskURL(table, taskID), bytes.NewReader(body))
	if err != nil {
		return false, false, err
	}
	token, err := s.TokenSource(ctx, "https://storage.azure.com/")
	if err != nil {
		return false, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json;odata=nometadata")
	req.Header.Set("Prefer", "return-no-content")
	if etag == "" {
		req.Header.Set("If-None-Match", "*")
	} else {
		req.Header.Set("If-Match", `"`+etag+`"`)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusCreated:
		return true, false, nil
	case http.StatusPreconditionFailed:
		return false, true, nil
	default:
		return false, false, fmt.Errorf("write fleet task %s: status %d", taskID, resp.StatusCode)
	}
}

// fleetTaskMaxRetries resolves the submit's budget: zero applies the
// default; -1 is the explicit zero-retry budget (fail on first
// staleness).
func fleetTaskMaxRetries(req int) int {
	switch {
	case req > 0:
		return req
	case req == -1:
		return 0
	default:
		return chinitport.FleetTaskDefaultMaxRetries
	}
}

// SubmitTask implements port.FleetTaskStore: insert when absent; a
// terminal row (done/failed) is replaced with a fresh epoch; a live
// duplicate is the refused "already queued" answer.
func (s *TableFleetTaskStore) SubmitTask(ctx context.Context, req chinitport.FleetTaskSubmit) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task submit requires the claims table (FleetTaskSubmit.Table is empty)")
	}
	if req.TaskID == "" || req.Target == "" {
		return false, fmt.Errorf("fleet task submit requires TaskID and Target")
	}
	existing, _, found, err := s.read(ctx, req.Table, req.TaskID)
	if err != nil {
		return false, err
	}
	if found && !chinitport.FleetTaskTerminal(chinitport.FleetTask{
		Phase: chinitport.FleetTaskPhase(existing.Phase),
	}) {
		// Live duplicate: already queued.
		return false, nil
	}
	now := s.now().Unix()
	entity := fleetTaskEntity{
		PartitionKey:     "fleet-tasks",
		RowKey:           req.TaskID,
		TaskID:           req.TaskID,
		Type:             string(req.Type),
		Target:           req.Target,
		Phase:            string(chinitport.TaskPending),
		Epoch:            1,
		LeaseUntil:       now,
		HeartbeatSeconds: chinitport.FleetTaskDefaultHeartbeatSeconds,
		MaxRetries:       fleetTaskMaxRetries(req.MaxRetries),
		Payload:          req.Payload,
		UpdatedAt:        now,
	}
	// Insert when absent; when replacing a terminal row, match its ETag
	// (the If-Match CAS: a live-phase write raced by another submit
	// loses, never double-writes).
	etag := ""
	if found {
		etag, err = s.readEtag(ctx, req.Table, req.TaskID)
		if err != nil {
			return false, err
		}
	}
	ok, refused, err := s.write(ctx, req.Table, req.TaskID, etag, entity)
	if err != nil {
		return false, err
	}
	return ok && !refused, nil
}

// readEtag fetches only the row's ETag (the replace path).
func (s *TableFleetTaskStore) readEtag(ctx context.Context, table, taskID string) (string, error) {
	_, etag, _, err := s.read(ctx, table, taskID)
	if err != nil {
		return "", err
	}
	return etag, nil
}

// ClaimTask implements port.FleetTaskStore: the target's own pending
// row moves to executing under the claimed epoch (read-compare-write
// on the ETag — a racing sweep or poll loses the CAS).
func (s *TableFleetTaskStore) ClaimTask(ctx context.Context, req chinitport.FleetTaskClaim) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task claim requires the claims table (FleetTaskClaim.Table is empty)")
	}
	row, etag, found, err := s.read(ctx, req.Table, req.TaskID)
	if err != nil || !found {
		return false, err
	}
	if row.Phase != string(chinitport.TaskPending) || row.Target != req.Executor {
		// Not mine to claim: foreign target or already claimed.
		return false, nil
	}
	heartbeat := req.HeartbeatSeconds
	if heartbeat <= 0 {
		heartbeat = chinitport.FleetTaskDefaultHeartbeatSeconds
	}
	now := s.now().Unix()
	row.Phase = string(chinitport.TaskExecuting)
	row.Epoch = req.Epoch
	row.InstanceID = req.Executor
	row.LeaseUntil = now + int64(heartbeat)
	row.HeartbeatSeconds = heartbeat
	row.UpdatedAt = now
	ok, _, err := s.write(ctx, req.Table, req.TaskID, etag, *row)
	return ok, err
}

// HeartbeatTask implements port.FleetTaskStore: only the current
// executor at the current epoch extends the lease (the epoch check is
// the fence — a re-dispatched row's old executor is refused and must
// abort).
func (s *TableFleetTaskStore) HeartbeatTask(ctx context.Context, req chinitport.FleetTaskHeartbeat) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task heartbeat requires the claims table (FleetTaskHeartbeat.Table is empty)")
	}
	row, etag, found, err := s.read(ctx, req.Table, req.TaskID)
	if err != nil || !found {
		return false, err
	}
	if row.Phase != string(chinitport.TaskExecuting) || row.Target != req.Executor || row.Epoch != req.Epoch {
		return false, nil
	}
	now := s.now().Unix()
	row.LeaseUntil = now + int64(row.HeartbeatSeconds)
	row.UpdatedAt = now
	ok, _, err := s.write(ctx, req.Table, req.TaskID, etag, *row)
	return ok, err
}

// terminalWrite is the shared verdict path: executing/ack → the
// terminal phase, epoch-fenced.
func (s *TableFleetTaskStore) terminalWrite(ctx context.Context, req chinitport.FleetTaskVerdict, phase chinitport.FleetTaskPhase) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task verdict requires the claims table (FleetTaskVerdict.Table is empty)")
	}
	row, etag, found, err := s.read(ctx, req.Table, req.TaskID)
	if err != nil || !found {
		return false, err
	}
	if (row.Phase != string(chinitport.TaskExecuting) && row.Phase != string(chinitport.TaskAck)) ||
		row.Target != req.Executor || row.Epoch != req.Epoch {
		return false, nil
	}
	row.Phase = string(phase)
	row.Reason = req.Reason
	row.UpdatedAt = s.now().Unix()
	ok, _, err := s.write(ctx, req.Table, req.TaskID, etag, *row)
	return ok, err
}

// CompleteTask implements port.FleetTaskStore.
func (s *TableFleetTaskStore) CompleteTask(ctx context.Context, req chinitport.FleetTaskVerdict) (bool, error) {
	return s.terminalWrite(ctx, req, chinitport.TaskDone)
}

// FailTask implements port.FleetTaskStore.
func (s *TableFleetTaskStore) FailTask(ctx context.Context, req chinitport.FleetTaskVerdict) (bool, error) {
	return s.terminalWrite(ctx, req, chinitport.TaskFailed)
}

// RetryTask implements port.FleetTaskStore: the sweep's re-dispatch —
// epoch+1 (the fence), retries+1 (the budget), back to pending.
func (s *TableFleetTaskStore) RetryTask(ctx context.Context, req chinitport.FleetTaskVerdict) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task retry requires the claims table (FleetTaskVerdict.Table is empty)")
	}
	row, etag, found, err := s.read(ctx, req.Table, req.TaskID)
	if err != nil || !found {
		return false, err
	}
	if row.Phase == string(chinitport.TaskDone) {
		return false, nil
	}
	if row.Retries >= row.MaxRetries {
		// Budget spent: terminal failed is the caller's answer.
		return false, nil
	}
	now := s.now().Unix()
	row.Phase = string(chinitport.TaskPending)
	row.Epoch++
	row.Retries++
	row.InstanceID = ""
	row.LeaseUntil = now
	row.UpdatedAt = now
	ok, _, err := s.write(ctx, req.Table, req.TaskID, etag, *row)
	return ok, err
}

// ListFleetTasks implements port.FleetTaskStore: one partition query
// (the fleet-tasks partition), Target filter code-side.
func (s *TableFleetTaskStore) ListFleetTasks(ctx context.Context, req chinitport.FleetTaskList) ([]chinitport.FleetTask, error) {
	if req.Table == "" {
		return nil, fmt.Errorf("fleet task list requires the claims table (FleetTaskList.Table is empty)")
	}
	url := s.partitionURL(req.Table) + "?$filter=PartitionKey%20eq%20'fleet-tasks'"
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	token, err := s.TokenSource(ctx, "https://storage.azure.com/")
	if err != nil {
		return nil, err
	}
	listReq.Header.Set("Authorization", "Bearer "+token)
	listReq.Header.Set("Accept", "application/json;odata=nometadata")
	resp, err := s.client.Do(listReq)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list fleet tasks in %s: status %d", req.Table, resp.StatusCode)
	}
	var list struct {
		Value []fleetTaskEntity `json:"value"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("unmarshal fleet task list in %s: %w", req.Table, err)
	}
	var tasks []chinitport.FleetTask
	for _, e := range list.Value {
		if req.Target != "" && e.Target != req.Target {
			continue
		}
		tasks = append(tasks, chinitport.FleetTask{
			TaskID:           e.TaskID,
			Type:             chinitport.FleetTaskType(e.Type),
			Target:           e.Target,
			Phase:            chinitport.FleetTaskPhase(e.Phase),
			Epoch:            e.Epoch,
			HeartbeatAt:      e.LeaseUntil - int64(e.HeartbeatSeconds),
			HeartbeatSeconds: e.HeartbeatSeconds,
			Retries:          e.Retries,
			MaxRetries:       e.MaxRetries,
			Payload:          e.Payload,
			UpdatedAt:        e.UpdatedAt,
		})
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].UpdatedAt < tasks[j].UpdatedAt })
	return tasks, nil
}

// Compile-time capability check.
var _ chinitport.FleetTaskStore = (*TableFleetTaskStore)(nil)
