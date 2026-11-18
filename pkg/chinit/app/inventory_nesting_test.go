// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Verifies that backup_replica_hostname is parsed from the nested
// `backup:` section in chinit.yaml and mapped to its CR slot
// (writerMax + readerMax) in the host inventory.
func TestInventoryParsesNestedBackupSection(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chinit.yaml")
	rendered := []byte(`
cluster_name: demo
writer_max: 5
reader_max: 4
writer_hostname_prefix: "writer-"
reader_hostname_prefix: "reader-"
dns_zone_name: demo.clickhouse.internal
backup:
  bucket: "some-bucket"
  backup_replica_hostname: "backup-0"
  is_backup_replica: true
`)
	require.NoError(t, os.WriteFile(specPath, rendered, 0600))
	orig := inventorySpecPath
	inventorySpecPath = func() string { return specPath }
	t.Cleanup(func() { inventorySpecPath = orig })

	spec, err := loadInventorySpec()
	require.NoError(t, err)
	require.Equal(t, "backup-0", spec.backupReplicaHostname(),
		"the hostname lives under the backup: section — a flat field never parses")

	inv := hostInventory(*spec)
	require.Contains(t, inv, "01-9=backup-0.demo.clickhouse.internal",
		"the inventory must carry the backup's CR slot (writerMax+readerMax)")
}
