// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import (
	"context"
	"time"
)

// OrphanSweeper discovers and reclaims untagged, unattached cloud
// volumes past the configured age threshold and reports unassociated
// EIPs and untagged snapshots.
type OrphanSweeper interface {
	// SweepOrphans censuses and reclaims per the request. Deletes
	// happen only for the categories the request marks (volumes);
	// report-only categories (EIPs, snapshots) answer in the result.
	SweepOrphans(ctx context.Context, req OrphanSweepRequest) (*OrphanSweepResult, error)
}

// OrphanSweepRequest bounds one sweep.
type OrphanSweepRequest struct {
	// Region scopes the census.
	Region string
	// AgeThreshold: resources younger than this stay (a
	// half-completed apply's volume must not die mid-attach).
	AgeThreshold time.Duration
	// DryRun reports without deleting.
	DryRun bool
}

// OrphanSweepResult is the census outcome (the metric-push input).
type OrphanSweepResult struct {
	// VolumesDeleted lists what was reclaimed (empty in DryRun).
	VolumesDeleted []string
	// VolumesFound lists untagged unattached candidates.
	VolumesFound []string
	// EIPsUnassociated lists report-only unassociated EIPs.
	EIPsUnassociated []string
	// SnapshotsUntagged lists report-only untagged snapshots.
	SnapshotsUntagged []string
}
