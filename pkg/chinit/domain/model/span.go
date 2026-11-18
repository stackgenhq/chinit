// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import "strings"

// RecordedSpan is a pure domain value object representing one finished
// OpenTelemetry trace span persisted in ClickHouse's
// system.opentelemetry_span_log table.
type RecordedSpan struct {
	Hostname          string
	ClickHouseVersion string
	TraceID           string
	SpanID            uint64
	ParentSpanID      uint64
	OperationName     string
	Kind              string
	StartTimeMicros   uint64
	FinishTimeMicros  uint64
	StatusCode        string
	StatusMessage     string
	Attributes        map[string]string
}

// EffectiveOperationName returns OperationName or a non-empty fallback
// when ClickHouse recorded an unnamed span.
func (s RecordedSpan) EffectiveOperationName() string {
	if name := strings.TrimSpace(s.OperationName); name != "" {
		return name
	}
	return "clickhouse.span"
}

// NormalizedFinishMicros clamps FinishTimeMicros to at least
// StartTimeMicros so skewed clocks never produce negative span durations.
func (s RecordedSpan) NormalizedFinishMicros() uint64 {
	if s.FinishTimeMicros < s.StartTimeMicros {
		return s.StartTimeMicros
	}
	return s.FinishTimeMicros
}

// SpanCursor is the persisted high-water mark (microseconds since Unix
// epoch) tracking the most recent span exported from a node.
type SpanCursor struct {
	FinishTimeMicros uint64 `json:"finish_time_us"`
}

// Advance returns a new SpanCursor moved forward to candidate when
// candidate exceeds the current watermark.
func (c SpanCursor) Advance(candidate uint64) SpanCursor {
	if candidate > c.FinishTimeMicros {
		return SpanCursor{FinishTimeMicros: candidate}
	}
	return c
}
