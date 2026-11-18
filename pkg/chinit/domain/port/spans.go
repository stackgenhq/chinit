// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// SpanLogQuery bounds one incremental read from ClickHouse's
// system.opentelemetry_span_log table.
type SpanLogQuery struct {
	// AfterFinishMicros selects spans with finish_time_us strictly
	// greater than this watermark.
	AfterFinishMicros uint64
	// Limit caps the number of spans returned in one pass.
	Limit int
}

// SpanLogReader reads newly flushed OpenTelemetry spans from a local
// ClickHouse server. Implementations live in infra/operator; when the
// span log table has not been materialized yet (Code 60 UNKNOWN_TABLE),
// implementations return an empty slice without error.
type SpanLogReader interface {
	ReadSpans(ctx context.Context, query SpanLogQuery) ([]model.RecordedSpan, error)
}

// SpanCursorStore persists the node-local high-water SpanCursor across
// recurring span-ship runs.
type SpanCursorStore interface {
	LoadCursor() (model.SpanCursor, bool, error)
	SaveCursor(cursor model.SpanCursor) error
}

// SpanBatch pairs recorded ClickHouse spans with the emitting node's
// cluster identity dimensions for remote OTLP export.
type SpanBatch struct {
	Cluster  string
	Role     model.Role
	Hostname string
	Cloud    string
	Region   string
	Spans    []model.RecordedSpan
}

// MaxFinishMicros returns the highest FinishTimeMicros across the batch.
func (b SpanBatch) MaxFinishMicros() uint64 {
	var maxUs uint64
	for _, s := range b.Spans {
		if s.FinishTimeMicros > maxUs {
			maxUs = s.FinishTimeMicros
		}
	}
	return maxUs
}

// FirstClickHouseVersion returns the first non-empty ClickHouse server
// version reported in the batch.
func (b SpanBatch) FirstClickHouseVersion() string {
	for _, s := range b.Spans {
		if s.ClickHouseVersion != "" {
			return s.ClickHouseVersion
		}
	}
	return ""
}

// EffectiveHostname resolves the batch's node hostname, falling back to
// the first span's recorded hostname when identity state is absent.
func (b SpanBatch) EffectiveHostname() string {
	if b.Hostname != "" {
		return b.Hostname
	}
	for _, s := range b.Spans {
		if s.Hostname != "" {
			return s.Hostname
		}
	}
	return ""
}

// SpanSink exports a batch of recorded ClickHouse spans to a remote
// tracing backend (e.g. VictoriaTraces or an OTLP collector), returning
// the number of exported spans.
type SpanSink interface {
	Export(ctx context.Context, batch SpanBatch) (int, error)
}
