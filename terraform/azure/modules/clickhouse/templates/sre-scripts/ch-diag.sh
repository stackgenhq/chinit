# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-diag.sh - ClickHouse Diagnostic Information Collector
# ============================================================================
# Description: Collects comprehensive diagnostic information for 
#              troubleshooting ClickHouse issues. Creates a diagnostic 
#              bundle that can be shared with support.
#
# Usage: ch-diag.sh [--output DIR] [--include-logs] [--include-configs]
#
# Options:
#   --output DIR        Output directory (default: /tmp/ch-diag-<timestamp>)
#   --include-logs      Include recent ClickHouse logs (last 1000 lines)
#   --include-configs   Include ClickHouse configuration files
#
# Output: Creates a tarball with diagnostic information
#
# Author: SRE Team
# Version: 1.0.0
# ============================================================================

set -euo pipefail

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/ch-common.sh"

TIMESTAMP=$(date +%Y%m%d_%H%M%S)
OUTPUT_DIR="/tmp/ch-diag-${TIMESTAMP}"
INCLUDE_LOGS=false
INCLUDE_CONFIGS=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --output)
            OUTPUT_DIR=$2
            shift 2
            ;;
        --include-logs)
            INCLUDE_LOGS=true
            shift
            ;;
        --include-configs)
            INCLUDE_CONFIGS=true
            shift
            ;;
        -h|--help)
            echo "ch-diag.sh - ClickHouse Diagnostic Collector"
            echo ""
            echo "Usage: ch-diag.sh [--output DIR] [--include-logs] [--include-configs]"
            echo ""
            echo "Options:"
            echo "  --output DIR        Output directory (default: /tmp/ch-diag-<timestamp>)"
            echo "  --include-logs      Include recent logs (last 1000 lines)"
            echo "  --include-configs   Include configuration files (passwords masked)"
            echo "  -h, --help          Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# ============================================================================
# Collection Functions
# ============================================================================

collect_system_info() {
    local file="${OUTPUT_DIR}/system_info.txt"
    log_info "Collecting system information..."
    
    {
        echo "=== System Information ==="
        echo "Date: $(date)"
        echo "Hostname: $(hostname)"
        echo "Kernel: $(uname -r)"
        echo ""
        
        echo "=== OS Release ==="
        cat /etc/os-release 2>/dev/null || echo "N/A"
        echo ""
        
        echo "=== CPU Info ==="
        lscpu | head -20
        echo ""
        
        echo "=== Memory Info ==="
        free -h
        echo ""
        
        echo "=== Disk Usage ==="
        df -h
        echo ""
        
        echo "=== Mount Points ==="
        mount | grep -E "(ext4|xfs|azure|/dev/sd)"
        echo ""
        
        echo "=== Network Interfaces ==="
        ip addr 2>/dev/null || ifconfig 2>/dev/null || echo "N/A"
        echo ""
        
        echo "=== Process List (Top 20 by Memory) ==="
        ps aux --sort=-%mem | head -20
        
    } > "$file"
    
    log_success "System information collected"
}

collect_clickhouse_info() {
    local file="${OUTPUT_DIR}/clickhouse_info.txt"
    log_info "Collecting ClickHouse information..."
    
    {
        echo "=== ClickHouse Version ==="
        run_query "SELECT version()" || echo "Could not connect"
        echo ""
        
        echo "=== Server Uptime ==="
        run_query "SELECT formatReadableTimeDelta(uptime())" || echo "N/A"
        echo ""
        
        echo "=== Build Info ==="
        run_query "SELECT * FROM system.build_options LIMIT 10 FORMAT Vertical" || echo "N/A"
        echo ""
        
        echo "=== Databases ==="
        run_query "SELECT name, engine, count() as tables FROM system.databases LEFT JOIN system.tables USING (database) GROUP BY name, engine ORDER BY name FORMAT PrettyCompact" || echo "N/A"
        echo ""
        
        echo "=== Cluster Configuration ==="
        run_query "SELECT * FROM system.clusters FORMAT Vertical" || echo "N/A"
        echo ""
        
        echo "=== Macros ==="
        run_query "SELECT * FROM system.macros FORMAT PrettyCompact" || echo "N/A"
        echo ""
        
        echo "=== Settings (Non-Default) ==="
        run_query "SELECT name, value, changed FROM system.settings WHERE changed = 1 FORMAT PrettyCompact" || echo "N/A"
        
    } > "$file"
    
    log_success "ClickHouse information collected"
}

collect_metrics() {
    local file="${OUTPUT_DIR}/metrics.txt"
    log_info "Collecting metrics..."
    
    {
        echo "=== Current Metrics ==="
        run_query "SELECT metric, value, description FROM system.metrics ORDER BY metric FORMAT PrettyCompact" || echo "N/A"
        echo ""
        
        echo "=== Asynchronous Metrics ==="
        run_query "SELECT metric, value, description FROM system.asynchronous_metrics ORDER BY metric FORMAT PrettyCompact" || echo "N/A"
        echo ""
        
        echo "=== Events ==="
        run_query "SELECT event, value, description FROM system.events ORDER BY value DESC LIMIT 50 FORMAT PrettyCompact" || echo "N/A"
        
    } > "$file"
    
    log_success "Metrics collected"
}

collect_storage_info() {
    local file="${OUTPUT_DIR}/storage_info.txt"
    log_info "Collecting storage information..."
    
    {
        echo "=== Disks ==="
        run_query "SELECT * FROM system.disks FORMAT Vertical" || echo "N/A"
        echo ""
        
        echo "=== Storage Policies ==="
        run_query "SELECT * FROM system.storage_policies FORMAT Vertical" || echo "N/A"
        echo ""
        
        echo "=== Parts Summary (Top 20 Tables) ==="
        run_query "
            SELECT 
                database, table, 
                count() as parts,
                formatReadableSize(sum(bytes_on_disk)) as size,
                formatReadableQuantity(sum(rows)) as rows
            FROM system.parts 
            WHERE active 
            GROUP BY database, table 
            ORDER BY sum(bytes_on_disk) DESC 
            LIMIT 20 
            FORMAT PrettyCompact
        " || echo "N/A"
        echo ""
        
        echo "=== Parts by Partition (Large Tables) ==="
        run_query "
            SELECT 
                database, table, partition,
                count() as parts,
                formatReadableSize(sum(bytes_on_disk)) as size
            FROM system.parts 
            WHERE active AND database != 'system'
            GROUP BY database, table, partition
            HAVING count() > 10
            ORDER BY count() DESC 
            LIMIT 20 
            FORMAT PrettyCompact
        " || echo "N/A"
        
    } > "$file"
    
    log_success "Storage information collected"
}

collect_replication_info() {
    local file="${OUTPUT_DIR}/replication_info.txt"
    log_info "Collecting replication information..."
    
    {
        echo "=== Replicas Status ==="
        run_query "
            SELECT 
                database, table, replica_name, replica_path,
                is_leader, is_readonly, is_session_expired,
                future_parts, parts_to_check, 
                queue_size, inserts_in_queue, merges_in_queue,
                absolute_delay
            FROM system.replicas 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Replication Queue ==="
        run_query "
            SELECT 
                database, table, replica_name, type,
                create_time, source_replica, num_tries, last_exception
            FROM system.replication_queue 
            ORDER BY create_time 
            LIMIT 50 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Distributed DDL Queue ==="
        run_query "
            SELECT * 
            FROM system.distributed_ddl_queue 
            ORDER BY query_create_time DESC 
            LIMIT 20 
            FORMAT Vertical
        " || echo "N/A"
        
    } > "$file"
    
    log_success "Replication information collected"
}

collect_query_info() {
    local file="${OUTPUT_DIR}/query_info.txt"
    log_info "Collecting query information..."
    
    {
        echo "=== Running Queries ==="
        run_query "
            SELECT 
                query_id, user, elapsed, read_rows, memory_usage,
                substring(query, 1, 200) as query_preview
            FROM system.processes 
            WHERE is_cancelled = 0 
            ORDER BY elapsed DESC 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Slow Queries (Last Hour) ==="
        run_query "
            SELECT 
                type, query_start_time, query_duration_ms,
                read_rows, memory_usage,
                substring(query, 1, 200) as query_preview,
                exception
            FROM system.query_log 
            WHERE type IN ('QueryFinish', 'ExceptionWhileProcessing')
              AND event_time > now() - INTERVAL 1 HOUR
              AND query_duration_ms > 5000
            ORDER BY query_duration_ms DESC 
            LIMIT 20 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Query Errors (Last Hour) ==="
        run_query "
            SELECT 
                type, query_start_time, user, client_hostname,
                exception_code, exception,
                substring(query, 1, 200) as query_preview
            FROM system.query_log 
            WHERE type = 'ExceptionWhileProcessing'
              AND event_time > now() - INTERVAL 1 HOUR
            ORDER BY query_start_time DESC 
            LIMIT 20 
            FORMAT Vertical
        " || echo "N/A"
        
    } > "$file"
    
    log_success "Query information collected"
}

collect_background_tasks() {
    local file="${OUTPUT_DIR}/background_tasks.txt"
    log_info "Collecting background tasks information..."
    
    {
        echo "=== Active Merges ==="
        run_query "
            SELECT 
                database, table, elapsed, progress,
                formatReadableSize(total_size_bytes_compressed) as size,
                formatReadableSize(memory_usage) as memory,
                merge_type, merge_algorithm
            FROM system.merges 
            ORDER BY elapsed DESC 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Pending Mutations ==="
        run_query "
            SELECT 
                database, table, mutation_id, command,
                create_time, parts_to_do, is_done,
                latest_fail_reason
            FROM system.mutations 
            WHERE is_done = 0 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Parts Moves ==="
        run_query "SELECT * FROM system.moves FORMAT Vertical" || echo "N/A"
        echo ""
        
        echo "=== Detached Parts ==="
        run_query "
            SELECT 
                database, table, partition_id, name, reason
            FROM system.detached_parts 
            LIMIT 50 
            FORMAT PrettyCompact
        " || echo "N/A"
        
    } > "$file"
    
    log_success "Background tasks information collected"
}

collect_errors() {
    local file="${OUTPUT_DIR}/errors.txt"
    log_info "Collecting error information..."
    
    {
        echo "=== Error Statistics ==="
        run_query "
            SELECT 
                name, code, value, 
                last_error_time, last_error_message
            FROM system.errors 
            WHERE value > 0 
            ORDER BY last_error_time DESC 
            FORMAT Vertical
        " || echo "N/A"
        echo ""
        
        echo "=== Crash Reports ==="
        run_query "
            SELECT * 
            FROM system.crash_log 
            ORDER BY event_time DESC 
            LIMIT 5 
            FORMAT Vertical
        " || echo "N/A"
        
    } > "$file"
    
    log_success "Error information collected"
}

collect_keeper_info() {
    local file="${OUTPUT_DIR}/keeper_info.txt"
    log_info "Collecting Keeper/ZooKeeper information..."
    
    {
        echo "=== ZooKeeper Connection ==="
        run_query "SELECT * FROM system.zookeeper_connection FORMAT Vertical" || echo "N/A"
        echo ""
        
        echo "=== Keeper (if running locally) ==="
        if systemctl is-active --quiet clickhouse-keeper 2>/dev/null; then
            echo "ClickHouse Keeper is running"
            echo ""
            echo "Keeper stats (from 4-letter command):"
            echo "mntr" | nc localhost 9181 2>/dev/null || echo "Could not connect to Keeper"
        else
            echo "ClickHouse Keeper not running locally"
        fi
        
    } > "$file"
    
    log_success "Keeper information collected"
}

collect_logs() {
    if [ "$INCLUDE_LOGS" = "true" ]; then
        log_info "Collecting logs..."
        
        local log_dir="${OUTPUT_DIR}/logs"
        mkdir -p "$log_dir"
        
        # ClickHouse server logs
        for log_file in /var/log/clickhouse-server/*.log; do
            if [ -f "$log_file" ]; then
                tail -n 1000 "$log_file" > "${log_dir}/$(basename "$log_file")"
            fi
        done
        
        # System logs
        journalctl -u clickhouse-server --no-pager -n 500 > "${log_dir}/clickhouse-server.journal" 2>/dev/null || true
        journalctl -u clickhouse-keeper --no-pager -n 500 > "${log_dir}/clickhouse-keeper.journal" 2>/dev/null || true
        
        log_success "Logs collected"
    fi
}

collect_configs() {
    if [ "$INCLUDE_CONFIGS" = "true" ]; then
        log_info "Collecting configurations (sensitive data will be masked)..."
        
        local config_dir="${OUTPUT_DIR}/configs"
        mkdir -p "$config_dir"
        
        # Copy config files but mask passwords
        for config_file in /etc/clickhouse-server/*.xml /etc/clickhouse-server/config.d/*.xml; do
            if [ -f "$config_file" ]; then
                # Mask passwords and keys in config
                sed -E 's/(<password>)[^<]*(.*)/\1***MASKED***\2/gi; s/(<secret_access_key>)[^<]*(.*)/\1***MASKED***\2/gi' \
                    "$config_file" > "${config_dir}/$(basename "$config_file")"
            fi
        done
        
        log_success "Configurations collected (passwords masked)"
    fi
}

create_tarball() {
    local tarball="${OUTPUT_DIR}.tar.gz"
    
    log_info "Creating diagnostic bundle..."
    
    tar -czf "$tarball" -C "$(dirname "$OUTPUT_DIR")" "$(basename "$OUTPUT_DIR")"
    
    rm -rf "$OUTPUT_DIR"
    
    log_success "Diagnostic bundle created: $tarball"
    echo ""
    echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "  Diagnostic bundle: ${YELLOW}${tarball}${NC}"
    echo -e "  Size: $(du -h "$tarball" | cut -f1)"
    echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
}

# ============================================================================
# Main
# ============================================================================

main() {
    print_header "ClickHouse Diagnostic Collector"
    
    mkdir -p "$OUTPUT_DIR"
    
    # Collect all diagnostic information
    collect_system_info
    collect_clickhouse_info
    collect_metrics
    collect_storage_info
    collect_replication_info
    collect_query_info
    collect_background_tasks
    collect_errors
    collect_keeper_info
    collect_logs
    collect_configs
    
    # Create final tarball
    create_tarball
}

main
