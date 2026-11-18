// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestAppSuite is the entrypoint for the app package's Ginkgo specs:
// the CR-slot inventory (host_inventory_test.go), the job inventory's
// live-claims path (spec_inventory_test.go), the drain subcommand
// (drain_test.go) and the maintenance gating/two-pass GC
// (maintenance_test.go). One RunSpecs per package — Ginkgo forbids a
// second call; new spec files land as plain Describe files and run
// under this entrypoint.
func TestAppSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Chinit App Suite")
}
