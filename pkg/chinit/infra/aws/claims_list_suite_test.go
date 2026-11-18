// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// TestClaimsListSuite is the package's single Ginkgo entrypoint: the
// live-claims lister specs (claims_list_test.go) and the S3 bundle
// fetch specs (s3bundle_test.go) both run here — one RunSpecs per
// package, per the repo's BDD mandate.
func TestClaimsListSuite(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DynamoDB Claim Lister Suite")
}
