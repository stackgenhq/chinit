// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// StorageLossVerdict is the detector's answer.
type StorageLossVerdict struct {
	// StorageLoss is true when the node's condition matches: a data
	// directory that carries no parts while its replica name holds
	// Keeper registrations.
	StorageLoss bool
	// DataDir is the inspected directory (diagnostics).
	DataDir string
	// Reason carries the evidence (the journal line; empty when no
	// loss).
	Reason string
}

// DetectStorageLoss answers the boot's storage-loss check. Pure over
// the inputs (the caller gathers the facts; the detector never touches
// ClickHouse — the DDL pass's Code 253 conflict-retry owns the actual
// drop-then-recreate, and the replica-sync timer owns the catch-up).
//
// The facts:
//   - dataDir: the mounted data directory (the metadata-disk store
//     path); the walk counts ClickHouse part directories ("data_*"
//     under any table path) — zero parts + zero detached = empty;
//   - registeredReplicas: the node's OWN replica names found in
//     system.replicas (gathered by the caller over the local port,
//     BEFORE the schema pass would create anything).
//
// The verdict: StorageLoss = an empty data dir AND existing
// registrations — the re-image/replacement case the operator handles
// via its migration path. An empty dir with NO registrations is a
// fresh node (the normal first boot); a populated dir with
// registrations is the healthy case; a populated dir with NO
// registrations is the re-attach case (the replica-sync/restore path
// heals it, not this detector).
func DetectStorageLoss(dataDir string, registeredReplicas []string) (StorageLossVerdict, error) {
	if dataDir == "" {
		return StorageLossVerdict{}, fmt.Errorf("storage-loss detection requires a data directory")
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		// The mount step owns the directory; an unstatable path is a
		// mount bug, not a storage-loss verdict.
		return StorageLossVerdict{}, fmt.Errorf("stat %s: %w", dataDir, err)
	}
	if !info.IsDir() {
		return StorageLossVerdict{}, fmt.Errorf("%s is not a directory", dataDir)
	}
	hasParts, err := dirHasParts(dataDir)
	if err != nil {
		return StorageLossVerdict{}, fmt.Errorf("walk %s: %w", dataDir, err)
	}
	v := StorageLossVerdict{DataDir: dataDir}
	switch {
	case hasParts:
		v.Reason = "data directory carries parts (healthy or re-attach; the heal paths own it)"
		return v, nil
	case len(registeredReplicas) == 0:
		v.Reason = "empty data directory with no Keeper registrations (a fresh node's first boot)"
		return v, nil
	default:
		v.StorageLoss = true
		v.Reason = fmt.Sprintf("empty data directory (%s) while Keeper holds %d replica registrations for this name — storage loss; the schema pass's conflict-retry drops the stale registrations and re-creates",
			dataDir, len(registeredReplicas))
		return v, nil
	}
}

// dirHasParts reports whether the data directory carries any ClickHouse
// part directory. The walk is bounded at depth 3 (store/db/table/data_part)
// and stops at the first hit (the boot's one call over a fresh volume is
// cheap; a healthy volume answers on its first child).
func dirHasParts(dataDir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable child (a torn store mid-mount) must not
			// wedge the boot: skip the subtree, keep walking.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if isPartDir(d.Name()) {
				found = true
				return filepath.SkipAll
			}
			// Parts live at store/db/table/part (depth 4): skip nothing
			// above depth 3 (the bound prunes deep nested stores, not
			// the standard layout).
			rel, rerr := filepath.Rel(dataDir, path)
			if rerr == nil && depthOf(rel) > 3 {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if found {
		return true, nil
	}
	return false, err
}

// isPartDir answers whether a directory name is a ClickHouse part
// (the "data_" / "data_part" encodings the store writes).
func isPartDir(name string) bool {
	return len(name) > 5 && name[:5] == "data_"
}

// depthOf counts path separators ("" = depth 0).
func depthOf(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	n := 0
	for _, c := range rel {
		if c == os.PathSeparator {
			n++
		}
	}
	return n + 1
}

// ClickHouseDataDir is the server's store root (the metadata-disk
// mount; keepers use their own coordination directories and never
// reach this step).
const ClickHouseDataDir = "/var/lib/clickhouse"

// detectStorageLoss is the boot step: gather the two facts (the data
// directory's part presence; the node's OWN replica registrations) and
// record the verdict. BestEffort by design — the verdict's remedy is
// the DDL pass's conflict-retry plus the replica-sync timer, both of
// which follow the step; this step only makes the condition VISIBLE
// (the journal line the operator greps when a replacement boots slow).
func (b *Boot) detectStorageLoss(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil // keepers have no replicated tables
	}
	log := logger.GetLogger(ctx)
	// The registration read runs BEFORE the schema pass would create
	// anything: an empty system.replicas on a FRESH node answers the
	// fresh-boot verdict; a populated one on an empty disk answers the
	// storage-loss verdict.
	pwBytes, perr := os.ReadFile(bootstrapAdminPasswordPath())
	if perr != nil {
		log.Warn("storage-loss: admin password unavailable (no verdict this boot)", "err", perr.Error())
		return nil
	}
	out, err := execClient(ctx, strings.TrimSpace(string(pwBytes)), "SELECT table FROM system.replicas FORMAT TSV")
	if err != nil {
		// A server that just started may not answer yet — the DDL pass
		// and the health gate own the startup convergence. No verdict.
		log.Warn("storage-loss: replica listing unavailable (no verdict this boot)",
			"err", strings.TrimSpace(out))
		return nil
	}
	var registered []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			registered = append(registered, line)
		}
	}
	verdict, err := DetectStorageLoss(ClickHouseDataDir, registered)
	if err != nil {
		log.Warn("storage-loss: detection failed", "err", err.Error())
		return nil
	}
	if verdict.StorageLoss {
		log.Warn("storage-loss: DETECTED — the schema pass's conflict-retry will drop the stale registrations and re-create; replica-sync converges the catch-up",
			"data_dir", verdict.DataDir,
			"registrations", len(registered),
			"reason", verdict.Reason)
	} else {
		log.Info("storage-loss: none", "reason", verdict.Reason)
	}
	return nil
}
