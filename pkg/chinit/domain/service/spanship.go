// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

const (
	defaultSpanBatchLimit      = 5000
	defaultSpanInitialLookback = 5 * time.Minute
)

// JSONSpanCursorStore implements port.SpanCursorStore over an atomic
// JSON file on the local filesystem.
type JSONSpanCursorStore struct {
	path string
}

// NewJSONSpanCursorStore constructs a JSON-backed SpanCursorStore at path.
func NewJSONSpanCursorStore(path string) *JSONSpanCursorStore {
	return &JSONSpanCursorStore{path: path}
}

// LoadCursor reads the persisted SpanCursor, returning ok=false when no
// cursor file exists yet.
func (s *JSONSpanCursorStore) LoadCursor() (model.SpanCursor, bool, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return model.SpanCursor{}, false, nil
	}
	if err != nil {
		return model.SpanCursor{}, false, err
	}
	var cursor model.SpanCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return model.SpanCursor{}, false, err
	}
	return cursor, cursor.FinishTimeMicros > 0, nil
}

// SaveCursor atomically writes cursor as structured JSON.
func (s *JSONSpanCursorStore) SaveCursor(cursor model.SpanCursor) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SpanShipperDeps bundles the ports and policy bounds for SpanShipper.
type SpanShipperDeps struct {
	Reader          port.SpanLogReader
	Cursors         port.SpanCursorStore
	Sink            port.SpanSink
	Clock           Clock
	BatchLimit      int
	InitialLookback time.Duration
}

// SpanShipper is the domain service that incrementally reads finished
// spans from ClickHouse's system.opentelemetry_span_log, exports them
// through port.SpanSink, and advances the persisted SpanCursor only
// after export succeeds (at-least-once delivery).
type SpanShipper struct {
	reader          port.SpanLogReader
	cursors         port.SpanCursorStore
	sink            port.SpanSink
	clock           Clock
	batchLimit      int
	initialLookback time.Duration
}

// NewSpanShipper constructs a SpanShipper with defaults for zero bounds.
func NewSpanShipper(deps SpanShipperDeps) *SpanShipper {
	clock := deps.Clock
	if clock == nil {
		clock = NewWallClock()
	}
	limit := deps.BatchLimit
	if limit <= 0 {
		limit = defaultSpanBatchLimit
	}
	lookback := deps.InitialLookback
	if lookback <= 0 {
		lookback = defaultSpanInitialLookback
	}
	return &SpanShipper{
		reader:          deps.Reader,
		cursors:         deps.Cursors,
		sink:            deps.Sink,
		clock:           clock,
		batchLimit:      limit,
		initialLookback: lookback,
	}
}

// SpanShipRequest carries the node identity dimensions for one pass.
type SpanShipRequest struct {
	Cluster  string
	Role     model.Role
	Hostname string
	Cloud    string
	Region   string
}

// SpanShipResult reports the outcome of one SpanShipper.Ship pass.
type SpanShipResult struct {
	Exported        int
	HighWaterMicros uint64
	SkippedReason   string
}

// Ship executes one incremental span-forwarding pass.
func (s *SpanShipper) Ship(ctx context.Context, req SpanShipRequest) (SpanShipResult, error) {
	if req.Role == model.RoleKeeper {
		return SpanShipResult{SkippedReason: "keeper role has no server span log"}, nil
	}
	cursor := s.resolveStartingCursor()
	spans, err := s.reader.ReadSpans(ctx, port.SpanLogQuery{
		AfterFinishMicros: cursor.FinishTimeMicros,
		Limit:             s.batchLimit,
	})
	if err != nil {
		return SpanShipResult{}, fmt.Errorf("read span log: %w", err)
	}
	if len(spans) == 0 {
		return SpanShipResult{HighWaterMicros: cursor.FinishTimeMicros}, nil
	}

	batch := port.SpanBatch{
		Cluster:  req.Cluster,
		Role:     req.Role,
		Hostname: req.Hostname,
		Cloud:    req.Cloud,
		Region:   req.Region,
		Spans:    spans,
	}
	exported, err := s.sink.Export(ctx, batch)
	if err != nil {
		return SpanShipResult{}, fmt.Errorf("export spans: %w", err)
	}

	next := cursor.Advance(batch.MaxFinishMicros())
	if s.cursors != nil && next.FinishTimeMicros > cursor.FinishTimeMicros {
		if err := s.cursors.SaveCursor(next); err != nil {
			return SpanShipResult{Exported: exported, HighWaterMicros: next.FinishTimeMicros}, fmt.Errorf("persist span cursor: %w", err)
		}
	}
	return SpanShipResult{
		Exported:        exported,
		HighWaterMicros: next.FinishTimeMicros,
	}, nil
}

func (s *SpanShipper) resolveStartingCursor() model.SpanCursor {
	if s.cursors != nil {
		if stored, ok, err := s.cursors.LoadCursor(); err == nil && ok {
			return stored
		}
	}
	startAt := s.clock.Now().Add(-s.initialLookback)
	if startAt.UnixMicro() <= 0 {
		return model.SpanCursor{}
	}
	return model.SpanCursor{FinishTimeMicros: uint64(startAt.UnixMicro())}
}
