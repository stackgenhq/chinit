// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

// Package compose_test is the E2E acceptance suite for chinit on the
// local Docker Compose cluster (deploy/compose/docker-compose.yaml).
//
// It builds the chinit-node image from the CURRENT checkout, boots the
// full systemd cluster on fresh volumes (keeper + writer + reader), and
// verifies the stack's core promise end to end:
//
//  1. keeper-0's ensemble answers ruok → imok
//  2. chinit-boot finished on writer-0 and reader-0 (marker + live SQL)
//  3. the compose-demo cluster lists both replicas on both nodes
//  4. a row WRITTEN on writer-0 arrives on READER-0 — ReplicatedMergeTree
//     replication across the secure interserver port, the whole point
//  5. the events replica is writable with all replicas active
//
// The suite is gated behind the `acceptance` build tag so a plain
// `go test ./...` never boots docker; run it with:
//
//	go test -tags acceptance ./acceptance_test/compose/ -timeout 30m -v
//
// Flags (via standard `go test -args`):
//
//	-args -keep-cluster    keep the cluster running on success
//	-args -reuse-cluster   reuse a running cluster (no rebuild)
//
// A failing spec's output is self-diagnosing: the suite prints compose
// ps plus the affected nodes' chinit-boot and clickhouse-server journals.
package compose_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestAcceptance is the suite's single entrypoint (one RunSpecs per
// package — Ginkgo forbids a second call).
func TestAcceptance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Chinit Compose E2E Acceptance Suite")
}
