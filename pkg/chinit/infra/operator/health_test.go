// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"

	"github.com/stretchr/testify/require"
)

// healthFakeServer is an httptest fake speaking the driver's TSV wire
// format: it answers the pool's SELECT 1 ping and serves one canned
// result per query marker. The shape mirrors the FindStaleReplicas
// tests — the health paths must exercise the real driver connection.
type healthFakeServer struct {
	// responses maps a substring of the query to the TSV body to
	// answer with (column headers first, then type rows, then data).
	responses map[string]string
	// captured records every query the server received.
	captured []string
}

// startHealthServer boots the fake and returns the address and port
// the inventory flags carry.
func startHealthServer(t *testing.T, fake *healthFakeServer) (string, int) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		fake.captured = append(fake.captured, string(query))
		w.Header().Set("Content-Type", "text/tab-separated-values; charset=UTF-8")
		for marker, body := range fake.responses {
			if strings.Contains(string(query), marker) {
				_, _ = w.Write([]byte(body))
				return
			}
		}
		// Default: the driver's pool ping plus a bare SELECT 1
		// proper TSV shape (name row, type row, then data) so the rows
		// parser accepts it.
		_, _ = w.Write([]byte("1\nUInt8\n1\n"))
	}))
	t.Cleanup(server.Close)
	return splitAddr(t, server.URL)
}

// healthInventory builds a one-host inventory entry around the fake's
// address.
func healthInventory(host string, _ int) domain.HostIdentity {
	return domain.HostIdentity{Name: "0-0", Address: host}
}

func TestHealthProbePingHostOverWire(t *testing.T) {
	fake := &healthFakeServer{responses: map[string]string{}}
	host, httpPort := startHealthServer(t, fake)

	probe := NewHealthProbe()
	result, err := probe.PingHost(context.Background(), port.HealthPingRequest{
		Host:     healthInventory(host, httpPort),
		HTTPPort: httpPort,
	})
	require.NoError(t, err)
	require.Equal(t, host, result.Address)
	// The ping went out over the real driver path.
	require.NotEmpty(t, fake.captured)
}

func TestHealthProbePingHostUnreachable(t *testing.T) {
	// A closed port: the per-host fail path — the fan-out in the
	// service reports this as a down host, never an abort.
	probe := NewHealthProbe()
	_, err := probe.PingHost(context.Background(), port.HealthPingRequest{
		Host:     domain.HostIdentity{Name: "0-0", Address: "127.0.0.1:1"},
		HTTPPort: 1,
		Timeout:  2e9,
	})
	require.Error(t, err)
}

func TestHealthProbeQueryReplicasSQLShape(t *testing.T) {
	// The fan-out query mirrors the schemer's stale-replica detection:
	// clusterAllReplicas with skip_unavailable_shards, and the fields
	// the readonly/self checks judge. The cluster name is the only
	// interpolated value (whitelisted by the CLI against the CR).
	//
	// The 26.8 column contract (the live schema proof): host_name was
	// REMOVED from system.replicas (renamed replica_name), so the
	// projection takes hostName() AS host_name, replica_name, and
	// indexes the Map-typed replica_is_active by replica name.
	fake := &healthFakeServer{responses: map[string]string{
		"system.replicas": "host_name\tdatabase\ttable\treplica_name\tis_readonly\treplica_is_active\n" +
			"String\tString\tString\tString\tUInt8\tUInt8\n" +
			"writer-0\tdemo\tevents\twriter-0\t0\t1\n" +
			"reader-0\tdemo\tevents\treader-0\t1\t0\n",
	}}
	host, httpPort := startHealthServer(t, fake)

	probe := NewHealthProbe()
	rows, err := probe.QueryReplicas(context.Background(), port.HealthReplicasRequest{
		RunOn:       healthInventory(host, httpPort),
		ClusterName: "demo",
		HTTPPort:    httpPort,
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, port.HealthReplicaRow{
		Host: "writer-0", Database: "demo", Table: "events",
		Replica: "writer-0", Readonly: false, Active: true,
	}, rows[0])
	require.Equal(t, port.HealthReplicaRow{
		Host: "reader-0", Database: "demo", Table: "events",
		Replica: "reader-0", Readonly: true, Active: false,
	}, rows[1])

	captured := strings.Join(fake.captured, "\n")
	require.Contains(t, captured, "clusterAllReplicas('demo', system.replicas)")
	require.Contains(t, captured, "skip_unavailable_shards = 1")
	require.Contains(t, captured, "hostName() AS host_name")
	require.Contains(t, captured, "replica_name")
	require.Contains(t, captured, "replica_is_active[replica_name]")
	require.Contains(t, captured, "is_readonly")
}

func TestHealthProbeQueryReplicasEmptyCluster(t *testing.T) {
	// A fresh cluster with no replicated tables yet: header only, zero
	// rows, no error (the replicas check reports zero tables, pass).
	fake := &healthFakeServer{responses: map[string]string{
		"system.replicas": "host_name\tdatabase\ttable\treplica_name\tis_readonly\treplica_is_active\n" +
			"String\tString\tString\tString\tUInt8\tUInt8\n",
	}}
	host, httpPort := startHealthServer(t, fake)

	probe := NewHealthProbe()
	rows, err := probe.QueryReplicas(context.Background(), port.HealthReplicasRequest{
		RunOn:       healthInventory(host, httpPort),
		ClusterName: "demo",
		HTTPPort:    httpPort,
	})
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestHealthProbeQueryTablesSQLShape(t *testing.T) {
	// The tables check reads user tables only — system databases are
	// excluded so the expected-set comparison is apples-to-apples.
	fake := &healthFakeServer{responses: map[string]string{
		"system.tables": "database\tname\n" +
			"String\tString\n" +
			"demo\tevents\n" +
			"demo\tminutes_log_index_summary\n" +
			"system\tquery_log\n",
	}}
	host, httpPort := startHealthServer(t, fake)

	probe := NewHealthProbe()
	tables, err := probe.QueryTables(context.Background(), port.HealthTablesRequest{
		Host:     healthInventory(host, httpPort),
		HTTPPort: httpPort,
	})
	require.NoError(t, err)
	require.Equal(t, []domain.TableRef{
		{Database: "demo", Name: "events"},
		{Database: "demo", Name: "minutes_log_index_summary"},
		{Database: "system", Name: "query_log"},
	}, tables)

	captured := strings.Join(fake.captured, "\n")
	require.Contains(t, captured, "FROM system.tables")
	require.Contains(t, captured, "NOT IN")
}

func TestHealthProbeEmptyAddressRejected(t *testing.T) {
	probe := NewHealthProbe()
	ctx := context.Background()
	_, err := probe.PingHost(ctx, port.HealthPingRequest{Host: domain.HostIdentity{Name: "0-0"}})
	require.ErrorContains(t, err, "no address")
	_, err = probe.QueryReplicas(ctx, port.HealthReplicasRequest{ClusterName: "demo"})
	require.ErrorContains(t, err, "no address")
	_, err = probe.QueryTables(ctx, port.HealthTablesRequest{})
	require.ErrorContains(t, err, "no address")
}

func TestExpectedTablesFromCRSQLFiles(t *testing.T) {
	// The stack's real spec shape: schema DDL rides the files section
	// as .sql entries. Qualified and bare names, materialized views
	// and dictionaries all count; CREATE DATABASE does not; non-.sql
	// files are ignored.
	cr := loadCHIFromFile(t, "example", "")
	specYAML := `
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseInstallation
metadata:
  name: schema-test
spec:
  configuration:
    clusters:
      - name: demo
        layout: { shardsCount: 1, replicasCount: 1 }
    files:
      initdb/init.sql: |
        CREATE DATABASE IF NOT EXISTS demo;
        CREATE TABLE IF NOT EXISTS demo.events (...) ENGINE = ReplicatedMergeTree;
        CREATE MATERIALIZED VIEW demo.mv_minutes TO demo.minutes AS SELECT 1;
      second.sql: |
        CREATE TABLE hourly_log_index_summary (...) ENGINE = SummingMergeTree;
      notes.xml: "<yandex/>"
`
	loader := NewSpecLoader()
	crAny, err := loader.LoadCHI(context.Background(), port.LoadCHIRequest{
		Spec: domain.SpecReference{Data: []byte(specYAML)},
	})
	require.NoError(t, err)

	tables := ExpectedTablesFromCR(crAny)
	// Sorted by database then name (empty database first). NOTE the
	// MV's TO-target is NOT listed: `CREATE MATERIALIZED VIEW ... TO
	// db.minutes` creates only the view — the target must be created
	// by its own statement to be expected.
	require.Equal(t, []domain.TableRef{
		{Name: "hourly_log_index_summary"},
		{Database: "demo", Name: "events"},
		{Database: "demo", Name: "mv_minutes"},
	}, tables)

	// A CR without .sql files yields nothing (the check reports "no
	// tables declared" — pass, not fail).
	require.Empty(t, ExpectedTablesFromCR(cr))
}

func TestExpectedTablesFromCRRejectsNonCHI(t *testing.T) {
	require.Nil(t, ExpectedTablesFromCR(nil))
	require.Nil(t, ExpectedTablesFromCR(map[string]any{}))
}

func TestParseCreatedTableNames(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{
			name: "qualified and bare names",
			sql:  "CREATE TABLE db.a (x Int);\nCREATE TABLE b (x Int);",
			want: []string{"b", "db.a"},
		},
		{
			name: "if not exists and materialized view",
			sql:  "CREATE TABLE IF NOT EXISTS db.a (x Int);\nCREATE MATERIALIZED VIEW db.mv TO db.t AS SELECT 1;",
			want: []string{"db.a", "db.mv"},
		},
		{
			name: "backticked names with spaces",
			sql:  "CREATE DICTIONARY db.`my dict` (...) ...;",
			want: []string{"db.my dict"},
		},
		{
			name: "database statements skipped",
			sql:  "CREATE DATABASE IF NOT EXISTS db;\nCREATE TABLE db.t (x Int);",
			want: []string{"db.t"},
		},
		{
			name: "comments stripped",
			sql:  "-- CREATE TABLE phantom.x (...) ...\nCREATE TABLE real.y (x Int);",
			want: []string{"real.y"},
		},
		{
			name: "empty document",
			sql:  "",
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, parseCreatedTableNames(c.sql))
		})
	}
}
