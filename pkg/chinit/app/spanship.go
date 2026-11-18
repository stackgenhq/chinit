// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/operator"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
	"github.com/stackgenhq/chinit/pkg/chinit/telemetry"
)

const (
	defaultSpanShipCursorPath = "/var/lib/chinit/span-ship.json"
	defaultSpanShipLimit      = 5000
	spanShipTimeout           = 25 * time.Second
)

// spanShipCoordinator wires CLI flags, persisted node state, and
// infrastructure adapters into the domain service.SpanShipper.
type spanShipCoordinator struct {
	statePath  string
	cursorPath string
	batchLimit int
	// reader overrides operator.SpanLogStore when injected by tests.
	reader port.SpanLogReader
}

func newSpanShipCoordinator(args []string) (*spanShipCoordinator, error) {
	fs := flag.NewFlagSet("chinit span-ship", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "path to chinit state.json")
	cursorPath := fs.String("cursor", defaultSpanShipCursorPath, "path to span cursor JSON file")
	limit := fs.Int("limit", defaultSpanShipLimit, "max spans exported per tick")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return &spanShipCoordinator{
		statePath:  *statePath,
		cursorPath: *cursorPath,
		batchLimit: *limit,
	}, nil
}

func (c *spanShipCoordinator) run(ctx context.Context) error {
	ls := c.loadShippingSpec()
	tracesURL := ls.EffectiveTracesEndpoint()
	if tracesURL == "" {
		logger.GetLogger(ctx).Info("span-ship: traces_endpoint is empty; skipping")
		return nil
	}

	shipper := service.NewSpanShipper(service.SpanShipperDeps{
		Reader:     c.spanReader(),
		Cursors:    service.NewJSONSpanCursorStore(c.cursorPath),
		Sink:       telemetry.NewOTLPSpanSink(telemetry.OTLPSpanSinkConfig{EndpointURL: tracesURL, Headers: ls.EffectiveTracesHeaders()}),
		BatchLimit: c.batchLimit,
	})

	res, err := shipper.Ship(ctx, c.buildRequest())
	if err != nil {
		return err
	}
	if res.Exported > 0 {
		logger.GetLogger(ctx).Info("span-ship: exported ClickHouse spans",
			"exported", res.Exported, "cursor_us", res.HighWaterMicros, "endpoint", tracesURL)
	}
	return nil
}

func (c *spanShipCoordinator) loadShippingSpec() model.LogShipping {
	tcfg := loadTelemetryConfig()
	return model.LogShipping{
		Endpoint:       tcfg.Endpoint,
		TracesEndpoint: tcfg.TracesEndpoint,
		Headers:        tcfg.Headers,
		TracesHeaders:  tcfg.TracesHeaders,
	}
}

func (c *spanShipCoordinator) spanReader() port.SpanLogReader {
	if c.reader != nil {
		return c.reader
	}
	return operator.NewSpanLogStore(operator.SpanLogStoreConfig{
		Credentials: domain.Credentials{
			Username: "admin",
			Password: adminPassword(),
		},
	})
}

func (c *spanShipCoordinator) buildRequest() service.SpanShipRequest {
	st, _ := loadNodeState(c.statePath)
	if st == nil {
		return service.SpanShipRequest{}
	}
	return service.SpanShipRequest{
		Cluster:  st.clusterName(),
		Role:     st.Identity.Role,
		Hostname: st.Identity.Hostname,
		Cloud:    providerNameOf(st),
		Region:   st.Identity.Region,
	}
}

// runSpanShip is the CLI entrypoint for `chinit span-ship`.
func runSpanShip(args []string) error {
	coord, err := newSpanShipCoordinator(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), spanShipTimeout)
	defer cancel()
	return coord.run(ctx)
}
