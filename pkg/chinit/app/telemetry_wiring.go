// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

// telemetrySpec is the log_shipping projection off the bundle's
// chinit.yaml (the same partial-parse discipline as inventorySpec: a
// model rename must not break the jobs' telemetry). The boot reads
// the same fields off its own parsed spec — this projection is the
// subcommand path, where the full spec was never parsed.
type telemetrySpec struct {
	LogShipping struct {
		Endpoint string `yaml:"endpoint"`
		Headers  string `yaml:"headers"`
	} `yaml:"log_shipping"`
}

// loadTelemetryConfig reads the bundle spec's log_shipping section
// (empty endpoint = telemetry disabled — the node's own convergence
// contract: the optional pipeline never gates a job).
func loadTelemetryConfig() telemetry.Config {
	cfg := telemetry.Config{ServiceVersion: Version}
	data, err := os.ReadFile(inventorySpecPath())
	if err != nil {
		return cfg
	}
	var spec telemetrySpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return cfg
	}
	cfg.Endpoint = spec.LogShipping.Endpoint
	cfg.Headers = spec.LogShipping.Headers
	return cfg
}

// initJobTelemetry wires the process-wide provider for a maintenance
// subcommand: the bundle spec's log_shipping endpoint, the identity
// snapshot's dimensions. A disabled endpoint is one loud WARNING
// (per process, not per emit) and the job runs on.
func initJobTelemetry(ctx context.Context, st *nodeState) {
	cfg := loadTelemetryConfig()
	cfg.InstanceID = st.Identity.Hostname
	cfg.Cluster = st.clusterName()
	cfg.Role = string(st.Identity.Role)
	cfg.Cloud = providerNameOf(st)
	cfg.Region = st.Identity.Region
	if err := telemetry.Init(ctx, cfg); err != nil {
		logger.GetLogger(ctx).Warn("telemetry init failed; metrics disabled for this run", "err", err.Error())
	}
	if !telemetry.Enabled() {
		logger.GetLogger(ctx).Warn("telemetry disabled: log_shipping.endpoint is empty in the bundle spec (set it to the OTLP collector to capture chinit metrics)")
	}
}

// shutdownTelemetry flushes the provider on the process's way out
// (every exit path — success, failure, usage errors alike: a failed
// job's outcome metric is the one the alert needs MOST). A flush
// failure is a WARNING, never a job failure — the pushKeeperUp rule.
func shutdownTelemetry(ctx context.Context) {
	if err := telemetry.Shutdown(ctx); err != nil {
		logger.GetLogger(ctx).Warn("telemetry flush failed (job outcome stands)", "err", err.Error())
	}
}

// initBootTelemetry wires the provider for the boot pipeline: the
// spec's log_shipping endpoint plus the provider's instance id (the
// hostname is unsettled this early — the identity claim is a step).
// Same loud-once WARNING when the endpoint is empty.
func initBootTelemetry(ctx context.Context, spec *model.BootSpec, meta port.InstanceMetadata, cloud string) {
	cfg := telemetry.Config{
		ServiceVersion: Version,
		Endpoint:       spec.LogShipping.Endpoint,
		Headers:        spec.LogShipping.Headers,
		InstanceID:     meta.InstanceID,
		Cluster:        spec.ClusterName,
		Role:           string(spec.Role),
		Cloud:          cloud,
		Region:         spec.Region,
	}
	if err := telemetry.Init(ctx, cfg); err != nil {
		logger.GetLogger(ctx).Warn("telemetry init failed; metrics disabled for this run", "err", err.Error())
	}
	if !telemetry.Enabled() {
		logger.GetLogger(ctx).Warn("telemetry disabled: log_shipping.endpoint is empty in the spec (set it to the OTLP collector to capture chinit metrics)")
	}
}

// emitBootTelemetry records the pipeline's per-step durations and the
// overall verdict (the engine's own StepOutcome rows — no re-timing,
// the engine stays SDK-free; the telemetry reads the result only).
func emitBootTelemetry(ctx context.Context, res service.RunResult, refresh bool) {
	if !telemetry.Enabled() {
		return
	}
	for _, st := range res.Steps {
		telemetry.BootStep(ctx, telemetry.BootStepRequest{
			Step:            st.Name,
			DurationSeconds: st.Duration.Seconds(),
			Failed:          st.Err != nil,
			Skipped:         st.Skipped,
		})
	}
	telemetry.BootOutcome(ctx, telemetry.BootOutcomeRequest{
		Failed:  res.Fatality != nil,
		Refresh: refresh,
	})
}

// runWithTelemetry wraps one registry subcommand: init, time, emit,
// flush. The subcommand's own error is untouched — telemetry observes
// the run, it never classifies it (the exit-code contract belongs to
// Main).
//
// The boot command owns its own init (it knows the spec, the detected
// cloud and the provider metadata — richer dimensions than the state
// file); every other subcommand is a timer tick over the settled
// identity, so the wrapper reads them off the state snapshot.
func runWithTelemetry(c command, args []string) error {
	ctx := context.Background()
	if !c.Boot {
		st, _ := loadNodeState(service.ChinitStatePath)
		if st != nil {
			initJobTelemetry(ctx, st)
		} else {
			// No state file (a pre-boot node, a manual run off a
			// timer): still ship the job outcome, dimensionless
			// beyond version.
			if err := telemetry.Init(ctx, loadTelemetryConfig()); err != nil {
				logger.GetLogger(ctx).Warn("telemetry init failed; metrics disabled for this run", "err", err.Error())
			}
		}
	}
	start := time.Now()
	err := c.Run(args)
	outcome := telemetry.OutcomeCompleted
	if err != nil {
		outcome = telemetry.OutcomeFailed
	}
	telemetry.JobRun(ctx, telemetry.JobRunRequest{
		Job:             c.Name,
		Outcome:         outcome,
		DurationSeconds: time.Since(start).Seconds(),
	})
	shutdownTelemetry(ctx)
	return err
}
