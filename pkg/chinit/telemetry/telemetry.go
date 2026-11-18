// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// flushTimeout bounds Shutdown's ForceFlush — a wedged collector must
// never wedge the node's jobs on THEIR way out.
const flushTimeout = 5 * time.Second

// Config is the init request (the two-parameter style: one struct,
// no growing positional lists).
type Config struct {
	// Endpoint is the base OTLP/HTTP target — the spec's
	// log_shipping.endpoint. Empty disables telemetry unless
	// MetricsEndpoint or TracesEndpoint is explicitly set.
	Endpoint string
	// MetricsEndpoint overrides Endpoint for OTLP metrics (e.g.
	// VictoriaMetrics "/opentelemetry/v1/metrics"). Prometheus Remote
	// Write URLs ("/api/v1/write") are scraped/forwarded by otelcol.
	MetricsEndpoint string
	// TracesEndpoint overrides Endpoint for OTLP traces (e.g.
	// VictoriaTraces "/insert/opentelemetry/v1/traces").
	TracesEndpoint string
	// Headers are comma-separated k=v pairs from log_shipping.headers
	// (bearer tokens for collector front doors that need them).
	Headers string
	// MetricsHeaders / TracesHeaders optionally override Headers per signal.
	MetricsHeaders string
	TracesHeaders  string
	// ServiceVersion is the chinit build (app.Version).
	ServiceVersion string
	// InstanceID is the emitting process's identity — the settled
	// hostname (subcommands) or the provider instance id (boot, before
	// identity settles).
	InstanceID string
	// Cluster is the ClickHouse cluster name (identity.ClusterName).
	Cluster string
	// Role is the node role ("writer"/"reader"/"keeper"/"backup"; the
	// boot's spec may be empty when identity is not settled yet).
	Role string
	// Cloud is the provider name ("aws"/"azure").
	Cloud string
	// Region is the provider region.
	Region string
}

// instruments is the process's metric set (operator-style: one struct
// built once at Init, every emit reads from it).
type instruments struct {
	jobRuns     otelmetric.Int64Counter
	jobDuration otelmetric.Float64Histogram
	bootStep    otelmetric.Float64Histogram
	bootOutcome otelmetric.Int64Counter
	sweepRows   otelmetric.Int64Gauge
	sweepStale  otelmetric.Int64Counter
	sweepFailed otelmetric.Int64Counter
	taskClaimed otelmetric.Int64Counter
	taskDone    otelmetric.Int64Counter
	taskFailed  otelmetric.Int64Counter
	keeperUp    otelmetric.Float64Gauge
	diskFree    otelmetric.Float64Gauge
	diskGrew    otelmetric.Int64Counter
	backupOK    otelmetric.Float64Gauge
}

var (
	mu             sync.Mutex
	provider       *metric.MeterProvider
	meter          otelmetric.Meter
	instr          *instruments
	tracerProvider *sdktrace.TracerProvider
	tracer         oteltrace.Tracer
	baseAttrs      []attribute.KeyValue
	initialized    bool
)

func (cfg Config) logShipping() model.LogShipping {
	return model.LogShipping{
		Endpoint:        cfg.Endpoint,
		MetricsEndpoint: cfg.MetricsEndpoint,
		TracesEndpoint:  cfg.TracesEndpoint,
		Headers:         cfg.Headers,
		MetricsHeaders:  cfg.MetricsHeaders,
		TracesHeaders:   cfg.TracesHeaders,
	}
}

// Init installs the process-wide metric and/or trace providers.
// Idempotent: a second call (tests, re-entry) is a no-op while the
// first provider lives. When neither an OTLP metrics endpoint nor a
// traces endpoint resolves, the package stays in its disabled zero
// state — Init returns nil and every emit no-ops.
func Init(ctx context.Context, cfg Config) error {
	mu.Lock()
	defer mu.Unlock()
	if initialized {
		return nil
	}
	ls := cfg.logShipping()
	var reader metric.Reader
	if metricsURL := ls.EffectiveMetricsEndpoint(); metricsURL != "" && !ls.IsPrometheusRemoteWrite() {
		exporter, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL(metricsURL),
			otlpmetrichttp.WithHeaders(parseHeaders(ls.EffectiveMetricsHeaders())),
		)
		if err != nil {
			return fmt.Errorf("telemetry metric exporter: %w", err)
		}
		// Oneshot discipline: the interval is a wedged-process backstop
		// only — Shutdown's ForceFlush is the real delivery path.
		reader = metric.NewPeriodicReader(exporter, metric.WithInterval(time.Hour))
	} else if ls.IsPrometheusRemoteWrite() && ls.Agent != "fluentbit" {
		// When metrics_endpoint is Prometheus Remote Write (e.g.
		// VictoriaMetrics /api/v1/write), push chinit's OTLP job/boot
		// metrics to the local otelcol OTLP receiver on 127.0.0.1:4318
		// so otelcol forwards them through prometheusremotewrite.
		exporter, err := otlpmetrichttp.New(ctx,
			otlpmetrichttp.WithEndpointURL("http://127.0.0.1:4318/v1/metrics"),
		)
		if err == nil {
			reader = metric.NewPeriodicReader(exporter, metric.WithInterval(time.Hour))
		}
	}

	var spanExp sdktrace.SpanExporter
	if tracesURL := ls.EffectiveTracesEndpoint(); tracesURL != "" {
		exp, err := otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(tracesURL),
			otlptracehttp.WithHeaders(parseHeaders(ls.EffectiveTracesHeaders())),
		)
		if err != nil {
			return fmt.Errorf("telemetry trace exporter: %w", err)
		}
		spanExp = exp
	}
	if reader == nil && spanExp == nil {
		return nil // telemetry disabled — emits no-op
	}
	return initProvidersLocked(cfg, reader, spanExp)
}

// InitWithReader installs the provider over a caller-supplied reader
// instead of the OTLP exporter (the specs' collection seam — a
// sdk/metric/manual.Reader collects the datapoints in-process; the
// production path never calls it). Same config, same instruments, same
// disabled-when-empty rule.
func InitWithReader(cfg Config, reader metric.Reader) error {
	return InitWithProviders(cfg, reader, nil)
}

// InitWithProviders installs caller-supplied metric reader and/or span
// exporter (the specs' collection seam for both metrics and traces).
func InitWithProviders(cfg Config, reader metric.Reader, spanExp sdktrace.SpanExporter) error {
	mu.Lock()
	defer mu.Unlock()
	if initialized {
		return nil
	}
	if reader == nil && spanExp == nil {
		return nil // telemetry disabled — emits no-op
	}
	return initProvidersLocked(cfg, reader, spanExp)
}

// initProvidersLocked assembles the meter and/or tracer providers over
// a shared OTel Resource.
func initProvidersLocked(cfg Config, reader metric.Reader, spanExp sdktrace.SpanExporter) error {
	res, err := otelresource.Merge(
		otelresource.Default(),
		otelresource.NewSchemaless(
			semconv.ServiceName("chinit"),
			semconv.ServiceVersion(cfg.ServiceVersion),
			semconv.ServiceInstanceID(cfg.InstanceID),
			attribute.String("chinit.cluster", cfg.Cluster),
			attribute.String("chinit.cloud", cfg.Cloud),
			attribute.String("chinit.region", cfg.Region),
		),
	)
	if err != nil {
		return fmt.Errorf("telemetry resource: %w", err)
	}
	if reader != nil {
		provider = metric.NewMeterProvider(
			metric.WithResource(res),
			metric.WithReader(reader),
		)
		meter = provider.Meter("chinit")
		var err2 error
		instr, err2 = buildInstruments(meter)
		if err2 != nil {
			provider = nil
			meter = nil
			return fmt.Errorf("telemetry instruments: %w", err2)
		}
	}
	if spanExp != nil {
		tracerProvider = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(spanExp),
		)
		tracer = tracerProvider.Tracer("chinit")
	}
	baseAttrs = []attribute.KeyValue{
		attribute.String("chinit.cluster", cfg.Cluster),
		attribute.String("chinit.role", cfg.Role),
	}
	initialized = true
	return nil
}

// parseHeaders turns the spec's comma-separated "k=v,k2=v2" header
// string into the exporter's map (whitespace-tolerant; malformed
// pairs are dropped, not fatal — the same best-effort rule as the
// fluentbit output name).
func parseHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildInstruments declares the process's metric set — the operator's
// createMetrics shape: names carry the chinit_ prefix (Prometheus
// translation adds no namespace), descriptions are the alert author's
// documentation.
func buildInstruments(m otelmetric.Meter) (*instruments, error) {
	i := &instruments{}
	var err error
	if i.jobRuns, err = m.Int64Counter("chinit_job_runs",
		otelmetric.WithDescription("number of chinit subcommand runs by outcome"),
		otelmetric.WithUnit("{run}")); err != nil {
		return nil, err
	}
	if i.jobDuration, err = m.Float64Histogram("chinit_job_duration_seconds",
		otelmetric.WithDescription("wall-clock duration of chinit subcommand runs"),
		otelmetric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.bootStep, err = m.Float64Histogram("chinit_boot_step_duration_seconds",
		otelmetric.WithDescription("duration of each boot pipeline step by outcome"),
		otelmetric.WithUnit("s")); err != nil {
		return nil, err
	}
	if i.bootOutcome, err = m.Int64Counter("chinit_boot_outcomes",
		otelmetric.WithDescription("number of boot pipeline runs by outcome (refresh included)"),
		otelmetric.WithUnit("{boot}")); err != nil {
		return nil, err
	}
	if i.sweepRows, err = m.Int64Gauge("chinit_fleet_sweep_rows",
		otelmetric.WithDescription("fleet task rows the sweep inventoried this tick, by phase"),
		otelmetric.WithUnit("{row}")); err != nil {
		return nil, err
	}
	if i.sweepStale, err = m.Int64Counter("chinit_fleet_sweep_redispatched",
		otelmetric.WithDescription("stale task rows the sweep re-dispatched (heartbeat expired, retries remaining)"),
		otelmetric.WithUnit("{task}")); err != nil {
		return nil, err
	}
	if i.sweepFailed, err = m.Int64Counter("chinit_fleet_sweep_failed",
		otelmetric.WithDescription("stale task rows the sweep moved to failed (retries exhausted)"),
		otelmetric.WithUnit("{task}")); err != nil {
		return nil, err
	}
	if i.taskClaimed, err = m.Int64Counter("chinit_fleet_task_claimed",
		otelmetric.WithDescription("fleet task rows this node claimed in a poll tick"),
		otelmetric.WithUnit("{task}")); err != nil {
		return nil, err
	}
	if i.taskDone, err = m.Int64Counter("chinit_fleet_task_completed",
		otelmetric.WithDescription("claimed fleet tasks whose motion completed"),
		otelmetric.WithUnit("{task}")); err != nil {
		return nil, err
	}
	if i.taskFailed, err = m.Int64Counter("chinit_fleet_task_failed",
		otelmetric.WithDescription("claimed fleet tasks whose motion failed (fenced motions included)"),
		otelmetric.WithUnit("{task}")); err != nil {
		return nil, err
	}
	if i.keeperUp, err = m.Float64Gauge("chinit_keeper_up",
		otelmetric.WithDescription("keeper mntr probe result (1 up, 0 down) — the OTEL mirror of the CloudWatch keeper_up alarm key"),
		otelmetric.WithUnit("1")); err != nil {
		return nil, err
	}
	if i.diskFree, err = m.Float64Gauge("chinit_disk_effective_free_gb",
		otelmetric.WithDescription("effective data-disk free space in GB — the OTEL mirror of the CloudWatch DiskEffectiveFreeGB alarm key"),
		otelmetric.WithUnit("GiB")); err != nil {
		return nil, err
	}
	if i.diskGrew, err = m.Int64Counter("chinit_disk_autoscaled",
		otelmetric.WithDescription("data-disk volumes this job grew (1 per grow event)"),
		otelmetric.WithUnit("{volume}")); err != nil {
		return nil, err
	}
	if i.backupOK, err = m.Float64Gauge("chinit_backup_success",
		otelmetric.WithDescription("the nightly backup pass verdict (1 clean, 0 failed) — the OTEL mirror of the CloudWatch BackupSuccess alarm key"),
		otelmetric.WithUnit("1")); err != nil {
		return nil, err
	}
	return i, nil
}

// ForceFlush flushes pending metrics and spans without tearing down
// the active providers.
func ForceFlush(ctx context.Context) error {
	mu.Lock()
	p := provider
	tp := tracerProvider
	mu.Unlock()
	flushCtx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	var err error
	if tp != nil {
		if ferr := tp.ForceFlush(flushCtx); ferr != nil && err == nil {
			err = ferr
		}
	}
	if p != nil {
		if ferr := p.ForceFlush(flushCtx); ferr != nil && err == nil {
			err = ferr
		}
	}
	return err
}

// Shutdown flushes the metric and trace providers and tears them down.
// Safe on the disabled zero state. The flush deadline bounds the
// collector-unreachable case so a dying job never hangs on telemetry.
func Shutdown(ctx context.Context) error {
	mu.Lock()
	p := provider
	tp := tracerProvider
	provider = nil
	meter = nil
	instr = nil
	tracerProvider = nil
	tracer = nil
	baseAttrs = nil
	initialized = false
	mu.Unlock()
	if p == nil && tp == nil {
		return nil
	}
	flushCtx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	var err error
	if tp != nil {
		if ferr := tp.ForceFlush(flushCtx); ferr != nil && err == nil {
			err = ferr
		}
		if serr := tp.Shutdown(flushCtx); serr != nil && err == nil {
			err = serr
		}
	}
	if p != nil {
		if ferr := p.ForceFlush(flushCtx); ferr != nil && err == nil {
			err = ferr
		}
		if serr := p.Shutdown(flushCtx); serr != nil && err == nil {
			err = serr
		}
	}
	return err
}

// TracesEnabled reports whether Init wired a live TracerProvider.
func TracesEnabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return initialized && tracer != nil
}

// StartJobSpan opens a root span for a chinit subcommand execution when
// trace export is enabled, returning a finish closure that sets the
// outcome status/attributes and ends the span. No-op when disabled.
func StartJobSpan(ctx context.Context, job string) (context.Context, func(outcome string, runErr error)) {
	mu.Lock()
	tr := tracer
	base := baseAttrs
	mu.Unlock()
	if tr == nil {
		return ctx, func(string, error) {}
	}
	spanCtx, span := tr.Start(ctx, "chinit.job."+job,
		oteltrace.WithSpanKind(oteltrace.SpanKindInternal),
		oteltrace.WithAttributes(attrs(base, attribute.String("chinit.job", job))...),
	)
	return spanCtx, func(outcome string, runErr error) {
		span.SetAttributes(attribute.String("chinit.outcome", outcome))
		if runErr != nil {
			span.RecordError(runErr)
			span.SetStatus(codes.Error, runErr.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// BootTraceStep describes one step's timing and verdict for BootTrace.
type BootTraceStep struct {
	Name        string
	Criticality string
	StartedAt   time.Time
	Duration    time.Duration
	Err         error
	Skipped     bool
}

// BootTraceRequest carries the boot pipeline run's timing and outcome
// so BootTrace can emit one correlated trace waterfall ("chinit.boot"
// root span + child spans per step).
type BootTraceRequest struct {
	StartedAt  time.Time
	FinishedAt time.Time
	Refresh    bool
	Fatality   error
	Steps      []BootTraceStep
}

// BootTrace emits a root "chinit.boot" span with one child span per
// executed step using the engine's recorded timestamps. No-op when
// trace telemetry is disabled.
func BootTrace(ctx context.Context, req BootTraceRequest) {
	mu.Lock()
	tr := tracer
	base := baseAttrs
	mu.Unlock()
	if tr == nil {
		return
	}
	bootStart := req.StartedAt
	if bootStart.IsZero() {
		bootStart = time.Now()
	}
	bootEnd := req.FinishedAt
	if bootEnd.Before(bootStart) {
		bootEnd = bootStart
	}
	rootCtx, rootSpan := tr.Start(ctx, "chinit.boot",
		oteltrace.WithTimestamp(bootStart),
		oteltrace.WithSpanKind(oteltrace.SpanKindInternal),
		oteltrace.WithAttributes(attrs(base, attribute.Bool("chinit.refresh", req.Refresh))...),
	)
	for _, st := range req.Steps {
		stepStart := st.StartedAt
		if stepStart.IsZero() {
			stepStart = bootStart
		}
		stepEnd := stepStart.Add(st.Duration)
		if stepEnd.After(bootEnd) {
			bootEnd = stepEnd
		}
		outcome := OutcomeCompleted
		if st.Err != nil {
			outcome = OutcomeFailed
		}
		if st.Skipped {
			outcome = OutcomeSkipped
		}
		_, child := tr.Start(rootCtx, "chinit.boot."+st.Name,
			oteltrace.WithTimestamp(stepStart),
			oteltrace.WithSpanKind(oteltrace.SpanKindInternal),
			oteltrace.WithAttributes(attrs(base,
				attribute.String("chinit.step", st.Name),
				attribute.String("chinit.criticality", st.Criticality),
				attribute.String("chinit.outcome", outcome),
				attribute.Bool("chinit.skipped", st.Skipped),
			)...),
		)
		if st.Err != nil {
			child.RecordError(st.Err)
			child.SetStatus(codes.Error, st.Err.Error())
		} else {
			child.SetStatus(codes.Ok, "")
		}
		child.End(oteltrace.WithTimestamp(stepEnd))
	}
	if req.Fatality != nil {
		rootSpan.RecordError(req.Fatality)
		rootSpan.SetStatus(codes.Error, req.Fatality.Error())
	} else {
		rootSpan.SetStatus(codes.Ok, "")
	}
	rootSpan.End(oteltrace.WithTimestamp(bootEnd))
}

// Enabled reports whether Init wired a live provider (the CLI's
// WARNING seam: disabled telemetry is loud once, not once per emit).
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return initialized
}

// emit is the common no-op guard: disabled telemetry answers false
// and the caller's recording is skipped (no allocation past the lock).
func emit() (*instruments, []attribute.KeyValue, bool) {
	mu.Lock()
	defer mu.Unlock()
	if !initialized || instr == nil {
		return nil, nil, false
	}
	return instr, baseAttrs, true
}

// attrs merges the base attrs with per-datum extras.
func attrs(base []attribute.KeyValue, extra ...attribute.KeyValue) []attribute.KeyValue {
	return append(base, extra...)
}

// Outcome attrs for job/boot counters.
const (
	// OutcomeCompleted is a successful run.
	OutcomeCompleted = "completed"
	// OutcomeFailed is an erroring run (the CLI's terminal error).
	OutcomeFailed = "failed"
	// OutcomeSkipped is a deliberate no-op run (role gates, not-
	// this-node's-job guards).
	OutcomeSkipped = "skipped"
)

// JobRun records one subcommand's outcome and wall-clock duration
// (the dispatch wrapper's single emit per process).
func JobRun(ctx context.Context, req JobRunRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	opts := otelmetric.WithAttributes(attrs(base,
		attribute.String("chinit.job", req.Job),
		attribute.String("chinit.outcome", req.Outcome))...)
	i.jobRuns.Add(ctx, 1, opts)
	i.jobDuration.Record(ctx, req.DurationSeconds, opts)
}

// JobRunRequest is the JobRun emit request.
type JobRunRequest struct {
	// Job is the subcommand name ("schema-sync", "boot", ...).
	Job string
	// Outcome is OutcomeCompleted/OutcomeFailed/OutcomeSkipped.
	Outcome string
	// DurationSeconds is the subcommand's wall-clock time.
	DurationSeconds float64
}

// BootStep records one boot step's duration by outcome (the engine's
// StepOutcome rows, emitted once at boot end — the histogram's
// cumulative view is what a slow-step alert keys on).
func BootStep(ctx context.Context, req BootStepRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	outcome := OutcomeCompleted
	if req.Failed {
		outcome = OutcomeFailed
	}
	if req.Skipped {
		outcome = OutcomeSkipped
	}
	i.bootStep.Record(ctx, req.DurationSeconds,
		otelmetric.WithAttributes(attrs(base,
			attribute.String("chinit.step", req.Step),
			attribute.String("chinit.outcome", outcome))...))
}

// BootStepRequest is the BootStep emit request.
type BootStepRequest struct {
	// Step is the pipeline step name (StepOutcome.Name).
	Step string
	// DurationSeconds is the step's engine-measured duration.
	DurationSeconds float64
	// Failed/Skipped mirror StepOutcome.Err/Skipped.
	Failed  bool
	Skipped bool
}

// BootOutcome records the whole pipeline's verdict (Fatality != nil is
// OutcomeFailed; a dry-run plan or a refresh that converged counts as
// completed).
func BootOutcome(ctx context.Context, req BootOutcomeRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	outcome := OutcomeCompleted
	if req.Failed {
		outcome = OutcomeFailed
	}
	i.bootOutcome.Add(ctx, 1, otelmetric.WithAttributes(attrs(base,
		attribute.Bool("chinit.refresh", req.Refresh),
		attribute.String("chinit.outcome", outcome))...))
}

// BootOutcomeRequest is the BootOutcome emit request.
type BootOutcomeRequest struct {
	// Failed is true when RunResult.Fatality != nil.
	Failed bool
	// Refresh is true for a boot -refresh run.
	Refresh bool
}

// FleetSweep records the coordinator's tick: the inventory gauges and
// the two transition counters. The domain result's fields are the
// exact numbers the journal line prints.
func FleetSweep(ctx context.Context, req FleetSweepRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	i.sweepRows.Record(ctx, int64(req.Rows),
		otelmetric.WithAttributes(attrs(base, attribute.String("chinit.fleet.phase", "total"))...))
	i.sweepRows.Record(ctx, int64(req.LeftPending),
		otelmetric.WithAttributes(attrs(base, attribute.String("chinit.fleet.phase", "pending"))...))
	i.sweepRows.Record(ctx, int64(req.LiveExecuting),
		otelmetric.WithAttributes(attrs(base, attribute.String("chinit.fleet.phase", "live_executing"))...))
	if req.Redispatched > 0 {
		i.sweepStale.Add(ctx, int64(req.Redispatched), otelmetric.WithAttributes(base...))
	}
	if req.Failed > 0 {
		i.sweepFailed.Add(ctx, int64(req.Failed), otelmetric.WithAttributes(base...))
	}
}

// FleetSweepRequest is the FleetSweep emit request (the domain
// FleetTaskSweepResult's fields).
type FleetSweepRequest struct {
	Rows          int
	Redispatched  int
	Failed        int
	LeftPending   int
	LiveExecuting int
}

// FleetPoll records one executor tick's claims and verdicts.
func FleetPoll(ctx context.Context, req FleetPollRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	if req.Claimed > 0 {
		i.taskClaimed.Add(ctx, int64(req.Claimed), otelmetric.WithAttributes(base...))
	}
	if req.Completed > 0 {
		i.taskDone.Add(ctx, int64(req.Completed), otelmetric.WithAttributes(base...))
	}
	if req.Failed > 0 {
		i.taskFailed.Add(ctx, int64(req.Failed), otelmetric.WithAttributes(base...))
	}
}

// FleetPollRequest is the FleetPoll emit request (the domain
// FleetTaskExecutorResult's fields).
type FleetPollRequest struct {
	Claimed   int
	Completed int
	Failed    int
}

// KeeperUp records the mntr probe verdict (the OTEL dual-write beside
// the CloudWatch PutMetric the keeper_down alarm already keys on).
func KeeperUp(ctx context.Context, req KeeperUpRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	i.keeperUp.Record(ctx, req.Up, otelmetric.WithAttributes(base...))
}

// KeeperUpRequest is the KeeperUp emit request.
type KeeperUpRequest struct {
	// Up is 1.0 (probe ok) or 0.0 (probe failed).
	Up float64
}

// DiskPressure records the disk-autoscale job's probe and grow
// outcome (the OTEL dual-write beside the CloudWatch push).
func DiskPressure(ctx context.Context, req DiskPressureRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	host := otelmetric.WithAttributes(attrs(base,
		attribute.String("chinit.host", req.Host))...)
	i.diskFree.Record(ctx, req.FreeGB, host)
	if req.Grew {
		i.diskGrew.Add(ctx, 1, host)
	}
}

// DiskPressureRequest is the DiskPressure emit request.
type DiskPressureRequest struct {
	// FreeGB is the effective free space (the gauge alarms key on).
	FreeGB float64
	// Grew is true when this run grew the volume.
	Grew bool
	// Host is the emitting node's hostname (the per-node dimension
	// the CloudWatch dimensions carry too).
	Host string
}

// BackupOutcome records the nightly backup pass's verdict (the OTEL
// dual-write beside the CloudWatch push the backup-missing alarm keys
// on).
func BackupOutcome(ctx context.Context, req BackupOutcomeRequest) {
	i, base, ok := emit()
	if !ok {
		return
	}
	host := otelmetric.WithAttributes(attrs(base,
		attribute.String("chinit.host", req.Host))...)
	v := 0.0
	if req.Success {
		v = 1.0
	}
	i.backupOK.Record(ctx, v, host)
}

// BackupOutcomeRequest is the BackupOutcome emit request.
type BackupOutcomeRequest struct {
	// Success is true when the create/upload/delete-local pass ran
	// clean.
	Success bool
	// Host is the emitting node's hostname.
	Host string
}
