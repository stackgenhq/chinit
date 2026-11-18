// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

import "context"

// The config-drift convergence ports: the spec-sync job's cloud seams.
//
// Why a port at all: the config-refresher Lambda (the S3-event → SSM
// dispatcher this job replaces) baked the bucket/prefix/region into
// apply-time env and pushed changes AT nodes. The bundle contract the
// boot already fetches under is the one source of truth — the node
// re-reads its OWN role spec from the same prefix on a timer and
// converges to it in-place (the pull-side answer to the same
// convergence the boot's --refresh path performs).

// RoleSpecSource reads the node's role spec from the bundle prefix.
//
// Why separate from ConfigSource: FetchBundle is the boot's whole-
// bundle SHA-gated download (a mismatch is Fatal); the spec-sync job
// needs the one-key read with a benign "absent" answer (a role whose
// spec the stack never uploaded — the backup with backups disabled —
// is a no-op, not a boot-fatal).
type RoleSpecSource interface {
	// FetchRoleSpec reads <prefix>/chinit-<role>.yaml. Exists is
	// false when the key is absent (the caller skips, never errors).
	FetchRoleSpec(ctx context.Context, req RoleSpecRequest) (content []byte, exists bool, err error)
}

// RoleSpecRequest names one role-spec read.
type RoleSpecRequest struct {
	// Bucket is the bundle's bucket (the spec's config_bundle.bucket).
	Bucket string
	// Prefix is the bundle prefix ("bootstrap/chinit").
	Prefix string
	// Role is the node's role ("writer", "reader", "keeper", "backup").
	Role string
}

// RefreshLock is the per-role refresh serialization: one node of a
// role runs the restart-bearing converge at a time (the Lambda's
// DynamoDB lock, minus the Lambda — the same claims table the
// ordinals ride, under a key a hostname lease row can never produce).
type RefreshLock interface {
	// AcquireRefreshLock answers whether THIS holder took the lock.
	// False is benign (a peer holds it — the timer retries).
	AcquireRefreshLock(ctx context.Context, req RefreshLockRequest) (bool, error)
	// ReleaseRefreshLock frees the slot (the holder's exit path).
	ReleaseRefreshLock(ctx context.Context, req RefreshLockRequest) error
}

// RefreshLockRequest names one per-role refresh lock.
type RefreshLockRequest struct {
	// Table is the claims/lease table the lock rides.
	Table string
	// Key is the lock row's key ("refresh-writer").
	Key string
	// Holder is the lock holder's identity (the hostname).
	Holder string
	// Minutes bounds the lock's TTL (the dead-holder safety net —
	// the release path deletes the row; the TTL reaps a node that
	// died mid-converge).
	Minutes int
}
