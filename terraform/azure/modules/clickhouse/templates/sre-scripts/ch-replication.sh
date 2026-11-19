# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-replication.sh - ClickHouse Replication Health Monitor
# ============================================================================
# Description: Monitors replication health, lag, and queue status across
#              all replicated tables. Essential for cluster health monitoring.
#
# Usage: ch-replication.sh [--watch] [--interval SECONDS] [--alerts-only]
#
# Options:
#   --watch        Continuously monitor replication
#   --interval     Update interval for watch mode (default: 10)
#   --alerts-only  Only show tables with replication issues
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
WATCH_INTERVAL=10
ALERTS_ONLY=false

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
        --alerts-only)
            ALERTS_ONLY=true
            shift
            ;;
        -h|--help)
            echo "ch-replication.sh - ClickHouse Replication Monitor"
            echo ""
            echo "Usage: ch-replication.sh [--watch] [--interval SECONDS] [--alerts-only]"
            echo ""
            echo "Options:"
            echo "  --watch        Continuously monitor replication"
            echo "  --interval     Update interval (default: 10)"
            echo "  --alerts-only  Only show tables with issues"
            echo "  -h, --help     Show this help message"
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

get_replica_summary() {
    run_query "
        SELECT 
            count() as total_tables,
            countIf(is_readonly = 1) as readonly,
            countIf(is_session_expired = 1) as session_expired,
            countIf(absolute_delay > 300) as lagging_5min,
            countIf(queue_size > 0) as with_queue,
            countIf(parts_to_check > 0) as parts_check_needed,
            countIf(lost_part_count > 0) as lost_parts
        FROM system.replicas
    "
}

get_replication_details() {
    local filter=""
    if [ "$ALERTS_ONLY" = "true" ]; then
        filter="WHERE is_readonly = 1 
                   OR is_session_expired = 1 
                   OR absolute_delay > 60 
                   OR queue_size > 10 
                   OR future_parts > 20
                   OR parts_to_check > 0
                   OR lost_part_count > 0"
    fi
    
    run_query "
        SELECT 
            database,
            table,
            replica_name,
            multiIf(
                is_readonly = 1, '${RED}READONLY${NC}',
                is_session_expired = 1, '${RED}EXPIRED${NC}',
                absolute_delay > 300, '${YELLOW}LAGGING${NC}',
                queue_size > 10, '${YELLOW}QUEUE${NC}',
                '${GREEN}OK${NC}'
            ) as status,
            is_leader,
            total_replicas,
            active_replicas,
            queue_size,
            inserts_in_queue,
            merges_in_queue,
            future_parts,
            parts_to_check,
            absolute_delay as lag_seconds,
            lost_part_count
        FROM system.replicas
        $filter
        ORDER BY absolute_delay DESC, queue_size DESC
    "
}

get_replication_queue() {
    run_query "
        SELECT 
            database,
            table,
            type,
            create_time,
            source_replica,
            num_tries,
            formatReadableSize(size_in_bytes) as size,
            CASE 
                WHEN num_tries > 5 THEN '${RED}STUCK${NC}'
                WHEN num_tries > 2 THEN '${YELLOW}RETRYING${NC}'
                ELSE '${GREEN}OK${NC}'
            END as status,
            substring(last_exception, 1, 100) as last_error
        FROM system.replication_queue
        WHERE num_tries > 0 OR create_time < now() - INTERVAL 5 MINUTE
        ORDER BY num_tries DESC, create_time ASC
        LIMIT 20
    "
}

get_zookeeper_status() {
    run_query "
        SELECT 
            host,
            port,
            index,
            is_expired,
            session_uptime_elapsed_seconds
        FROM system.zookeeper_connection
    "
}

get_distributed_ddl() {
    run_query "
        SELECT 
            entry,
            query_start_time,
            host,
            port,
            status,
            exception_code,
            substring(exception_text, 1, 100) as exception
        FROM system.distributed_ddl_queue
        WHERE status != 'Finished' OR query_start_time > now() - INTERVAL 1 HOUR
        ORDER BY query_start_time DESC
        LIMIT 10
    "
}

# ============================================================================
# Display Functions
# ============================================================================

print_header_replication() {
    if [ "$WATCH_MODE" = "true" ]; then
        clear
    fi
    print_header "ClickHouse Replication Monitor"
}

print_summary() {
    print_section "Replication Summary"
    
    local summary
    summary=$(get_replica_summary)
    
    if [ -n "$summary" ]; then
        local total=$(echo "$summary" | awk '{print $1}')
        local readonly=$(echo "$summary" | awk '{print $2}')
        local expired=$(echo "$summary" | awk '{print $3}')
        local lagging=$(echo "$summary" | awk '{print $4}')
        local queue=$(echo "$summary" | awk '{print $5}')
        local check=$(echo "$summary" | awk '{print $6}')
        local lost=$(echo "$summary" | awk '{print $7}')
        
        printf "  %-25s %s\n" "Total Replicated Tables:" "$total"
        
        # Color code based on status
        if [ "$readonly" -gt 0 ]; then
            printf "  %-25s ${RED}%s${NC}\n" "Readonly:" "$readonly"
        else
            printf "  %-25s ${GREEN}%s${NC}\n" "Readonly:" "$readonly"
        fi
        
        if [ "$expired" -gt 0 ]; then
            printf "  %-25s ${RED}%s${NC}\n" "Session Expired:" "$expired"
        else
            printf "  %-25s ${GREEN}%s${NC}\n" "Session Expired:" "$expired"
        fi
        
        if [ "$lagging" -gt 0 ]; then
            printf "  %-25s ${YELLOW}%s${NC}\n" "Lagging (>5 min):" "$lagging"
        else
            printf "  %-25s ${GREEN}%s${NC}\n" "Lagging (>5 min):" "$lagging"
        fi
        
        printf "  %-25s %s\n" "With Queue:" "$queue"
        
        if [ "$check" -gt 0 ]; then
            printf "  %-25s ${YELLOW}%s${NC}\n" "Parts to Check:" "$check"
        fi
        
        if [ "$lost" -gt 0 ]; then
            printf "  %-25s ${RED}%s${NC}\n" "Lost Parts:" "$lost"
        fi
    else
        echo "  No replicated tables found or unable to connect"
    fi
    echo ""
}

print_replica_details() {
    print_section "Replica Details"
    
    local details
    details=$(get_replication_details)
    
    if [ -n "$details" ]; then
        echo ""
        echo "Database          | Table                  | Status   | Leader | Active | Queue | Lag"
        echo "------------------|------------------------|----------|--------|--------|-------|--------"
        
        while IFS=$'\t' read -r db table replica status leader total active queue inserts merges future check lag lost; do
            if [ -n "$db" ]; then
                local lag_display="${lag}s"
                if [ "$lag" -gt 300 ]; then
                    lag_display="${YELLOW}${lag}s${NC}"
                elif [ "$lag" -gt 600 ]; then
                    lag_display="${RED}${lag}s${NC}"
                fi
                
                printf "%-17s | %-22s | " "$db" "$table"
                echo -e -n "$status"
                printf " | %-6s | %d/%d    | %-5d | %s\n" "$leader" "$active" "$total" "$queue" "$lag_display"
            fi
        done <<< "$details"
    else
        if [ "$ALERTS_ONLY" = "true" ]; then
            echo -e "  ${GREEN}No replication issues detected${NC}"
        else
            echo "  No replicated tables found"
        fi
    fi
    echo ""
}

print_queue_status() {
    print_section "Replication Queue (Problem Items)"
    
    local queue
    queue=$(get_replication_queue)
    
    if [ -n "$queue" ]; then
        echo ""
        echo "Database       | Table              | Type       | Tries | Status   | Error"
        echo "---------------|--------------------|-----------:|-------|----------|----------------"
        
        while IFS=$'\t' read -r db table type created source tries size status error; do
            if [ -n "$db" ]; then
                printf "%-14s | %-18s | %-10s | %-5s | " "$db" "$table" "$type" "$tries"
                echo -e -n "$status"
                printf " | %s\n" "${error:0:40}"
            fi
        done <<< "$queue"
    else
        echo -e "  ${GREEN}Replication queue is healthy${NC}"
    fi
    echo ""
}

print_zk_status() {
    print_section "Keeper/ZooKeeper Connection"
    
    local zk_status
    zk_status=$(get_zookeeper_status)
    
    if [ -n "$zk_status" ]; then
        while IFS=$'\t' read -r host port index expired uptime; do
            if [ -n "$host" ]; then
                local status_color=$GREEN
                local status_text="Connected"
                if [ "$expired" = "1" ]; then
                    status_color=$RED
                    status_text="EXPIRED"
                fi
                printf "  %-30s ${status_color}%s${NC} (uptime: %ss)\n" "$host:$port" "$status_text" "$uptime"
            fi
        done <<< "$zk_status"
    else
        echo "  Not connected to Keeper/ZooKeeper or standalone mode"
    fi
    echo ""
}

print_ddl_queue() {
    print_section "Distributed DDL Queue"
    
    local ddl
    ddl=$(get_distributed_ddl)
    
    if [ -n "$ddl" ]; then
        while IFS=$'\t' read -r entry time host port status code exception; do
            if [ -n "$entry" ]; then
                local status_color=$GREEN
                if [ "$status" != "Finished" ]; then
                    status_color=$YELLOW
                fi
                if [ -n "$code" ] && [ "$code" != "0" ]; then
                    status_color=$RED
                fi
                printf "  %-30s ${status_color}%s${NC} on %s:%s\n" "$entry" "$status" "$host" "$port"
                if [ -n "$exception" ]; then
                    echo "    Error: ${exception:0:60}..."
                fi
            fi
        done <<< "$ddl"
    else
        echo "  No recent DDL operations"
    fi
    echo ""
}

# ============================================================================
# Main
# ============================================================================

monitor_once() {
    print_header_replication
    print_summary
    print_zk_status
    print_replica_details
    print_queue_status
    print_ddl_queue
    
    if [ "$WATCH_MODE" = "true" ]; then
        echo -e "  ${YELLOW}Refreshing every ${WATCH_INTERVAL}s. Press Ctrl+C to exit.${NC}"
    fi
}

main() {
    if [ "$WATCH_MODE" = "true" ]; then
        while true; do
            monitor_once
            sleep "$WATCH_INTERVAL"
        done
    else
        monitor_once
    fi
}

main
