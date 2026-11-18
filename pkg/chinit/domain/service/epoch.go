// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// PersistentEpoch captures the S3 bucket and storage-mode epoch that owns the
// local ClickHouse replica metadata on a persistent EBS volume.
type PersistentEpoch struct {
	S3DataBucket string `json:"s3_data_bucket"`
	CasEnabled   bool   `json:"cas_enabled"`
}

// ClickHouseDataStore abstracts local ClickHouse metadata/cache persistence so
// domain reconcilers stay decoupled from raw filesystem paths.
type ClickHouseDataStore interface {
	ReadEpoch() (PersistentEpoch, bool, error)
	WriteEpoch(epoch PersistentEpoch) error
	HasLegacyS3Store() bool
	ResetReplicaMetadata() error
}

// osClickHouseDataStore implements ClickHouseDataStore on the local filesystem.
type osClickHouseDataStore struct {
	root string
}

func newOSClickHouseDataStore() *osClickHouseDataStore {
	return &osClickHouseDataStore{root: clickhouseDataDir()}
}

func (s *osClickHouseDataStore) epochPath() string {
	return filepath.Join(s.root, "boot_epoch.json")
}

func (s *osClickHouseDataStore) ReadEpoch() (PersistentEpoch, bool, error) {
	raw, err := os.ReadFile(s.epochPath())
	if errors.Is(err, os.ErrNotExist) {
		return PersistentEpoch{}, false, nil
	}
	if err != nil {
		return PersistentEpoch{}, false, err
	}
	var epoch PersistentEpoch
	if err := json.Unmarshal(raw, &epoch); err != nil {
		return PersistentEpoch{}, false, err
	}
	return epoch, true, nil
}

func (s *osClickHouseDataStore) WriteEpoch(epoch PersistentEpoch) error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(epoch)
	if err != nil {
		return err
	}
	return os.WriteFile(s.epochPath(), append(data, '\n'), 0o644)
}

func (s *osClickHouseDataStore) HasLegacyS3Store() bool {
	entries, err := os.ReadDir(filepath.Join(s.root, "disks", "s3", "store"))
	return err == nil && len(entries) > 0
}

func (s *osClickHouseDataStore) ResetReplicaMetadata() error {
	for _, sub := range []string{
		"store",
		"metadata",
		"data",
		filepath.Join("disks", "s3", "store"),
		filepath.Join("disks", "s3_cache"),
		filepath.Join("disks", "cache_default"),
		filepath.Join("disks", "cas_cache"),
		filepath.Join("disks", "cas_cache_default"),
		"first_boot_done",
	} {
		if err := os.RemoveAll(filepath.Join(s.root, sub)); err != nil {
			return err
		}
	}
	return nil
}

// PersistentEpochReconciler protects persistent EBS nodes (such as backup-0)
// from bucket switches or non-CAS -> CAS downgrades that leave stale part stubs
// pointing to a vanished bucket (Code 499) or v1 packed metadata (Code 287).
type PersistentEpochReconciler struct {
	store      ClickHouseDataStore
	stopServer func(context.Context)
}

// NewPersistentEpochReconciler constructs a reconciler over store and a stop callback.
func NewPersistentEpochReconciler(store ClickHouseDataStore, stopServer func(context.Context)) *PersistentEpochReconciler {
	return &PersistentEpochReconciler{store: store, stopServer: stopServer}
}

// Reconcile resets local replica metadata (preserving /var/lib/clickhouse/uuid)
// whenever the active S3 bucket changes or a CAS backup replica carries legacy
// disks/s3/store residue, then persists the current epoch.
func (r *PersistentEpochReconciler) Reconcile(ctx context.Context, spec *model.BootSpec) {
	if spec == nil {
		return
	}
	desired := PersistentEpoch{S3DataBucket: spec.S3DataBucket, CasEnabled: spec.CasEnabled}
	if reason := r.driftReason(spec); reason != "" {
		logger.GetLogger(ctx).Warn(
			"resetting stale local ClickHouse metadata/cache (preserving /var/lib/clickhouse/uuid)",
			"reason", reason,
		)
		if r.stopServer != nil {
			r.stopServer(ctx)
		}
		if err := r.store.ResetReplicaMetadata(); err != nil {
			logger.GetLogger(ctx).Warn("persistent metadata reset incomplete", "err", err)
		}
	}
	if err := r.store.WriteEpoch(desired); err != nil {
		logger.GetLogger(ctx).Warn("failed to persist boot_epoch.json", "err", err)
	}
}

func (r *PersistentEpochReconciler) driftReason(spec *model.BootSpec) string {
	prev, ok, err := r.store.ReadEpoch()
	if err != nil {
		return fmt.Sprintf("unreadable boot_epoch.json (%v)", err)
	}
	if ok {
		if prev.S3DataBucket != "" && spec.S3DataBucket != "" && prev.S3DataBucket != spec.S3DataBucket {
			return fmt.Sprintf("s3_data_bucket changed %s -> %s", prev.S3DataBucket, spec.S3DataBucket)
		}
		if prev.CasEnabled != spec.CasEnabled {
			return fmt.Sprintf("cas_enabled changed %t -> %t", prev.CasEnabled, spec.CasEnabled)
		}
		return ""
	}
	if spec.Role == model.RoleBackup && spec.CasEnabled && r.store.HasLegacyS3Store() {
		return "legacy non-CAS disks/s3/store residue detected on CAS-enabled backup replica"
	}
	return ""
}
