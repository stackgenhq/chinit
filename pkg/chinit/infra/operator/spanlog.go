// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/model/clickhouse"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// SpanLogStoreConfig configures SpanLogStore's local ClickHouse endpoint.
type SpanLogStoreConfig struct {
	Address     string
	HTTPPort    int
	Secure      bool
	Credentials domain.Credentials
}

// SpanLogStore implements port.SpanLogReader over clickhouse-operator's
// pooled EndpointConnectionParams / GetPooledDBConnection machinery (the
// same connection pool MetricsFetcher uses).
type SpanLogStore struct {
	endpointParams *clickhouse.EndpointConnectionParams
}

// NewSpanLogStore constructs a SpanLogStore targeting cfg.Address
// (defaulting to 127.0.0.1:8123 when omitted).
func NewSpanLogStore(cfg SpanLogStoreConfig) *SpanLogStore {
	addr := strings.TrimSpace(cfg.Address)
	if addr == "" {
		addr = "127.0.0.1"
	}
	portNum := cfg.HTTPPort
	if portNum == 0 {
		portNum = 8123
	}
	scheme := v1.ChSchemeHTTP
	if cfg.Secure {
		scheme = v1.ChSchemeHTTPS
	}
	user := cfg.Credentials.Username
	if user == "" {
		if cfg.Credentials.Password != "" {
			user = "admin"
		} else {
			user = "default"
		}
	}
	return &SpanLogStore{
		endpointParams: clickhouse.NewEndpointConnectionParams(scheme, addr, user, cfg.Credentials.Password, "", portNum),
	}
}

// ReadSpans queries newly flushed spans strictly newer than
// query.AfterFinishMicros, returning an empty slice when the table has
// not been materialized yet (Code 60 UNKNOWN_TABLE).
func (s *SpanLogStore) ReadSpans(ctx context.Context, query port.SpanLogQuery) ([]model.RecordedSpan, error) {
	if query.Limit <= 0 {
		return nil, nil
	}
	conn := clickhouse.GetPooledDBConnection(s.endpointParams)
	res, err := conn.QueryContext(ctx, s.buildQuery(query))
	if err != nil {
		if s.isMissingTableError(err) {
			return nil, nil
		}
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	defer res.Close()
	if res.Rows == nil {
		return nil, nil
	}
	return s.scanRows(res, query.Limit)
}

func (s *SpanLogStore) buildQuery(query port.SpanLogQuery) string {
	return fmt.Sprintf(`SELECT
    hostname,
    version(),
    toString(trace_id),
    span_id,
    parent_span_id,
    operation_name,
    toString(kind),
    start_time_us,
    finish_time_us,
    toString(status_code),
    status_message,
    toJSONString(attribute)
FROM system.opentelemetry_span_log
WHERE finish_date >= today() - 1
  AND finish_time_us > %d
ORDER BY finish_time_us ASC, span_id ASC
LIMIT %d`, query.AfterFinishMicros, query.Limit)
}

func (s *SpanLogStore) isMissingTableError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNKNOWN_TABLE") || strings.Contains(msg, "Code: 60")
}

func (s *SpanLogStore) scanRows(res *clickhouse.QueryResult, capHint int) ([]model.RecordedSpan, error) {
	spans := make([]model.RecordedSpan, 0, min(capHint, 256))
	for res.Rows.Next() {
		var (
			rec     model.RecordedSpan
			attrRaw string
		)
		if err := res.Rows.Scan(
			&rec.Hostname,
			&rec.ClickHouseVersion,
			&rec.TraceID,
			&rec.SpanID,
			&rec.ParentSpanID,
			&rec.OperationName,
			&rec.Kind,
			&rec.StartTimeMicros,
			&rec.FinishTimeMicros,
			&rec.StatusCode,
			&rec.StatusMessage,
			&attrRaw,
		); err != nil {
			return nil, err
		}
		if attrRaw != "" {
			_ = json.Unmarshal([]byte(attrRaw), &rec.Attributes)
		}
		spans = append(spans, rec)
	}
	if err := res.Rows.Err(); err != nil {
		return nil, err
	}
	return spans, nil
}
