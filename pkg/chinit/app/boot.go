// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/aws"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/azure"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/detect"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// registry is the provider factory. Adding a cloud is one constructor
// here plus one infra package — nothing in the pipeline changes.
func registry() port.Registry {
	r := port.Registry{}
	r.Register(detect.AWSName, func(cfg port.Config) port.Provider { return aws.New(cfg) })
	r.Register(detect.AzureName, func(cfg port.Config) port.Provider { return azure.New(cfg) })
	return r
}

// runBoot is the `chinit boot` entry: load spec, detect cloud, build
// the provider, probe metadata, run the pipeline.
func runBoot(args []string) error {
	flags, err := parseBootFlags(args)
	if err != nil {
		return err
	}

	spec, err := loadSpec(flags.specPath)
	if err != nil {
		return fail.Fatal(err)
	}

	ctx := context.Background()

	// Detect the cloud (flag > env > DMI > IMDS) — the spec's cloud
	// field is an override at the same precedence as the flag.
	detected, err := detect.Detect(ctx, detect.Options{
		Flag:    firstNonEmpty(flags.cloud, spec.Cloud),
		EnvName: detect.EnvDetection(),
	})
	if err != nil {
		return fail.Fatal(fmt.Errorf("cloud detection: %w", err))
	}
	logger.GetLogger(ctx).Info("Detected cloud",
		"cloud", detected.Provider,
		"via", detected.Via,
		"detail", detected.Detail)

	// Build the provider. In dry-run the metadata probe is optional: the
	// plan prints without it (a dev machine has no IMDS); a real boot
	// requires it — the pipeline's region and identity derive from it.
	provider, err := registry().Build(detected.Provider, port.Config{
		Spec: spec,
	})
	if err != nil {
		return fail.Fatal(err)
	}
	meta, err := provider.Metadata(ctx)
	if err != nil {
		if !flags.dryRun {
			return fail.Fatal(fmt.Errorf("instance metadata: %w", err))
		}
		logger.GetLogger(ctx).Warn("NOTE: metadata unavailable — printing the plan with an empty identity", "err", err)
		meta = port.InstanceMetadata{Provider: detected.Provider}
	}
	if spec.Region == "" {
		spec.Region = meta.Region
	}

	// Telemetry init from the boot's own inputs (after metadata so the
	// resource carries the instance id — the hostname is not settled
	// yet, the claim step below owns it). Empty log_shipping endpoint
	// disables the whole package: every emit below no-ops.
	initBootTelemetry(ctx, spec, meta, detected.Provider)

	// The boot pipeline. The resume key covers BOTH the bundle hash
	// (what the nodes converge to) and a digest of the spec file itself
	// (how they converge): a spec-only change — users.d networks, tag set,
	// listener addresses — used to leave the node un-converged because
	// the bundle hash never changed.
	specHash, err := specFingerprint(flags.specPath, spec.ConfigBundle.SHA256)
	if err != nil {
		return fail.Fatal(err)
	}
	boot := &service.Boot{
		Spec:      spec,
		Provider:  provider,
		Meta:      meta,
		SpecHash:  specHash,
		Refresh:   flags.refresh,
		StatePath: flags.statePath,
	}
	steps, err := boot.Steps(ctx)
	if err != nil {
		return fail.Fatal(err)
	}

	if flags.dryRun {
		fmt.Println("Dry-run: the pipeline would run these steps in order:")
		for _, st := range steps {
			if group, ok := st.(service.ParallelGroup); ok {
				fmt.Printf("  %-24s [%s] (children run concurrently)\n", st.Name(), st.Criticality())
				for _, child := range group.Children {
					fmt.Printf("    %-22s [%s]\n", child.Name(), child.Criticality())
				}
				continue
			}
			fmt.Printf("  %-24s [%s]\n", st.Name(), st.Criticality())
		}
		return nil
	}

	store, err := service.NewFileStateStore(flags.statePath)
	if err != nil {
		return fail.Fatal(err)
	}
	engine := service.NewEngine(nil, store)
	// The spec hash keys resume; the identity persister writes the
	// settled identity the moment the claim/derivation finishes.
	// Refresh mode: every step re-executes; the identity comes from
	// the state file (never re-claimed) — see Boot.Refresh.
	engine = engine.WithIdentity(nil, detected.Provider, specHash).WithRefresh(flags.refresh)
	boot.PersistIdentity = engine.IdentityPersister(detected.Provider)
	if !flags.refresh {
		boot.HydrateIdentityFromState(flags.statePath)
	}
	res := engine.Run(ctx, steps)

	service.FormatTimingSummary(os.Stdout, res)
	service.DescribeFailures(os.Stderr, res)

	// The pipeline's verdict and per-step timings ship to the OTLP
	// endpoint when telemetry is enabled (disabled = no-op): the
	// step-duration histogram is what a slow-step alert keys on, and
	// the outcome counter distinguishes fresh boots from refreshes.
	emitBootTelemetry(ctx, res, flags.refresh)

	if res.Fatality != nil {
		return &exitCodeError{Code: exitBootFailed, Err: res.Fatality}
	}
	return nil
}

// specFingerprint folds the spec FILE into the resume key. The bundle
// hash alone does not cover spec edits: the spec is read before the
// bundle is fetched, so a spec-only change (e.g. a network CIDR in
// users.d) left the recorded hash untouched and every boot stepped
// over the already-"done" render steps. Hashing the raw file alongside
// the bundle hash makes ANY spec change invalidate resume.
func specFingerprint(specPath, bundleSHA string) (string, error) {
	data, err := os.ReadFile(specPath)
	if err != nil {
		return "", fmt.Errorf("read spec for fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	if bundleSHA == "" {
		return "spec:" + hex.EncodeToString(sum[:]), nil
	}
	return bundleSHA + "+spec:" + hex.EncodeToString(sum[:]), nil
}

// loadSpec reads and validates chinit.yaml.
func loadSpec(path string) (*model.BootSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (the cloud-init user-data wrapper must fetch the bundle, or pass -spec)", path, err)
	}
	var spec model.BootSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
