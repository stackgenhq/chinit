// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// specSyncTimeout bounds one tick: the fetch is one GET, the compare
// is local; a converge (the in-process boot --refresh) can legitimately
// take minutes (bundle download + render + service restart + health
// gate) — the timer's next fire owns the retry on any timeout.
const specSyncTimeout = 30 * time.Minute

// runSpecSync is `chinit spec-sync`: fetch the role spec, compare,
// converge on drift.
func runSpecSync(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), specSyncTimeout)
	defer cancel()

	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return fmt.Errorf("spec-sync: %w", err)
	}
	specs := port.AsRoleSpecSource(provider)
	locker := port.AsRefreshLock(provider)
	if specs == nil {
		// A capability gap is a logged no-op (the Azure seam lands with
		// its adapter — same degradation the autoscale job draws), never
		// a timer-failing error the fleet would page on.
		logger.GetLogger(ctx).Info("spec-sync: provider lacks the role-spec capability; skipping",
			"cloud", provider.Name())
		return nil
	}
	bundle, err := specSyncBundleOf()
	if err != nil {
		logger.GetLogger(ctx).Warn("spec-sync: cannot resolve the bundle location; skipping", "err", err)
		return nil
	}
	spec, exists, err := specs.FetchRoleSpec(ctx, port.RoleSpecRequest{
		Bucket: bundle.Bucket,
		Prefix: bundle.Prefix,
		Role:   string(st.Identity.Role),
	})
	if err != nil {
		return fmt.Errorf("spec-sync: %w", err)
	}
	if !exists {
		logger.GetLogger(ctx).Info("spec-sync: the bundle carries no spec for this role; skipping",
			"role", st.Identity.Role)
		return nil
	}
	specPath := defaultSpecSyncPath()
	current, err := os.ReadFile(specPath)
	if err == nil && bytesEqual(current, spec) {
		logger.GetLogger(ctx).Info("spec-sync: role spec unchanged", "role", st.Identity.Role)
		return nil
	}
	if ok, release, err := acquireSpecSyncLock(ctx, locker, st); err != nil {
		return err
	} else if !ok {
		return nil
	} else if release != nil {
		defer release()
	}
	return specSyncConvergeFn(ctx, st, specPath, spec)
}

// acquireSpecSyncLock takes the per-role refresh lock the restart-
// bearing converge serializes on. The bool answers "this node owns the
// converge this tick" (false = a peer holds it, the timer retries);
// the release func frees the row on the holder's exit (nil when no
// lock applied — the capability-absent and no-table degradations
// converge unlocked, loudly).
func acquireSpecSyncLock(ctx context.Context, locker port.RefreshLock, st *nodeState) (bool, func(), error) {
	if locker == nil {
		logger.GetLogger(ctx).Warn("spec-sync: provider lacks the refresh-lock capability; converging WITHOUT the per-role lock")
		return true, nil, nil
	}
	table := cooldownTableOf(st)
	if table == "" {
		logger.GetLogger(ctx).Warn("spec-sync: no claims table in the state or spec; converging WITHOUT the per-role lock (nodes of one role may restart together)")
		return true, nil, nil
	}
	lockReq := port.RefreshLockRequest{
		Table:   table,
		Key:     "refresh-" + string(st.Identity.Role),
		Holder:  st.Identity.Hostname,
		Minutes: 30,
	}
	ok, err := locker.AcquireRefreshLock(ctx, lockReq)
	if err != nil {
		return false, nil, fmt.Errorf("spec-sync: %w", err)
	}
	if !ok {
		logger.GetLogger(ctx).Info("spec-sync: a peer holds the per-role refresh lock; retrying next tick",
			"role", st.Identity.Role)
		return false, nil, nil
	}
	release := func() {
		if rerr := locker.ReleaseRefreshLock(ctx, lockReq); rerr != nil {
			logger.GetLogger(ctx).Warn("spec-sync: lock release failed; the TTL reaps the row", "err", rerr)
		}
	}
	return true, release, nil
}

// specSyncConvergeFn is the converge seam: production binds
// specSyncConverge; the specs substitute a recorder so the drift
// branches run without executing the boot pipeline (the runCommand
// var pattern the app layer uses throughout).
var specSyncConvergeFn = specSyncConverge

// specSyncConverge installs the new spec (the old one backed up
// alongside) and re-runs the boot pipeline in-process with --refresh.
// A failed converge restores the last-known-good spec — the same
// discipline the bash refresh script drew (the next tick re-attempts
// from the OLD baseline, never piles onto a broken one).
func specSyncConverge(ctx context.Context, st *nodeState, specPath string, spec []byte) error {
	var backup string
	if old, err := os.ReadFile(specPath); err == nil {
		backup = specPath + ".bak-" + time.Now().Format("20060102T150405")
		if err := os.WriteFile(backup, old, 0644); err != nil {
			return fmt.Errorf("spec-sync: back up the current spec: %w", err)
		}
	}
	if err := os.WriteFile(specPath, spec, 0644); err != nil {
		return fmt.Errorf("spec-sync: install the new spec: %w", err)
	}
	logger.GetLogger(ctx).Info("spec-sync: role spec changed — converging in-place (boot --refresh)",
		"role", st.Identity.Role,
		"spec_path", specPath,
		"backup", backup)

	// The converge is the boot pipeline over the NEW spec: fetch bundle
	// (SHA-gated), re-render, restart the service. The subcommand
	// inherits the exit-code contract (a failed converge exits nonzero;
	// the restoration below keeps the next tick's baseline sound).
	if err := runBoot([]string{"-spec", specPath, "-cloud", providerNameOf(st), "-refresh", "-state", service.ChinitStatePath}); err != nil {
		if backup != "" {
			if rerr := os.WriteFile(specPath, mustReadFile(backup), 0644); rerr != nil {
				logger.GetLogger(ctx).Error("spec-sync: could NOT restore the last-known-good spec after a failed refresh; the on-disk spec is the FAILED version",
					"backup", backup, "err", rerr)
			} else {
				logger.GetLogger(ctx).Warn("spec-sync: refresh failed — restored the last-known-good spec; retrying next tick",
					"backup", backup)
			}
		}
		return fmt.Errorf("spec-sync: the in-place refresh failed: %w", err)
	}
	logger.GetLogger(ctx).Info("spec-sync: converged")
	return nil
}

// bytesEqual is the drift check (a plain compare; the engine's own
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mustReadFile is the restore path's read (the backup was just written
// by this process; an absent backup at restore time is a filesystem
// failure worth the panic trace).
func mustReadFile(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("spec-sync: read the backup %s: %v", path, err))
	}
	return data
}

// defaultSpecSyncPath is the on-disk role spec the boot converged to
// (the same path the user-data wrapper seeded and the bash refresh
// script compared — one canonical location). A package-level var so
// the specs redirect it off /var/lib.
var defaultSpecSyncPath = func() string { return "/var/lib/clickhouse-bootstrap/chinit.yaml" }

// specSyncBundleOf reads the bundle location from the node's own spec
// (the config_bundle section names the bucket/prefix every member of
// the fleet — including this job — fetches from).
func specSyncBundleOf() (bundleLocation, error) {
	spec, err := loadInventorySpec()
	if err != nil {
		return bundleLocation{}, err
	}
	if spec == nil {
		return bundleLocation{}, fmt.Errorf("no on-disk chinit.yaml to read the bundle location from")
	}
	if spec.ConfigBundle.Bucket == "" {
		return bundleLocation{}, fmt.Errorf("the on-disk spec carries no config_bundle.bucket")
	}
	return bundleLocation{Bucket: spec.ConfigBundle.Bucket, Prefix: spec.ConfigBundle.Prefix}, nil
}

// bundleLocation is the spec's config_bundle projection.
type bundleLocation struct {
	Bucket string
	Prefix string
}
