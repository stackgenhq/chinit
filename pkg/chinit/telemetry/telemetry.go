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
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	otelresource "go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// flushTimeout bounds Shutdown's ForceFlush — a wedged collector must
// never wedge the node's jobs on THEIR way out.
const flushTimeout = 5 * time.Second

// Config is the init request (the two-parameter style: one struct,
// no growing positional lists).
type Config struct {
	// Endpoint is the OTLP/HTTP metrics target — the spec's
	// log_shipping.endpoint. Empty disables the whole package (every
	// emit no-ops).
	Endpoint string
	// Headers are comma-separated k=v pairs from log_shipping.headers
	// (bearer tokens for collector front doors that need them).
	Headers string
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
	mu          sync.Mutex
	provider    *metric.MeterProvider
	meter       otelmetric.Meter
	instr       *instruments
	baseAttrs   []attribute.KeyValue
	initialized bool
)

// Init installs the process-wide provider. Idempotent: a second call
// (tests, re-entry) is a no-op while the first provider lives. When
// cfg.Endpoint is empty the package stays in its disabled zero state —
// Init returns nil and every emit no-ops, the same degradation rule as
// the MetricPusher capability (a cloud without a metrics pipeline
// logs a WARNING at the callsite and the job still exits 0).
func Init(ctx context.Context, cfg Config) error {
	mu.Lock()
	defer mu.Unlock()
	if initialized {
		return nil
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil // telemetry disabled — emits no-op
	}
	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithEndpointURL(strings.TrimSuffix(cfg.Endpoint, "/")+"/v1/metrics"),
		otlpmetrichttp.WithHeaders(parseHeaders(cfg.Headers)),
	)
	if err != nil {
		return fmt.Errorf("telemetry exporter: %w", err)
	}
	// Oneshot discipline: the interval is a wedged-process backstop
	// only — Shutdown's ForceFlush is the real delivery path.
	return initWithReaderLocked(metric.NewPeriodicReader(exporter, metric.WithInterval(time.Hour)), cfg)
}

// InitWithReader installs the provider over a caller-supplied reader
// instead of the OTLP exporter (the specs' collection seam — a
// sdk/metric/manual.Reader collects the datapoints in-process; the
// production path never calls it). Same config, same instruments, same
// disabled-when-empty rule.
func InitWithReader(cfg Config, reader metric.Reader) error {
	mu.Lock()
	defer mu.Unlock()
	if initialized {
		return nil
	}
	if reader == nil {
		return nil // telemetry disabled — emits no-op
	}
	return initWithReaderLocked(reader, cfg)
}

// initWithReaderLocked assembles the provider over one reader (the
// OTLP exporter arrives wrapped in a periodic reader; the test seam
// passes a manual.Reader straight through). The schemaless merge rule
// is the operator's (machinery.go): a schema URL conflict between
// Default() and our semconv would be an SDK-upgrade crash, not a
// runtime condition.
func initWithReaderLocked(reader metric.Reader, cfg Config) error {
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

// Shutdown flushes the provider and tears it down. Safe on the
// disabled zero state. The flush deadline bounds the collector-
// unreachable case so a dying job never hangs on telemetry; a flush
// failure is returned (the CLI logs it as a WARNING — the job's own
// outcome stands, the same rule as pushKeeperUp).
func Shutdown(ctx context.Context) error {
	mu.Lock()
	p := provider
	provider = nil
	meter = nil
	instr = nil
	baseAttrs = nil
	initialized = false
	mu.Unlock()
	if p == nil {
		return nil
	}
	var err error
	defer func() {
		if serr := p.Shutdown(ctx); serr != nil && err == nil {
			err = serr
		}
	}()
	flushCtx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()
	err = p.ForceFlush(flushCtx)
	return err
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
