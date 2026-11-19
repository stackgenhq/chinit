# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ClickHouse Garbage Collector for Orphaned Azure Blobs
#
# This script removes "orphaned" blobs in Azure Storage that are no longer
# referenced by ClickHouse metadata. This prevents "Ghost Costs" where
# you pay for storage that ClickHouse doesn't know exists.
#
# Usage: ./orphaned-data-gc.sh [--dry-run]

set -e

CLICKHOUSE_CLIENT_OPTS="--config-file=/etc/clickhouse-server/config.xml"
DRY_RUN=false
REMOVE_BATCH_SIZE=1000

if [[ "$1" == "--dry-run" ]]; then
    DRY_RUN=true
    echo "Running in DRY RUN mode. No files will be deleted."
fi

echo "Starting Garbage Collection for Orphaned Objects..."

# Check if clickhouse-disk tool is available (part of clickhouse-client)
if ! command -v clickhouse-disk &> /dev/null; then
    echo "Error: clickhouse-disk tool not found."
    exit 1
fi

# Run GC for the azure_blob_disk
# This command lists files in storage that are not in metadata
echo "Scanning for orphaned files (this may take a while)..."

# Note: The disk name 'azure_blob_disk' must match config.xml
DISK_NAME="azure_blob_disk"

if $DRY_RUN; then
    clickhouse-disk --config-file /etc/clickhouse-server/config.xml remove-unexpected-files --disk "$DISK_NAME" --dry-run
else
    # Run slightly nicer with low priority if possible, but clickhouse-disk is standalone
    clickhouse-disk --config-file /etc/clickhouse-server/config.xml remove-unexpected-files --disk "$DISK_NAME" --skip-unsupported-disks
fi

echo "Garbage Collection Completed."
