// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package detect

import (
	"context"
	"net/http"
	"time"
)

// IMDS endpoints. Both clouds use 169.254.169.254 with distinct
// authentication headers; a quick GET with a short timeout answers
// "am I on this platform?" without pulling credentials.
const (
	awsIMDSURL   = "http://169.254.169.254/latest/api/token"
	azureIMDSURL = "http://169.254.169.254/metadata/instance?api-version=2021-02-01"
)

// probeTimeout bounds each probe: detection must not hang a boot on a
// node whose network is not up yet (the engine retries the whole boot,
// but detection itself should be snappy).
const probeTimeout = 2 * time.Second

// probeAWSIMDS reports whether the EC2 IMDS answers. IMDSv2 requires the
// token PUT; a 404/403 from a non-EC2 host means "not AWS".
func probeAWSIMDS(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, awsIMDSURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "300")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}

// probeAzureIMDS reports whether the Azure IMDS answers. Azure requires
// the Metadata: true header and returns 404 on other clouds.
func probeAzureIMDS(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, azureIMDSURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Metadata", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK, nil
}
