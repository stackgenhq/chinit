# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-status.sh - ClickHouse Service Status Script
# ============================================================================
# Description: Provides quick service status and key metrics for ClickHouse.
#              Designed for quick operational checks.
#
# Usage: ch-status.sh [--watch] [--interval SECONDS]
#
# Options:
#   --watch              Continuously monitor status
#   --interval SECONDS   Update interval for watch mode (default: 5)
#
# Author: SRE Team
# Version: 1.0.0
# ============================================================================

set -euo pipefail

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/ch-common.sh"

# Parse arguments
WATCH_MODE=false
WATCH_INTERVAL=5

while [[ $# -gt 0 ]]; do
    case $1 in
        --watch)
            WATCH_MODE=true
            shift
            ;;
        --interval)
            WATCH_INTERVAL=$2
            shift 2
            ;;
        -h|--help)
            echo "ch-status.sh - ClickHouse Service Status"
            echo ""
            echo "Usage: ch-status.sh [--watch] [--interval SECONDS]"
            echo ""
            echo "Options:"
            echo "  --watch              Continuously monitor status"
            echo "  --interval SECONDS   Update interval (default: 5)"
            echo "  -h, --help           Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# ============================================================================
# Status Functions
# ============================================================================

get_service_status() {
    if ch_server_running; then
        echo -e "${GREEN}● Running${NC}"
    else
        echo -e "${RED}● Stopped${NC}"
    fi
}

get_service_uptime() {
    local uptime_seconds
    uptime_seconds=$(run_query "SELECT uptime()" 2>/dev/null || echo "0")
    
    if [ "$uptime_seconds" -gt 0 ] 2>/dev/null; then
        format_duration "$uptime_seconds"
    else
        echo "N/A"
    fi
}

get_connections() {
    run_query "SELECT count() FROM system.processes" 2>/dev/null || echo "0"
}

get_queries_per_second() {
    run_query "
        SELECT round(
            countIf(type = 'QueryFinish' AND event_time > now() - INTERVAL 60 SECOND) / 60.0, 2
        ) as qps
        FROM system.query_log
        WHERE event_time > now() - INTERVAL 60 SECOND
    " 2>/dev/null || echo "0"
}

get_memory_usage() {
    run_query "
        SELECT formatReadableSize(sum(value))
        FROM system.metrics 
        WHERE metric = 'MemoryTracking'
    " 2>/dev/null || echo "N/A"
}

get_running_queries() {
    run_query "SELECT count() FROM system.processes WHERE is_cancelled = 0" 2>/dev/null || echo "0"
}

get_merges() {
    run_query "SELECT count() FROM system.merges" 2>/dev/null || echo "0"
}

get_mutations() {
    run_query "SELECT count() FROM system.mutations WHERE is_done = 0" 2>/dev/null || echo "0"
}

get_replication_lag() {
    local lag
    lag=$(run_query "
        SELECT max(absolute_delay) 
        FROM system.replicas 
        WHERE active_replicas > 0
    " 2>/dev/null || echo "0")
    
    if [ -z "$lag" ] || [ "$lag" = "0" ]; then
        echo "${GREEN}0s${NC}"
    elif [ "$lag" -lt 60 ]; then
        echo "${YELLOW}${lag}s${NC}"
    else
        echo "${RED}${lag}s${NC}"
    fi
}

get_insert_rate() {
    run_query "
        SELECT formatReadableQuantity(
            sumIf(value, event = 'InsertedRows')
        )
        FROM system.events
    " 2>/dev/null || echo "N/A"
}

get_disk_usage() {
    run_query "
        SELECT 
            name,
            formatReadableSize(free_space) as free,
            round(100 * (1 - free_space / total_space), 1) as used_pct
        FROM system.disks
        WHERE total_space > 0
        FORMAT TabSeparated
    " 2>/dev/null
}

get_recent_errors() {
    run_query "
        SELECT count() 
        FROM system.errors 
        WHERE last_error_time > now() - INTERVAL 5 MINUTE
    " 2>/dev/null || echo "0"
}

# ============================================================================
# Display Functions
# ============================================================================

print_status() {
    clear
    
    local service_status=$(get_service_status)
    local uptime=$(get_service_uptime)
    local connections=$(get_connections)
    local qps=$(get_queries_per_second)
    local memory=$(get_memory_usage)
    local running=$(get_running_queries)
    local merges=$(get_merges)
    local mutations=$(get_mutations)
    local rep_lag=$(get_replication_lag)
    local errors=$(get_recent_errors)
    
    echo -e "${BLUE}╔══════════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BLUE}║              ClickHouse Status - $(hostname)                   ║${NC}"
    echo -e "${BLUE}║                     $(date '+%Y-%m-%d %H:%M:%S')                         ║${NC}"
    echo -e "${BLUE}╠══════════════════════════════════════════════════════════════════════╣${NC}"
    echo ""
    
    printf "${CYAN}  %-20s${NC} %s\n" "Service:" "$service_status"
    printf "${CYAN}  %-20s${NC} %s\n" "Uptime:" "$uptime"
    echo ""
    
    echo -e "${BLUE}  ─────────────── Performance ───────────────${NC}"
    printf "  %-20s %s\n" "Active Queries:" "$running"
    printf "  %-20s %s\n" "Connections:" "$connections"
    printf "  %-20s %s\n" "QPS (1 min avg):" "$qps"
    printf "  %-20s %s\n" "Memory Used:" "$memory"
    echo ""
    
    echo -e "${BLUE}  ─────────────── Background Tasks ───────────────${NC}"
    printf "  %-20s %s\n" "Active Merges:" "$merges"
    printf "  %-20s %s\n" "Pending Mutations:" "$mutations"
    echo ""
    
    echo -e "${BLUE}  ─────────────── Replication ───────────────${NC}"
    echo -e "  Replication Lag:     $rep_lag"
    echo ""
    
    echo -e "${BLUE}  ─────────────── Disk Usage ───────────────${NC}"
    while IFS=$'\t' read -r name free pct; do
        if [ -n "$name" ]; then
            local color=$GREEN
            if [ "${pct%.*}" -gt 90 ]; then
                color=$RED
            elif [ "${pct%.*}" -gt 80 ]; then
                color=$YELLOW
            fi
            printf "  %-12s ${color}%5.1f%%${NC} used (${free} free)\n" "$name:" "$pct"
        fi
    done <<< "$(get_disk_usage)"
    echo ""
    
    echo -e "${BLUE}  ─────────────── Health ───────────────${NC}"
    if [ "$errors" -gt 0 ]; then
        printf "  Recent Errors:       ${RED}%s (last 5 min)${NC}\n" "$errors"
    else
        printf "  Recent Errors:       ${GREEN}0${NC}\n"
    fi
    echo ""
    
    echo -e "${BLUE}╚══════════════════════════════════════════════════════════════════════╝${NC}"
    
    if [ "$WATCH_MODE" = "true" ]; then
        echo ""
        echo -e "  ${YELLOW}Refreshing every ${WATCH_INTERVAL}s. Press Ctrl+C to exit.${NC}"
    fi
}

# ============================================================================
# Main
# ============================================================================

main() {
    if [ "$WATCH_MODE" = "true" ]; then
        while true; do
            print_status
            sleep "$WATCH_INTERVAL"
        done
    else
        print_status
    fi
}

main
