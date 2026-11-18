// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestInventorySuite is the entrypoint for the service package's
// Ginkgo specs: the live-claims inventory suite (inventory_test.go)
// and the writer-ASG identity/ceiling suite (ceiling_test.go). One
// RunSpecs per package — Ginkgo forbids a second call.
func TestInventorySuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Chinit Service Suite")
}
