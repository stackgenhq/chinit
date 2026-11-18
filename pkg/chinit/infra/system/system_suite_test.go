// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestSystemSuite is the package's single Ginkgo entrypoint: the disk
// mount contracts (disks_test.go) and the unit/file installers
// (units_test.go).
func TestSystemSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Chinit System Suite")
}
