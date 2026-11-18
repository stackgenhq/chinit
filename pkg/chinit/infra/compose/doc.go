// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package compose implements the local Docker Compose cloud provider.
// It satisfies chinit's mandatory Provider ports (metadata, secrets,
// config bundles) and optional fleet-coordination ports (ordinal
// leases, lease listing, node status, role specs, refresh locks) using
// local files and container environment so full systemd-enabled
// ClickHouse clusters can run and be tested under Docker Compose
// without cloud infrastructure.
package compose
