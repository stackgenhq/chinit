// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// runConfigCheck is `chinit config-check` (the timer on every server
// node): compare the recorded boot fingerprint (state.spec_hash) with
// the fingerprint over freshly-fetched spec+bundle; drifted →
// `chinit boot --refresh` (the in-process re-render; restarts the
// server only when a rendered file actually changed).
func runConfigCheck(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return fmt.Errorf("config-check: %w", err)
	}
	specPath := inventorySpecPath()
	spec, err := loadBootSpec(specPath)
	if err != nil {
		return fmt.Errorf("config-check: %w", err)
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return fmt.Errorf("config-check: build provider: %w", err)
	}
	// The Provider interface carries ConfigSource (a mandatory
	// capability) — no probe needed.
	fetcher := provider
	// Fetch fresh (the SHA gate inside is the same contract as boot:
	// a mismatch is fatal, never a retry).
	tmp := os.TempDir() + "/chinit-config-check"
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return fmt.Errorf("config-check: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) // 5 min
	defer cancel()
	if _, err := fetcher.FetchBundle(ctx, spec.ConfigBundle, tmp); err != nil {
		return fmt.Errorf("config-check: bundle fetch: %w", err)
	}
	fresh, err := specFingerprint(specPath, spec.ConfigBundle.SHA256)
	if err != nil {
		return fmt.Errorf("config-check: %w", err)
	}
	if st.SpecHash == "" {
		logger.GetLogger(ctx).Warn("config-check: no recorded fingerprint; running boot --refresh to record one")
		return runBoot([]string{"-refresh", "-spec", specPath})
	}
	if fresh == st.SpecHash {
		return nil
	}
	logger.GetLogger(ctx).Warn("config-check: bundle/spec drifted — re-rendering",
		"recorded", st.SpecHash,
		"fresh", fresh)
	return runBoot([]string{"-refresh", "-spec", specPath})
}

// loadBootSpec reads the full model.BootSpec (the same parse boot
// uses — the bundle location and SHA are the config-check inputs).
func loadBootSpec(path string) (*model.BootSpec, error) {
	return loadSpec(path)
}
