# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-disk.sh - ClickHouse Disk and Storage Health Monitor
# ============================================================================
# Description: Monitors disk usage, storage policies, and Azure Blob Storage
#              health for ClickHouse. Essential for capacity planning.
#
# Usage: ch-disk.sh [command] [options]
#
# Commands:
#   status       Show disk status overview (default)
#   tables       Show table sizes
#   partitions   Show partition information
#   parts        Show parts information
#   blob         Show Azure Blob Storage status
#   cleanup      Show cleanup recommendations
#
# Options:
#   --limit N    Limit number of results (default: 20)
#   --database   Filter by database
#
# Author: SRE Team
# Version: 1.0.0
# ============================================================================

set -euo pipefail

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/ch-common.sh"

# Defaults
COMMAND="status"
LIMIT=20
DATABASE=""

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        status|tables|partitions|parts|blob|cleanup)
            COMMAND=$1
            shift
            ;;
        --limit)
            LIMIT=$2
            shift 2
            ;;
        --database)
            DATABASE=$2
            shift 2
            ;;
        -h|--help)
            echo "ch-disk.sh - ClickHouse Disk and Storage Monitor"
            echo ""
            echo "Usage: ch-disk.sh [command] [options]"
            echo ""
            echo "Commands:"
            echo "  status       Show disk status overview (default)"
            echo "  tables       Show table sizes"
            echo "  partitions   Show partition information"
            echo "  parts        Show parts information"
            echo "  blob         Show Azure Blob Storage status"
            echo "  cleanup      Show cleanup recommendations"
            echo ""
            echo "Options:"
            echo "  --limit N    Limit results (default: 20)"
            echo "  --database   Filter by database"
            echo "  -h, --help   Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# ============================================================================
# Command Functions
# ============================================================================

show_status() {
    print_header "ClickHouse Disk & Storage Status"
    
    # System disk info
    print_section "System Disk Usage"
    df -h /var/lib/clickhouse /var/log/clickhouse-server 2>/dev/null | head -5
    echo ""
    
    # ClickHouse disks
    print_section "ClickHouse Disks"
    run_query "
        SELECT 
            name,
            type,
            path,
            formatReadableSize(free_space) as free,
            formatReadableSize(total_space) as total,
            round(100 * (1 - free_space / total_space), 1) as used_percent,
            formatReadableSize(unreserved_space) as unreserved,
            cache_path
        FROM system.disks
        FORMAT Vertical
    "
    echo ""
    
    # Storage policies
    print_section "Storage Policies"
    run_query "
        SELECT 
            policy_name,
            volume_name,
            disks,
            max_data_part_size,
            move_factor,
            prefer_not_to_merge
        FROM system.storage_policies
        FORMAT Vertical
    "
    echo ""
    
    # Total data size
    print_section "Total Data Summary"
    run_query "
        SELECT 
            formatReadableSize(sum(bytes_on_disk)) as total_size,
            formatReadableQuantity(sum(rows)) as total_rows,
            count() as total_parts,
            countDistinct(database) as databases,
            countDistinct(table) as tables
        FROM system.parts
        WHERE active
    "
}

show_tables() {
    print_header "Table Sizes (Top $LIMIT)"
    
    local db_filter=""
    if [ -n "$DATABASE" ]; then
        db_filter="AND database = '$DATABASE'"
    fi
    
    run_query "
        SELECT 
            database,
            table,
            engine,
            formatReadableSize(sum(bytes_on_disk)) as size,
            formatReadableQuantity(sum(rows)) as rows,
            count() as parts,
            countDistinct(partition) as partitions,
            formatReadableSize(sum(primary_key_bytes_in_memory)) as pk_memory
        FROM system.parts
        WHERE active $db_filter
        GROUP BY database, table, engine
        ORDER BY sum(bytes_on_disk) DESC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
    echo ""
    
    # Show compression ratios
    print_section "Compression Ratios"
    run_query "
        SELECT 
            database,
            table,
            formatReadableSize(sum(data_compressed_bytes)) as compressed,
            formatReadableSize(sum(data_uncompressed_bytes)) as uncompressed,
            round(sum(data_uncompressed_bytes) / sum(data_compressed_bytes), 2) as ratio
        FROM system.columns
        WHERE database != 'system' $db_filter
        GROUP BY database, table
        HAVING sum(data_compressed_bytes) > 0
        ORDER BY sum(data_compressed_bytes) DESC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
}

show_partitions() {
    print_header "Partition Information"
    
    local db_filter=""
    if [ -n "$DATABASE" ]; then
        db_filter="AND database = '$DATABASE'"
    fi
    
    # Tables with many partitions
    print_section "Tables with Most Partitions"
    run_query "
        SELECT 
            database,
            table,
            countDistinct(partition) as partitions,
            count() as parts,
            formatReadableSize(sum(bytes_on_disk)) as size
        FROM system.parts
        WHERE active $db_filter
        GROUP BY database, table
        ORDER BY partitions DESC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
    echo ""
    
    # Partition details for large tables
    print_section "Partition Details (Large Tables)"
    run_query "
        SELECT 
            database,
            table,
            partition,
            count() as parts,
            formatReadableSize(sum(bytes_on_disk)) as size,
            min(min_date) as min_date,
            max(max_date) as max_date
        FROM system.parts
        WHERE active $db_filter
        GROUP BY database, table, partition
        HAVING sum(bytes_on_disk) > 100000000  -- > 100MB
        ORDER BY sum(bytes_on_disk) DESC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
}

show_parts() {
    print_header "Parts Information"
    
    local db_filter=""
    if [ -n "$DATABASE" ]; then
        db_filter="AND database = '$DATABASE'"
    fi
    
    # Parts summary
    print_section "Parts Summary"
    run_query "
        SELECT 
            count() as total_parts,
            countIf(active) as active_parts,
            countIf(NOT active) as inactive_parts,
            formatReadableSize(sumIf(bytes_on_disk, active)) as active_size,
            formatReadableSize(sumIf(bytes_on_disk, NOT active)) as inactive_size
        FROM system.parts
        WHERE 1=1 $db_filter
    "
    echo ""
    
    # Tables with many parts (potential merge candidates)
    print_section "Tables with Many Parts (Merge Candidates)"
    run_query "
        SELECT 
            database,
            table,
            partition,
            count() as parts,
            formatReadableSize(sum(bytes_on_disk)) as size,
            min(modification_time) as oldest_part,
            max(modification_time) as newest_part
        FROM system.parts
        WHERE active $db_filter
        GROUP BY database, table, partition
        HAVING count() > 10
        ORDER BY count() DESC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
    echo ""
    
    # Detached parts
    print_section "Detached Parts"
    local detached
    detached=$(run_query "SELECT count() FROM system.detached_parts WHERE 1=1 $db_filter")
    
    if [ "$detached" -gt 0 ]; then
        run_query "
            SELECT 
                database,
                table,
                partition_id,
                name,
                reason
            FROM system.detached_parts
            WHERE 1=1 $db_filter
            LIMIT $LIMIT
            FORMAT PrettyCompact
        "
    else
        echo -e "  ${GREEN}No detached parts${NC}"
    fi
}

show_blob() {
    print_header "Azure Blob Storage Status"
    
    # Check if blob storage is configured
    local blob_disks
    blob_disks=$(run_query "SELECT name, path FROM system.disks WHERE type = 'azure_blob_storage' OR type = 's3'")
    
    if [ -n "$blob_disks" ]; then
        print_section "Blob Storage Disks"
        run_query "
            SELECT 
                name,
                type,
                path,
                formatReadableSize(free_space) as free,
                formatReadableSize(total_space) as total,
                cache_path
            FROM system.disks
            WHERE type IN ('azure_blob_storage', 's3')
            FORMAT Vertical
        "
        echo ""
        
        # Data on blob storage
        print_section "Data on Blob Storage"
        run_query "
            SELECT 
                disk_name,
                database,
                table,
                formatReadableSize(sum(bytes_on_disk)) as size,
                count() as parts
            FROM system.parts
            WHERE active AND disk_name IN (
                SELECT name FROM system.disks 
                WHERE type IN ('azure_blob_storage', 's3')
            )
            GROUP BY disk_name, database, table
            ORDER BY sum(bytes_on_disk) DESC
            LIMIT $LIMIT
            FORMAT PrettyCompact
        "
    else
        echo -e "  ${YELLOW}No Azure Blob Storage or S3 disks configured${NC}"
    fi
    echo ""
    
    # Check storage configuration
    print_section "Storage Configuration"
    if [ -f /etc/clickhouse-server/config.d/storage-config.xml ]; then
        echo "Storage config found at /etc/clickhouse-server/config.d/storage-config.xml"
        # Show relevant parts (mask any keys)
        grep -E "(storage|disk|volume|policy)" /etc/clickhouse-server/config.d/storage-config.xml 2>/dev/null | head -30 || true
    else
        echo "  Using default storage configuration"
    fi
}

show_cleanup() {
    print_header "Cleanup Recommendations"
    
    # Inactive parts that can be dropped
    print_section "Inactive Parts (Can be GC'd)"
    run_query "
        SELECT 
            database,
            table,
            count() as inactive_parts,
            formatReadableSize(sum(bytes_on_disk)) as reclaimable_size
        FROM system.parts
        WHERE NOT active AND modification_time < now() - INTERVAL 1 DAY
        GROUP BY database, table
        HAVING count() > 10
        ORDER BY sum(bytes_on_disk) DESC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
    echo ""
    
    # Tables with excessive parts (need optimization)
    print_section "Tables Needing Optimization"
    run_query "
        SELECT 
            database,
            table,
            partition,
            count() as parts,
            formatReadableSize(sum(bytes_on_disk)) as size,
            'OPTIMIZE TABLE ' || database || '.' || table || ' PARTITION ''' || partition || ''' FINAL' as suggestion
        FROM system.parts
        WHERE active AND database != 'system'
        GROUP BY database, table, partition
        HAVING count() > 50
        ORDER BY count() DESC
        LIMIT 10
        FORMAT PrettyCompact
    "
    echo ""
    
    # Old partitions that might be dropped
    print_section "Old Partitions (Review for Retention)"
    run_query "
        SELECT 
            database,
            table,
            partition,
            min(min_date) as oldest_data,
            max(max_date) as newest_data,
            formatReadableSize(sum(bytes_on_disk)) as size,
            count() as parts
        FROM system.parts
        WHERE active AND database != 'system'
        GROUP BY database, table, partition
        HAVING max(max_date) < today() - 90  -- Older than 90 days
        ORDER BY min(min_date) ASC
        LIMIT $LIMIT
        FORMAT PrettyCompact
    "
    echo ""
    
    # Mutations that are stuck
    print_section "Stuck Mutations"
    local stuck
    stuck=$(run_query "SELECT count() FROM system.mutations WHERE is_done = 0 AND create_time < now() - INTERVAL 1 HOUR")
    
    if [ "$stuck" -gt 0 ]; then
        run_query "
            SELECT 
                database,
                table,
                mutation_id,
                command,
                create_time,
                parts_to_do,
                'KILL MUTATION WHERE mutation_id = ''' || mutation_id || '''' as kill_cmd
            FROM system.mutations
            WHERE is_done = 0 AND create_time < now() - INTERVAL 1 HOUR
            LIMIT 10
            FORMAT Vertical
        "
    else
        echo -e "  ${GREEN}No stuck mutations${NC}"
    fi
    echo ""
    
    echo -e "${YELLOW}Note: Review recommendations carefully before executing any cleanup commands.${NC}"
}

# ============================================================================
# Main
# ============================================================================

main() {
    case $COMMAND in
        status)
            show_status
            ;;
        tables)
            show_tables
            ;;
        partitions)
            show_partitions
            ;;
        parts)
            show_parts
            ;;
        blob)
            show_blob
            ;;
        cleanup)
            show_cleanup
            ;;
        *)
            echo "Unknown command: $COMMAND"
            exit 1
            ;;
    esac
}

main
