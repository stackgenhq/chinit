// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package detect

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestDetectSuite is the package's single Ginkgo entrypoint.
func TestDetectSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cloud Detection Suite")
}
