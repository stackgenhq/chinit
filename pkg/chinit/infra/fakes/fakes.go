// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RoundTripper is an http.RoundTripper over canned responses keyed by
// URL substring.
type RoundTripper struct {
	// Responses maps a URL substring to the body served (any request
	// whose URL contains the key gets the body, status 200).
	Responses map[string]string
}

// RoundTrip serves the first matching canned response; unmatched URLs
// get 404 (a probe that misses must look absent, not error).
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for fragment, body := range rt.Responses {
		if strings.Contains(req.URL.String(), fragment) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     http.Header{},
				Request:    req,
			}, nil
		}
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

// Client builds an *http.Client over the canned responses.
func (rt *RoundTripper) Client() *http.Client {
	return &http.Client{Transport: rt}
}

// AWSIMDS serves the EC2 IMDSv2 document set for one fake instance.
func AWSIMDS() *RoundTripper {
	return &RoundTripper{Responses: map[string]string{
		"latest/api/token":                             "fake-imds-token",
		"latest/meta-data/instance-id":                 "i-conformance-fake",
		"latest/meta-data/placement/region":            "us-east-1",
		"latest/meta-data/placement/availability-zone": "us-east-1a",
		"latest/meta-data/local-ipv4":                  "10.0.1.10",
		"latest/meta-data/hostname":                    "ip-10-0-1-10.ec2.internal",
		"latest/meta-data/instance-type":               "m7g.xlarge",
	}}
}

// AzureIMDS serves the Azure instance document for one fake VM.
func AzureIMDS() *RoundTripper {
	doc := `{
  "compute": {
    "name": "conformance-vm",
    "vmId": "11111111-2222-3333-4444-555555555555",
    "zone": "1",
    "location": "eastus"
  },
  "network": {
    "interface": [
      {"ipv4": {"ipAddress": [{"privateIpAddress": "10.0.1.10"}]}}
    ]
  }
}`
	return &RoundTripper{Responses: map[string]string{
		"metadata/instance": doc,
	}}
}

// String makes the fake printable in test failure output.
func (rt *RoundTripper) String() string {
	keys := make([]string, 0, len(rt.Responses))
	for k := range rt.Responses {
		keys = append(keys, k)
	}
	return fmt.Sprintf("fake(%s)", strings.Join(keys, ", "))
}
