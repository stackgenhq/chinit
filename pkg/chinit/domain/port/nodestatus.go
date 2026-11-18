// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"
)

// NodeHealth is the fleet-status row's verdict on a node. Derived from
// what the publisher can observe locally (the server's uptime and the
// settled identity) — never guessed.
type NodeHealth string

const (
	// NodeHealthy: the server answered the local probe.
	NodeHealthy NodeHealth = "healthy"
	// NodeDegraded: the node reports but the server probe failed
	// (the lease is live; the server is not answering).
	NodeDegraded NodeHealth = "degraded"
)

// NodeStatus is one node's publishable state: what budget checks,
// rollouts, and fleet-status consume. It rides the claims table's key
// space (the "status/<hostname>" prefix on AWS; the "node-status"
// partition on Azure) — the same one-table-one-liveness-view contract
// as the fleet task rows.
type NodeStatus struct {
	// Hostname is the node's settled cluster identity.
	Hostname string
	// Role is the node's role (writer/reader/keeper/backup).
	Role string
	// InstanceID is the provider instance id (diagnostics parity
	// with the claim rows).
	InstanceID string
	// SpecHash is the converged boot fingerprint (the config-check
	// job's resume key — the status row surfaces drift to the fleet).
	SpecHash string
	// ClickHouseVersion is the server binary version ("24.8.7.53"
	// — the version gate's input during rollouts).
	ClickHouseVersion string
	// Health is the local-probe verdict.
	Health NodeHealth
	// KeeperQuorum reports whether the Keeper ensemble is reachable and
	// answering mntr probes from this node. Writers and readers probe the
	// Keeper endpoint; Keepers probe localhost.
	KeeperQuorum bool
	// Cordoned is the maintenance flag: a cordoned node is excluded
	// from task dispatch and GC victim lists; its lease keeps renewing
	// (identity preserved).
	Cordoned bool
	// UpdatedAt is the publish time (unix seconds; a stale row is
	// "unknown", never healthy).
	UpdatedAt int64
}

// NodeStatusPublish is the input to StatusPublisher.PublishStatus.
type NodeStatusPublish struct {
	// Table is the provider lease/status table.
	Table string
	// Status is the row to publish (Hostname must be non-empty).
	Status NodeStatus
}

// NodeStatusList is the input to StatusPublisher.ListStatus.
type NodeStatusList struct {
	// Table is the provider lease/status table.
	Table string
	// Hostname filters to one node when non-empty (fleet-status's
	// single-node view); empty lists the fleet.
	Hostname string
}

// StatusPublisher is the fleet-status capability: each node publishes
// its own status row on the lease-renew tick, and any node (or an
// operator via `chinit fleet-status`) reads the fleet's view.
//
// Why a port (and why beside the claims): the operator's Status object
// is a cluster-scoped artifact a control loop writes; chinit has no
// control loop — but the claims table already carries the fleet's
// liveness. The status rows extend the same table with a
// "status/<hostname>" key space: one conditional write per node per
// tick, one scan for the fleet view, no new infrastructure. The
// cordon flag lives here too — the row is the one place the fleet's
// automation (dispatch, GC, rollouts) and an operator (cordon) agree
// on a node's maintenance state.
//
// Fencing: the row's writer is the node itself; a foreign update is
// refused by the CAS on instance_id (the cordon toggle's protection —
// one node can never cordon another by accident). An operator's
// cordon/uncordon writes carry the operator's own identity marker and
// bypass the holder CAS via the toggle request.
type StatusPublisher interface {
	// PublishStatus writes this node's status row (upsert; the row's
	// TTL rides the lease's — a dead node's status ages out with its
	// claim).
	PublishStatus(ctx context.Context, req NodeStatusPublish) error
	// ListStatus returns the fleet's status rows (Hostname filter
	// when non-empty), freshest-first within a hostname.
	ListStatus(ctx context.Context, req NodeStatusList) ([]NodeStatus, error)
	// SetCordon flips a node's cordoned flag. The actor is the
	// operator's CLI (any node) or the node itself; the write is a
	// targeted update, never a whole-row replace (a cordon toggle
	// racing a lease-renew publish must not drop either's fields).
	SetCordon(ctx context.Context, req NodeCordonRequest) error
}

// NodeCordonRequest is the input to StatusPublisher.SetCordon.
type NodeCordonRequest struct {
	// Table is the provider lease/status table.
	Table string
	// Hostname is the node to flag.
	Hostname string
	// Cordoned is the new flag value.
	Cordoned bool
}

// AsStatusPublisher returns the status-publishing capability, or nil.
func AsStatusPublisher(p Provider) StatusPublisher {
	if c, ok := p.(StatusPublisher); ok {
		return c
	}
	return nil
}
