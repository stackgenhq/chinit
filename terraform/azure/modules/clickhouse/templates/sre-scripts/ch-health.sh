# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-health.sh - ClickHouse Comprehensive Health Check Script
# ============================================================================
# Description: Performs comprehensive health checks on ClickHouse service
#              including connectivity, replication, memory, and disk status.
#
# Usage: ch-health.sh [--json] [--verbose]
#
# Options:
#   --json      Output results in JSON format
#   --verbose   Show detailed output
#
# Exit Codes:
#   0 - All checks passed (healthy)
#   1 - One or more checks failed (unhealthy)
#   2 - ClickHouse service not running
#
# Author: SRE Team
# Version: 1.0.0
# ============================================================================

set -euo pipefail

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/ch-common.sh"

# Parse arguments
JSON_OUTPUT=false
VERBOSE=false

while [[ $# -gt 0 ]]; do
    case $1 in
        --json)
            JSON_OUTPUT=true
            shift
            ;;
        --verbose)
            VERBOSE=true
            shift
            ;;
        -h|--help)
            echo "ch-health.sh - ClickHouse Comprehensive Health Check"
            echo ""
            echo "Usage: ch-health.sh [--json] [--verbose]"
            echo ""
            echo "Options:"
            echo "  --json      Output results in JSON format"
            echo "  --verbose   Show detailed output"
            echo "  -h, --help  Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Results storage
declare -A RESULTS
OVERALL_STATUS="healthy"

# ============================================================================
# Utility Functions
# ============================================================================

log_check() {
    local check_name=$1
    local status=$2
    local message=$3
    
    RESULTS["$check_name"]="$status:$message"
    
    if [ "$JSON_OUTPUT" = "false" ]; then
        print_status "$check_name" "$status" "$message"
    fi
    
    if [ "$status" = "FAIL" ]; then
        OVERALL_STATUS="unhealthy"
    fi
}

# ============================================================================
# Health Checks
# ============================================================================

check_service_running() {
    if ch_server_running; then
        log_check "Service Status" "PASS" "clickhouse-server is running"
        return 0
    else
        log_check "Service Status" "FAIL" "clickhouse-server is not running"
        return 1
    fi
}

check_connectivity() {
    if ch_is_reachable; then
        log_check "Connectivity" "PASS" "Successfully connected to ClickHouse"
        return 0
    else
        log_check "Connectivity" "FAIL" "Cannot connect to ClickHouse"
        return 1
    fi
}

check_uptime() {
    local uptime_seconds
    uptime_seconds=$(run_query "SELECT uptime()")
    
    if [ -n "$uptime_seconds" ]; then
        local uptime_hours=$((uptime_seconds / 3600))
        local uptime_days=$((uptime_hours / 24))
        
        if [ "$uptime_hours" -lt 1 ]; then
            log_check "Uptime" "WARN" "Server restarted recently: ${uptime_seconds}s ago"
        else
            log_check "Uptime" "PASS" "Uptime: ${uptime_days}d ${uptime_hours}h"
        fi
    else
        log_check "Uptime" "FAIL" "Could not determine uptime"
    fi
}

check_version() {
    local version
    version=$(run_query "SELECT version()")
    
    if [ -n "$version" ]; then
        log_check "Version" "PASS" "ClickHouse version: $version"
    else
        log_check "Version" "FAIL" "Could not determine version"
    fi
}

check_memory() {
    local memory_info
    memory_info=$(run_query "
        SELECT 
            formatReadableSize(sum(value)) as current_memory,
            formatReadableSize(getSetting('max_memory_usage')) as max_memory
        FROM system.metrics 
        WHERE metric = 'MemoryTracking'
    ")
    
    if [ -n "$memory_info" ]; then
        log_check "Memory Usage" "PASS" "$memory_info"
    else
        log_check "Memory Usage" "WARN" "Could not retrieve memory info"
    fi
    
    # Check for memory pressure
    local memory_percent
    memory_percent=$(run_query "
        SELECT round(100 * value / getSetting('max_memory_usage'), 2)
        FROM system.metrics 
        WHERE metric = 'MemoryTracking'
    " 2>/dev/null || echo "0")
    
    if [ -n "$memory_percent" ] && [ "${memory_percent%.*}" -gt 80 ]; then
        log_check "Memory Pressure" "WARN" "Memory usage at ${memory_percent}%"
    fi
}

check_active_queries() {
    local query_count
    query_count=$(run_query "SELECT count() FROM system.processes WHERE is_cancelled = 0")
    
    if [ -n "$query_count" ]; then
        if [ "$query_count" -gt 100 ]; then
            log_check "Active Queries" "WARN" "$query_count active queries (high)"
        else
            log_check "Active Queries" "PASS" "$query_count active queries"
        fi
    else
        log_check "Active Queries" "FAIL" "Could not determine active queries"
    fi
}

check_replication_health() {
    local replica_check
    replica_check=$(run_query "
        SELECT 
            count() as total,
            countIf(is_readonly = 1) as readonly,
            countIf(is_session_expired = 1) as expired,
            countIf(future_parts > 20) as behind
        FROM system.replicas
    ")
    
    if [ -n "$replica_check" ]; then
        local readonly=$(echo "$replica_check" | awk '{print $2}')
        local expired=$(echo "$replica_check" | awk '{print $3}')
        local behind=$(echo "$replica_check" | awk '{print $4}')
        
        if [ "$readonly" -gt 0 ] || [ "$expired" -gt 0 ]; then
            log_check "Replication" "FAIL" "Issues detected: $readonly readonly, $expired expired sessions"
        elif [ "$behind" -gt 0 ]; then
            log_check "Replication" "WARN" "$behind tables behind on replication"
        else
            log_check "Replication" "PASS" "All replicas healthy"
        fi
    else
        log_check "Replication" "PASS" "No replicated tables or not a data node"
    fi
}

check_zookeeper_connection() {
    local zk_status
    zk_status=$(run_query "
        SELECT count() as total, countIf(is_expired = 0) as connected
        FROM system.zookeeper_connection
    " 2>/dev/null)
    
    if [ -n "$zk_status" ]; then
        local total=$(echo "$zk_status" | awk '{print $1}')
        local connected=$(echo "$zk_status" | awk '{print $2}')
        
        if [ "$total" -gt 0 ] && [ "$connected" = "$total" ]; then
            log_check "Keeper Connection" "PASS" "Connected to ClickHouse Keeper"
        elif [ "$total" -gt 0 ]; then
            log_check "Keeper Connection" "FAIL" "Keeper connection issues: $connected/$total connected"
        else
            log_check "Keeper Connection" "PASS" "Keeper not configured (standalone mode)"
        fi
    fi
}

check_disk_space() {
    local disk_info
    disk_info=$(run_query "
        SELECT 
            name,
            formatReadableSize(free_space) as free,
            formatReadableSize(total_space) as total,
            round(100 * (1 - free_space / total_space), 2) as used_percent
        FROM system.disks
        WHERE total_space > 0
    ")
    
    if [ -n "$disk_info" ]; then
        while IFS=$'\t' read -r name free total used_percent; do
            if [ "${used_percent%.*}" -gt 90 ]; then
                log_check "Disk: $name" "FAIL" "CRITICAL: ${used_percent}% used ($free free)"
            elif [ "${used_percent%.*}" -gt 80 ]; then
                log_check "Disk: $name" "WARN" "${used_percent}% used ($free free)"
            else
                log_check "Disk: $name" "PASS" "${used_percent}% used ($free free)"
            fi
        done <<< "$disk_info"
    fi
}

check_parts_health() {
    local parts_check
    parts_check=$(run_query "
        SELECT 
            count() as total_parts,
            countIf(active = 0) as inactive_parts
        FROM system.parts
        WHERE database != 'system'
    ")
    
    if [ -n "$parts_check" ]; then
        local total=$(echo "$parts_check" | awk '{print $1}')
        local inactive=$(echo "$parts_check" | awk '{print $2}')
        
        if [ "$inactive" -gt 100 ]; then
            log_check "Parts Health" "WARN" "$total total parts, $inactive inactive (cleanup needed)"
        else
            log_check "Parts Health" "PASS" "$total total parts, $inactive inactive"
        fi
    fi
}

check_mutations() {
    local mutation_check
    mutation_check=$(run_query "
        SELECT count() as stuck_mutations
        FROM system.mutations
        WHERE is_done = 0 AND create_time < now() - INTERVAL 1 HOUR
    ")
    
    if [ -n "$mutation_check" ] && [ "$mutation_check" -gt 0 ]; then
        log_check "Mutations" "WARN" "$mutation_check stuck mutations (>1 hour old)"
    else
        log_check "Mutations" "PASS" "No stuck mutations"
    fi
}

check_merge_queue() {
    local merge_count
    merge_count=$(run_query "SELECT count() FROM system.merges")
    
    if [ -n "$merge_count" ]; then
        if [ "$merge_count" -gt 50 ]; then
            log_check "Merge Queue" "WARN" "$merge_count active merges (high backlog)"
        else
            log_check "Merge Queue" "PASS" "$merge_count active merges"
        fi
    fi
}

check_distributed_queue() {
    local queue_size
    queue_size=$(run_query "
        SELECT sum(data_compressed_bytes) 
        FROM system.distribution_queue
    " 2>/dev/null || echo "0")
    
    if [ -n "$queue_size" ] && [ "$queue_size" -gt 0 ]; then
        local readable_size
        readable_size=$(run_query "SELECT formatReadableSize($queue_size)")
        log_check "Distributed Queue" "WARN" "$readable_size pending in distributed queue"
    else
        log_check "Distributed Queue" "PASS" "Distributed queue empty"
    fi
}

check_errors() {
    local error_count
    error_count=$(run_query "
        SELECT count() 
        FROM system.errors 
        WHERE last_error_time > now() - INTERVAL 1 HOUR
    ")
    
    if [ -n "$error_count" ] && [ "$error_count" -gt 0 ]; then
        log_check "Recent Errors" "WARN" "$error_count error types in last hour"
        
        if [ "$VERBOSE" = "true" ] && [ "$JSON_OUTPUT" = "false" ]; then
            echo "  Recent errors:"
            run_query "
                SELECT name, value, last_error_message 
                FROM system.errors 
                WHERE last_error_time > now() - INTERVAL 1 HOUR 
                ORDER BY value DESC 
                LIMIT 5
                FORMAT PrettyCompact
            " | sed 's/^/    /'
        fi
    else
        log_check "Recent Errors" "PASS" "No recent errors"
    fi
}

# ============================================================================
# Output Results
# ============================================================================

output_json() {
    echo "{"
    echo "  \"timestamp\": \"$(date -Iseconds)\","
    echo "  \"hostname\": \"$(hostname)\","
    echo "  \"status\": \"$OVERALL_STATUS\","
    echo "  \"checks\": {"
    
    local first=true
    for key in "${!RESULTS[@]}"; do
        if [ "$first" = "true" ]; then
            first=false
        else
            echo ","
        fi
        
        local value="${RESULTS[$key]}"
        local status="${value%%:*}"
        local message="${value#*:}"
        
        printf "    \"%s\": {\"status\": \"%s\", \"message\": \"%s\"}" "$key" "$status" "$message"
    done
    
    echo ""
    echo "  }"
    echo "}"
}

# ============================================================================
# Main
# ============================================================================

main() {
    if [ "$JSON_OUTPUT" = "false" ]; then
        print_header "ClickHouse Health Check"
    fi
    
    # Run all health checks
    check_service_running || { 
        if [ "$JSON_OUTPUT" = "true" ]; then
            output_json
        fi
        exit 2
    }
    
    check_connectivity || { 
        if [ "$JSON_OUTPUT" = "true" ]; then
            output_json
        fi
        exit 2
    }
    
    check_uptime
    check_version
    check_memory
    check_active_queries
    check_replication_health
    check_zookeeper_connection
    check_disk_space
    check_parts_health
    check_mutations
    check_merge_queue
    check_distributed_queue
    check_errors
    
    if [ "$JSON_OUTPUT" = "true" ]; then
        output_json
    else
        echo ""
        if [ "$OVERALL_STATUS" = "healthy" ]; then
            echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
            echo -e "${GREEN}                    Overall Status: HEALTHY                        ${NC}"
            echo -e "${GREEN}═══════════════════════════════════════════════════════════════════${NC}"
        else
            echo -e "${RED}═══════════════════════════════════════════════════════════════════${NC}"
            echo -e "${RED}                   Overall Status: UNHEALTHY                        ${NC}"
            echo -e "${RED}═══════════════════════════════════════════════════════════════════${NC}"
        fi
        echo ""
    fi
    
    [ "$OVERALL_STATUS" = "healthy" ] && exit 0 || exit 1
}

main
