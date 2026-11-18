// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
)

// ClusterHealthProbe reads state of a ClickHouse cluster for
// health verification: server reachability, replication state and the
// existing tables.
//
// Why a port and not inline SQL in the service: the checks are thin, but
// they still cross the process boundary (HTTP/TCP to cluster hosts); the
// domain must stay free of driver and operator-machinery types so it can
// be exercised with fakes (the same shape KeeperMntrService uses).
type ClusterHealthProbe interface {
	// PingHost checks that one inventory host answers SQL queries
	// (the driver's SELECT 1 health probe).
	PingHost(ctx context.Context, request HealthPingRequest) (HealthHostResult, error)
	// QueryVersion reads the server binary version from one host (the
	// status publisher's version gate input; the rollout coordinator
	// compares it against the target).
	QueryVersion(ctx context.Context, request HealthPingRequest) (string, error)
	// QueryReplicas reads replication state across the cluster via the
	// run-on host (clusterAllReplicas over system.replicas, mirroring
	// the schemer's fan-out). The self-registration check consumes the
	// same rows — the fan-out is cluster-wide and tags host_name, so
	// "is my replica registered" needs no separate per-host query.
	QueryReplicas(ctx context.Context, request HealthReplicasRequest) ([]HealthReplicaRow, error)
	// QueryTables reads existing table names on one host (the
	// tables-present check source).
	QueryTables(ctx context.Context, request HealthTablesRequest) ([]domain.TableRef, error)
}

// HealthPingRequest is the input to ClusterHealthProbe.PingHost.
type HealthPingRequest struct {
	// Host is the inventory host to ping.
	Host domain.HostIdentity
	// ClusterName is the ClickHouse cluster name (unused by the current
	// ping, carried for adapter parity).
	ClusterName string
	// Credentials connect to the host.
	Credentials domain.Credentials
	// HTTPPort is the HTTP interface port (default 8123).
	HTTPPort int
	// Secure marks whether the host requires TLS.
	Secure bool
	// Timeout bounds the probe.
	Timeout time.Duration
}

// HealthHostResult is the outcome of one host ping.
type HealthHostResult struct {
	// Address is the host the probe ran against (echoed for per-host
	// reporting).
	Address string
}

// HealthReplicasRequest is the input to ClusterHealthProbe.QueryReplicas.
type HealthReplicasRequest struct {
	// RunOn is the host that executes the fan-out query (its address
	// resolves through the inventory, exactly like replica gc).
	RunOn domain.HostIdentity
	// ClusterName is the ClickHouse cluster name for clusterAllReplicas.
	ClusterName string
	// Credentials connect to the run-on host.
	Credentials domain.Credentials
	// HTTPPort is the HTTP interface port (default 8123).
	HTTPPort int
	// Secure marks whether the cluster requires TLS.
	Secure bool
	// Timeout bounds the probe.
	Timeout time.Duration
}

// HealthReplicaRow is one row of replication state from system.replicas.
type HealthReplicaRow struct {
	// Host is the host_name the row was reported for.
	Host string
	// Database of the replicated table.
	Database string
	// Table is the replicated table name.
	Table string
	// Replica is the replica name (the {replica} macro identity).
	Replica string
	// Readonly is is_readonly: 1 means the replica lost its Keeper
	// connection and refuses writes.
	Readonly bool
	// Active is replica_is_active for this replica.
	Active bool
}

// HealthTablesRequest is the input to ClusterHealthProbe.QueryTables.
type HealthTablesRequest struct {
	// Host is the inventory host to list tables on.
	Host domain.HostIdentity
	// Credentials connect to the host.
	Credentials domain.Credentials
	// HTTPPort is the HTTP interface port (default 8123).
	HTTPPort int
	// Secure marks whether the host requires TLS.
	Secure bool
	// Timeout bounds the probe.
	Timeout time.Duration
}
