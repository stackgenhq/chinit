// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
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

// statusEntity is the status row: PartitionKey "node-status", RowKey
// the hostname — the same table the claims ("readers" partition) and
// the fleet tasks ("fleet-tasks" partition) use, one liveness view.
type statusEntity struct {
	PartitionKey      string `json:"PartitionKey"`
	RowKey            string `json:"RowKey"`
	Role              string `json:"role"`
	InstanceID        string `json:"instance_id"`
	SpecHash          string `json:"spec_hash"`
	ClickHouseVersion string `json:"ch_version"`
	Health            string `json:"health"`
	KeeperQuorum      bool   `json:"keeper_quorum"`
	Cordoned          bool   `json:"cordoned"`
	UpdatedAt         int64  `json:"updated_at"`
	LeaseUntil        int64  `json:"lease_until"`
}

// TableStatusPublisher implements port.StatusPublisher over Azure
// Table Storage — the same REST surface the task store holds.
type TableStatusPublisher struct {
	client      httpClient
	accountName string
	TokenSource func(ctx context.Context, resource string) (string, error)
	now         func() time.Time
}

// NewTableStatusPublisher builds the publisher over the same account
// the claimer uses.
func NewTableStatusPublisher() *TableStatusPublisher {
	return &TableStatusPublisher{
		client:      http.DefaultClient,
		accountName: strings.TrimSpace(os.Getenv("CHINIT_AZURE_STORAGE_ACCOUNT")),
		TokenSource: func(ctx context.Context, resource string) (string, error) {
			return managedIdentityToken(ctx, http.DefaultClient, resource)
		},
		now: time.Now,
	}
}

// WithClient injects the HTTP client (tests).
func (s *TableStatusPublisher) WithClient(c httpClient) *TableStatusPublisher {
	s.client = c
	return s
}

func (s *TableStatusPublisher) statusURL(table, hostname string) string {
	return fmt.Sprintf("https://%s.table.core.windows.net/%s(PartitionKey='node-status',RowKey='%s')",
		s.accountName, table, hostname)
}

func (s *TableStatusPublisher) statusPartitionURL(table string) string {
	return fmt.Sprintf("https://%s.table.core.windows.net/%s()?$filter=PartitionKey%%20eq%%20'node-status'",
		s.accountName, table)
}

// readStatus fetches one entity with its ETag. (nil, "", false, nil)
// when absent.
func (s *TableStatusPublisher) readStatus(ctx context.Context, table, hostname string) (*statusEntity, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.statusURL(table, hostname), nil)
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
		var e statusEntity
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, "", false, fmt.Errorf("unmarshal status %s: %w", hostname, err)
		}
		return &e, etag, true, nil
	case http.StatusNotFound:
		return nil, "", false, nil
	default:
		return nil, "", false, fmt.Errorf("read status %s: status %d", hostname, resp.StatusCode)
	}
}

// writeStatus is the conditional MERGE (If-Match ETag; empty etag
// inserts with If-None-Match). refused answers the lost write race.
func (s *TableStatusPublisher) writeStatus(ctx context.Context, table, hostname, etag string, entity statusEntity) (refused bool, err error) {
	body, err := json.Marshal(entity)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.statusURL(table, hostname), strings.NewReader(string(body)))
	if err != nil {
		return false, err
	}
	token, err := s.TokenSource(ctx, "https://storage.azure.com/")
	if err != nil {
		return false, err
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
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusCreated:
		return false, nil
	case http.StatusPreconditionFailed:
		return true, nil
	default:
		return false, fmt.Errorf("write status %s: status %d", hostname, resp.StatusCode)
	}
}

// PublishStatus implements port.StatusPublisher: read-compare-write —
// an absent row inserts; a row this instance owns refreshes in place;
// a foreign LIVE row is the identity-moved refusal (never a wedge).
func (s *TableStatusPublisher) PublishStatus(ctx context.Context, req chinitport.NodeStatusPublish) error {
	if req.Table == "" {
		return fmt.Errorf("status publish requires the claims table (NodeStatusPublish.Table is empty)")
	}
	if req.Status.Hostname == "" {
		return fmt.Errorf("status publish requires Hostname")
	}
	existing, etag, found, err := s.readStatus(ctx, req.Table, req.Status.Hostname)
	if err != nil {
		return err
	}
	now := s.now().Unix()
	if found && existing.InstanceID != req.Status.InstanceID && existing.LeaseUntil >= now {
		return fmt.Errorf("status publish: another instance holds the %s status row (identity moved?)", req.Status.Hostname)
	}
	entity := statusEntity{
		PartitionKey:      "node-status",
		RowKey:            req.Status.Hostname,
		Role:              req.Status.Role,
		InstanceID:        req.Status.InstanceID,
		SpecHash:          req.Status.SpecHash,
		ClickHouseVersion: req.Status.ClickHouseVersion,
		Health:            string(req.Status.Health),
		KeeperQuorum:      req.Status.KeeperQuorum,
		Cordoned:          req.Status.Cordoned,
		UpdatedAt:         now,
		LeaseUntil:        now + 900,
	}
	// Preserve the cordoned flag a cordon set between read and write:
	// the publish is the node's own fields; the toggle owns the flag.
	if found && existing.Cordoned {
		entity.Cordoned = true
	}
	writeEtag := ""
	if found {
		writeEtag = etag
	}
	refused, err := s.writeStatus(ctx, req.Table, req.Status.Hostname, writeEtag, entity)
	if err != nil {
		return err
	}
	if refused {
		return fmt.Errorf("status publish %s: lost the write race (retry on the next tick)", req.Status.Hostname)
	}
	return nil
}

// ListStatus implements port.StatusPublisher: the partition query.
func (s *TableStatusPublisher) ListStatus(ctx context.Context, req chinitport.NodeStatusList) ([]chinitport.NodeStatus, error) {
	if req.Table == "" {
		return nil, fmt.Errorf("status list requires the claims table (NodeStatusList.Table is empty)")
	}
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.statusPartitionURL(req.Table), nil)
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
		return nil, fmt.Errorf("list status rows in %s: status %d", req.Table, resp.StatusCode)
	}
	var list struct {
		Value []statusEntity `json:"value"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("unmarshal status list in %s: %w", req.Table, err)
	}
	now := s.now().Unix()
	var statuses []chinitport.NodeStatus
	for _, e := range list.Value {
		if e.LeaseUntil <= now {
			continue // aged out with its node's lease
		}
		if req.Hostname != "" && e.RowKey != req.Hostname {
			continue
		}
		statuses = append(statuses, chinitport.NodeStatus{
			Hostname:          e.RowKey,
			Role:              e.Role,
			InstanceID:        e.InstanceID,
			SpecHash:          e.SpecHash,
			ClickHouseVersion: e.ClickHouseVersion,
			Health:            chinitport.NodeHealth(e.Health),
			KeeperQuorum:      e.KeeperQuorum,
			Cordoned:          e.Cordoned,
			UpdatedAt:         e.UpdatedAt,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Hostname < statuses[j].Hostname })
	return statuses, nil
}

// SetCordon implements port.StatusPublisher: read-modify-write on the
// ETag, preserving the node's own fields (the toggle owns the flag
// only). Absent row is an error naming the node.
func (s *TableStatusPublisher) SetCordon(ctx context.Context, req chinitport.NodeCordonRequest) error {
	if req.Table == "" {
		return fmt.Errorf("cordon requires the claims table (NodeCordonRequest.Table is empty)")
	}
	if req.Hostname == "" {
		return fmt.Errorf("cordon requires Hostname")
	}
	row, etag, found, err := s.readStatus(ctx, req.Table, req.Hostname)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("cordon %s: no status row (the node has never published — wait for its first lease-renew tick)", req.Hostname)
	}
	row.Cordoned = req.Cordoned
	row.UpdatedAt = s.now().Unix()
	refused, err := s.writeStatus(ctx, req.Table, req.Hostname, etag, *row)
	if err != nil {
		return err
	}
	if refused {
		return fmt.Errorf("cordon %s: lost the write race (retry)", req.Hostname)
	}
	return nil
}

// Compile-time capability check.
var _ chinitport.StatusPublisher = (*TableStatusPublisher)(nil)
