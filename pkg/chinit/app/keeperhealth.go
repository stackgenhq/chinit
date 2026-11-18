// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

// runKeeperHealth probes local ClickHouse Keeper health via mntr and
// publishes the keeper_up metric.
func runKeeperHealth(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := clusterOps.KeeperMntr(ctx, clusterops.MntrRequest{
		Address: "localhost:9181",
		Timeout: 30 * time.Second,
	})
	// The verbatim mntr payload is the probe's report: one record,
	// the payload as an attribute (the bash-era `echo mntr | nc` piped
	// the same text to the journal).
	logger.GetLogger(ctx).Info("keeper-health: mntr probe result", "output", out)

	up := 0.0
	if err == nil {
		up = 1.0
	}
	pushKeeperUp(ctx, st, up)
	return err
}

// pushKeeperUp publishes the keeper_up datapoint through the
// provider's MetricPusher capability (absent → log-only degradation),
// and dual-writes the OTEL gauge beside it — the Log Explorer alerts
// key on the OTLP copy the way the CloudWatch alarm keys on its own
// datapoint (either pipeline being broken never masks the other).
func pushKeeperUp(ctx context.Context, st *nodeState, up float64) {
	log := logger.GetLogger(ctx)
	telemetry.KeeperUp(ctx, telemetry.KeeperUpRequest{Up: up})
	provider, perr := buildProvider(providerNameOf(st), st.Identity.Region)
	if perr != nil {
		log.Warn("keeper-health cannot build provider for metric push", "err", perr)
		return
	}
	pusher := port.AsMetricPusher(provider)
	if pusher == nil {
		log.Warn("provider has no metric-push capability; keeper_up not published (the alarm stays INSUFFICIENT_DATA)",
			"cloud", providerNameOf(st),
			"alarm", st.clusterName()+"-keeper-down")
		return
	}
	perr = pusher.PutMetric(ctx, port.MetricPushRequest{
		Namespace:  "ClickHouse/Keeper",
		Name:       "keeper_up",
		Value:      up,
		Unit:       "Count",
		Dimensions: map[string]string{"Cluster": st.clusterName()},
	})
	if perr != nil {
		log.Warn("keeper_up push failed (probe result stands)", "err", perr)
	}
}
