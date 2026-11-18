// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package conformance_test wires the real provider constructors through
// the conformance suite with each provider's injectable fakes. This is
// the proof the factory contract holds for every registered cloud.
package conformance_test

import (
	"net/http"
	"testing"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service/conformance"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/aws"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/azure"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/fakes"
)

// TestProviderConformance runs the contract suite against every
// registered provider with fake IMDS endpoints, canned secrets and a
// canned bundle — no real API calls, deterministic CI. Adding GCP/Civo
// later means adding a suite entry here; the scenarios never change.
func TestProviderConformance(t *testing.T) {
	spec := &model.BootSpec{
		Role:         model.RoleReader,
		IdentityMode: model.IdentityLease,
		ClusterName:  "conformance",
	}

	secrets := &fakes.SecretSource{Values: map[string]string{
		"conformance-test-secret": "conformance-secret-value",
	}}
	bundle := &fakes.BundleSource{Files: map[string]string{
		"network.xml": "<clickhouse><listen_host>0.0.0.0</listen_host></clickhouse>",
		"storage.xml": "<clickhouse><storage_configuration/></clickhouse>",
	}}

	awsProvider := aws.New(port.Config{Region: "us-east-1", Spec: spec})
	if p, ok := awsProvider.(interface {
		WithIMDSClient(c interface {
			Do(req *http.Request) (*http.Response, error)
		}) port.Provider
	}); ok {
		awsProvider = p.WithIMDSClient(fakes.AWSIMDS().Client())
	}
	if p, ok := awsProvider.(interface {
		WithSecrets(port.SecretSource) port.Provider
	}); ok {
		awsProvider = p.WithSecrets(secrets)
	}
	if p, ok := awsProvider.(interface {
		WithBundle(port.ConfigSource) port.Provider
	}); ok {
		awsProvider = p.WithBundle(bundle)
	}

	azureProvider := azure.New(port.Config{Region: "eastus", Spec: spec})
	if p, ok := azureProvider.(interface {
		WithIMDSClient(c interface {
			Do(req *http.Request) (*http.Response, error)
		}) port.Provider
	}); ok {
		azureProvider = p.WithIMDSClient(fakes.AzureIMDS().Client())
	}
	if p, ok := azureProvider.(interface {
		WithSecrets(port.SecretSource) port.Provider
	}); ok {
		azureProvider = p.WithSecrets(secrets)
	}
	if p, ok := azureProvider.(interface {
		WithBundle(port.ConfigSource) port.Provider
	}); ok {
		azureProvider = p.WithBundle(bundle)
	}

	suites := []conformance.Suite{
		{Name: "aws", Provider: awsProvider},
		{Name: "azure", Provider: azureProvider},
	}
	conformance.RunAll(t, suites)
}
