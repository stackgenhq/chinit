// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package chinit provides the core domain models, cloud provider interfaces,
// convergent bootstrap engine, and maintenance services for managing stateful
// ClickHouse clusters on cloud virtual machines.
//
// Subpackages are organized into four layers:
//   - app: CLI entry point, subcommand dispatch, and provider wiring
//   - domain/model: Declarative BootSpec, NodeIdentity, disruption budgets, and lifecycle hooks
//   - domain/port: Cloud provider capability interfaces and provider registry
//   - domain/service: Convergent boot pipeline, retry engine, and cluster maintenance services
//   - infra/*: Cloud adapters (AWS, Azure), ClickHouse operator adapters, and local OS operations
package chinit
