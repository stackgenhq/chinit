// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// OTLPSpanSinkConfig configures OTLPSpanSink's OTLP/HTTP destination.
type OTLPSpanSinkConfig struct {
	EndpointURL string
	Headers     string
	// Exporter is an optional test seam; nil builds an otlptracehttp
	// exporter targeting EndpointURL.
	Exporter sdktrace.SpanExporter
}

// OTLPSpanSink implements port.SpanSink over the OpenTelemetry Go SDK's
// otlptracehttp exporter.
type OTLPSpanSink struct {
	endpointURL string
	headers     map[string]string
	exporter    sdktrace.SpanExporter
}

// NewOTLPSpanSink constructs an OTLPSpanSink for cfg.
func NewOTLPSpanSink(cfg OTLPSpanSinkConfig) *OTLPSpanSink {
	return &OTLPSpanSink{
		endpointURL: cfg.EndpointURL,
		headers:     parseHeaders(cfg.Headers),
		exporter:    cfg.Exporter,
	}
}

// Export converts batch into SDK ReadOnlySpans and pushes them via
// otlptracehttp, returning the count of exported spans.
func (s *OTLPSpanSink) Export(ctx context.Context, batch port.SpanBatch) (int, error) {
	spans := s.BuildReadOnlySpans(batch)
	if len(spans) == 0 {
		return 0, nil
	}
	exp := s.exporter
	if exp == nil {
		otelExp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(s.endpointURL),
			otlptracehttp.WithHeaders(s.headers),
		)
		if err != nil {
			return 0, fmt.Errorf("create otlptracehttp exporter: %w", err)
		}
		defer func() { _ = otelExp.Shutdown(ctx) }()
		exp = otelExp
	}
	if err := exp.ExportSpans(ctx, spans); err != nil {
		return 0, err
	}
	return len(spans), nil
}

// BuildReadOnlySpans maps a domain SpanBatch into OpenTelemetry SDK
// ReadOnlySpan records preserving TraceID, SpanID, ParentSpanID, and
// microsecond timestamps.
func (s *OTLPSpanSink) BuildReadOnlySpans(batch port.SpanBatch) []sdktrace.ReadOnlySpan {
	chVersion := batch.FirstClickHouseVersion()
	res := s.buildResource(batch, chVersion)
	scope := instrumentation.Scope{
		Name:    "clickhouse.opentelemetry_span_log",
		Version: chVersion,
	}

	out := make([]sdktrace.ReadOnlySpan, 0, len(batch.Spans))
	for _, rec := range batch.Spans {
		if ro, ok := s.buildSpan(rec, res, scope); ok {
			out = append(out, ro)
		}
	}
	return out
}

func (s *OTLPSpanSink) buildResource(batch port.SpanBatch, chVersion string) *otelresource.Resource {
	return otelresource.NewSchemaless(
		semconv.ServiceName("clickhouse"),
		semconv.ServiceVersion(chVersion),
		attribute.String("host.name", batch.EffectiveHostname()),
		attribute.String("chinit.cluster", batch.Cluster),
		attribute.String("chinit.role", string(batch.Role)),
		attribute.String("cloud.provider", batch.Cloud),
		attribute.String("cloud.region", batch.Region),
	)
}

func (s *OTLPSpanSink) buildSpan(rec model.RecordedSpan, res *otelresource.Resource, scope instrumentation.Scope) (sdktrace.ReadOnlySpan, bool) {
	parsedUUID, err := uuid.Parse(strings.TrimSpace(rec.TraceID))
	if err != nil || parsedUUID == uuid.Nil || rec.SpanID == 0 {
		return nil, false
	}
	traceID := oteltrace.TraceID(parsedUUID)

	var spanID oteltrace.SpanID
	binary.BigEndian.PutUint64(spanID[:], rec.SpanID)

	var parentCtx oteltrace.SpanContext
	if rec.ParentSpanID != 0 {
		var parentID oteltrace.SpanID
		binary.BigEndian.PutUint64(parentID[:], rec.ParentSpanID)
		parentCtx = oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     parentID,
			TraceFlags: oteltrace.FlagsSampled,
		})
	}

	keys := make([]string, 0, len(rec.Attributes))
	for k := range rec.Attributes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	attrs := make([]attribute.KeyValue, 0, len(keys)+1)
	for _, k := range keys {
		attrs = append(attrs, attribute.String(k, rec.Attributes[k]))
	}
	if rec.Hostname != "" {
		attrs = append(attrs, attribute.String("clickhouse.hostname", rec.Hostname))
	}

	return tracetest.SpanStub{
		Name: rec.EffectiveOperationName(),
		SpanContext: oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
			TraceID:    traceID,
			SpanID:     spanID,
			TraceFlags: oteltrace.FlagsSampled,
		}),
		Parent:               parentCtx,
		SpanKind:             s.spanKind(rec.Kind),
		StartTime:            time.UnixMicro(int64(rec.StartTimeMicros)),
		EndTime:              time.UnixMicro(int64(rec.NormalizedFinishMicros())),
		Attributes:           attrs,
		Status:               sdktrace.Status{Code: s.statusCode(rec.StatusCode), Description: rec.StatusMessage},
		Resource:             res,
		InstrumentationScope: scope,
	}.Snapshot(), true
}

func (s *OTLPSpanSink) spanKind(raw string) oteltrace.SpanKind {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "SERVER", "1":
		return oteltrace.SpanKindServer
	case "CLIENT", "2":
		return oteltrace.SpanKindClient
	case "PRODUCER", "3":
		return oteltrace.SpanKindProducer
	case "CONSUMER", "4":
		return oteltrace.SpanKindConsumer
	default:
		return oteltrace.SpanKindInternal
	}
}

func (s *OTLPSpanSink) statusCode(raw string) codes.Code {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "OK", "1":
		return codes.Ok
	case "ERROR", "2":
		return codes.Error
	default:
		return codes.Unset
	}
}
