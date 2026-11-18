// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/apis/swversion"
	"github.com/altinity/clickhouse-operator/pkg/model/clickhouse"

	"github.com/stretchr/testify/require"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
)

// detectSoftwareVersion drives the schemer's version-gated SQL shape
// selection. The factory must read the version the fleet actually
// reports over the driver's real wire path (the operator's own
// app-based detection), and fall back to swversion.MinVersion() when
// no host answers — never a hardcoded constant.

func TestDetectSoftwareVersionFromLiveHost(t *testing.T) {
	fake := &healthFakeServer{responses: map[string]string{
		"SELECT version()": "26.8.2.7\nString\n26.8.2.7\n",
	}}
	host, httpPort := startHealthServer(t, fake)

	params := clickhouse.NewClusterConnectionParams(
		v1.ChSchemeHTTP, "default", "", "", httpPort,
	)
	got := detectSoftwareVersion(context.Background(), params, []domain.HostIdentity{
		{Name: "0-0", Address: host},
	})

	require.True(t, got.IsKnown(), "version must resolve from a live host")
	require.True(t, got.Matches(">= 26.8"), "detected version should be the host's reported one")
	require.Equal(t, "26.8.2.7", got.GetOriginal())
}

func TestDetectSoftwareVersionFallsBackToMinWhenUnreachable(t *testing.T) {
	// No reachable host: the closed port fails every probe and the
	// fallback must be the operator's own "unable to acquire" answer.
	params := clickhouse.NewClusterConnectionParams(
		v1.ChSchemeHTTP, "default", "", "", 1,
	)
	got := detectSoftwareVersion(context.Background(), params, []domain.HostIdentity{
		{Name: "0-0", Address: "127.0.0.1"},
	})

	require.True(t, got.IsKnown(), "MinVersion is a known version")
	require.True(t, got.Matches("< 22.12"), "fallback takes the pre-22.12 SQL shapes — the conservative branch")
}

func TestDetectSoftwareVersionSkipsDownHost(t *testing.T) {
	// The first host is down; the second answers. Detection walks the
	// inventory until one host responds (reader-bootstrap shape: one
	// live peer beside down ones).
	fake := &healthFakeServer{responses: map[string]string{
		"SELECT version()": "25.3.1\nString\n25.3.1\n",
	}}
	aliveHost, httpPort := startHealthServer(t, fake)

	params := clickhouse.NewClusterConnectionParams(
		v1.ChSchemeHTTP, "default", "", "", httpPort,
	)
	got := detectSoftwareVersion(context.Background(), params, []domain.HostIdentity{
		{Name: "0-0", Address: "127.0.0.1"}, // down: wrong port, probe skipped
		{Name: "0-1", Address: aliveHost},
	})

	require.True(t, got.IsKnown())
	require.True(t, got.Matches(">= 25.3"), "the answering host's version wins")
}

func TestDetectSoftwareVersionQueriesVersionOnWire(t *testing.T) {
	// The query the server receives must be the operator's own
	// HostClickHouseVersion SQL (SELECT version()) — the same shape the
	// operator's app-based detection sends.
	fake := &healthFakeServer{responses: map[string]string{
		"SELECT version()": "24.3.1\nString\n24.3.1\n",
	}}
	host, httpPort := startHealthServer(t, fake)

	params := clickhouse.NewClusterConnectionParams(
		v1.ChSchemeHTTP, "default", "", "", httpPort,
	)
	_ = detectSoftwareVersion(context.Background(), params, []domain.HostIdentity{
		{Name: "0-0", Address: host},
	})

	require.NotEmpty(t, fake.captured)
	var sawVersionQuery bool
	for _, query := range fake.captured {
		if strings.Contains(strings.ToUpper(query), "SELECT VERSION()") {
			sawVersionQuery = true
			break
		}
	}
	require.True(t, sawVersionQuery, "detection must query SELECT version(), got %q", fake.captured)
	_ = swversion.MinVersion()
}
