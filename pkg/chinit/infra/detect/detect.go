// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package detect

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Names of the supported providers (the registry keys).
const (
	AWSName     = "aws"
	AzureName   = "azure"
	ComposeName = "compose"
)

// Result is what detection produced, for the boot log.
type Result struct {
	Provider string
	Via      string // "flag", "env", "dmi", "imds"
	Detail   string
}

// Options carries the override inputs and injectable probes (tests
// replace them with canned data).
type Options struct {
	// Flag is the -cloud override from the command line.
	Flag string
	// EnvName is the CHINIT_CLOUD environment value.
	EnvName string

	// DMIVendorFunc reads /sys/class/dmi/id/sys_vendor (nil → real read).
	DMIVendorFunc func() (string, error)
	// ProbeAWS contacts the EC2 IMDS and reports whether the node is on
	// EC2 (nil → real probe).
	ProbeAWS func(ctx context.Context) (bool, error)
	// ProbeAzure contacts the Azure IMDS and reports whether the node
	// is on Azure (nil → real probe).
	ProbeAzure func(ctx context.Context) (bool, error)
}

// vendorSignature maps a DMI sys_vendor substring to a provider name.
// Matching is case-insensitive substring, one entry per provider, first
// match wins in map-iteration-free order below.
type vendorSignature struct {
	contains string
	provider string
}

// vendorSignatures is the ordered detection table. "Amazon" matches
// "Amazon EC2"; "Microsoft" matches both "Microsoft Corporation" (Azure
// VMs) and "Microsoft Corporation Virtual Machine".
var vendorSignatures = []vendorSignature{
	{"amazon", AWSName},
	{"microsoft", AzureName},
	{"google", "gcp"}, // future provider; harmless if unregistered
}

// Detect resolves the provider name using the precedence chain.
func Detect(ctx context.Context, opts Options) (Result, error) {
	// 1. Command-line flag — the operator said so.
	if opts.Flag != "" {
		return Result{Provider: strings.ToLower(opts.Flag), Via: "flag", Detail: "-cloud flag"}, nil
	}
	// 2. Environment — the user-data wrapper can pin it.
	if opts.EnvName != "" {
		return Result{Provider: strings.ToLower(opts.EnvName), Via: "env", Detail: "CHINIT_CLOUD"}, nil
	}
	// 3. DMI vendor string — reliable, no network, works pre-IMDS.
	if opts.DMIVendorFunc == nil {
		opts.DMIVendorFunc = readDMIVendor
	}
	if vendor, err := opts.DMIVendorFunc(); err == nil && vendor != "" {
		lower := strings.ToLower(vendor)
		for _, sig := range vendorSignatures {
			if strings.Contains(lower, sig.contains) {
				return Result{Provider: sig.provider, Via: "dmi", Detail: "sys_vendor: " + vendor}, nil
			}
		}
	}
	// 4. IMDS probing — the fallback for hosts with a generic DMI
	// (some hypervisors and bare-metal cloud images).
	if opts.ProbeAWS == nil {
		opts.ProbeAWS = probeAWSIMDS
	}
	if opts.ProbeAzure == nil {
		opts.ProbeAzure = probeAzureIMDS
	}
	if ok, err := opts.ProbeAWS(ctx); err == nil && ok {
		return Result{Provider: AWSName, Via: "imds", Detail: "EC2 IMDS answered"}, nil
	}
	if ok, err := opts.ProbeAzure(ctx); err == nil && ok {
		return Result{Provider: AzureName, Via: "imds", Detail: "Azure IMDS answered"}, nil
	}
	return Result{}, fmt.Errorf("could not detect cloud provider: no -cloud flag, no CHINIT_CLOUD env, no DMI signature, no IMDS answer")
}

// readDMIVendor reads the DMI system vendor. Empty string on missing
// file (containers, some VM images) — not an error, detection continues.
func readDMIVendor() (string, error) {
	data, err := os.ReadFile("/sys/class/dmi/id/sys_vendor")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// EnvDetection reads CHINIT_CLOUD for the Options (kept here so the app
// layer has one place that touches the environment).
func EnvDetection() string {
	return strings.TrimSpace(os.Getenv("CHINIT_CLOUD"))
}
