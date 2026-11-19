# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-queries.sh - ClickHouse Query Monitor and Analyzer
# ============================================================================
# Description: Monitors running queries, analyzes slow queries, and helps
#              identify query performance issues.
#
# Usage: ch-queries.sh [command] [options]
#
# Commands:
#   running       Show currently running queries (default)
#   slow          Show slow queries from query log
#   errors        Show recent query errors
#   kill          Kill a query by ID (requires --query-id)
#   top           Show top resource-consuming queries
#
# Options:
#   --watch              Continuously monitor
#   --interval SECONDS   Update interval (default: 5)
#   --limit N            Limit number of results (default: 20)
#   --query-id ID        Query ID for kill command
#   --min-duration MS    Minimum duration for slow queries (default: 5000)
#
# Author: SRE Team
# Version: 1.0.0
# ============================================================================

set -euo pipefail

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/ch-common.sh"

# Defaults
COMMAND="running"
WATCH_MODE=false
WATCH_INTERVAL=5
LIMIT=20
QUERY_ID=""
MIN_DURATION=5000

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        running|slow|errors|kill|top)
            COMMAND=$1
            shift
            ;;
        --watch)
            WATCH_MODE=true
            shift
            ;;
        --interval)
            WATCH_INTERVAL=$2
            shift 2
            ;;
        --limit)
            LIMIT=$2
            shift 2
            ;;
        --query-id)
            QUERY_ID=$2
            shift 2
            ;;
        --min-duration)
            MIN_DURATION=$2
            shift 2
            ;;
        -h|--help)
            echo "ch-queries.sh - ClickHouse Query Monitor"
            echo ""
            echo "Usage: ch-queries.sh [command] [options]"
            echo ""
            echo "Commands:"
            echo "  running       Show running queries (default)"
            echo "  slow          Show slow queries from log"
            echo "  errors        Show recent query errors"
            echo "  kill          Kill a query by ID"
            echo "  top           Show top resource-consuming queries"
            echo ""
            echo "Options:"
            echo "  --watch              Continuously monitor"
            echo "  --interval SECONDS   Update interval (default: 5)"
            echo "  --limit N            Limit results (default: 20)"
            echo "  --query-id ID        Query ID for kill command"
            echo "  --min-duration MS    Min duration for slow queries (default: 5000)"
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
# Command Functions
# ============================================================================

show_running() {
    if [ "$WATCH_MODE" = "true" ]; then
        clear
    fi
    
    print_header "Running Queries"
    
    local count
    count=$(run_query "SELECT count() FROM system.processes WHERE is_cancelled = 0")
    echo -e "${CYAN}Active Queries: ${count}${NC}"
    echo ""
    
    local queries
    queries=$(run_query "
        SELECT 
            query_id,
            user,
            round(elapsed, 2) as elapsed_sec,
            formatReadableQuantity(read_rows) as rows_read,
            formatReadableSize(read_bytes) as bytes_read,
            formatReadableSize(memory_usage) as memory,
            formatReadableSize(peak_memory_usage) as peak_memory,
            substring(query, 1, 80) as query_preview
        FROM system.processes
        WHERE is_cancelled = 0
        ORDER BY elapsed DESC
        LIMIT $LIMIT
        FORMAT TabSeparated
    ")
    
    if [ -n "$queries" ]; then
        printf "%-36s | %-10s | %-8s | %-12s | %-10s | %-10s | Query\n" "Query ID" "User" "Elapsed" "Rows" "Memory" "Peak Mem"
        printf "%-36s-+-%-10s-+-%-8s-+-%-12s-+-%-10s-+-%-10s-+--------\n" "------------------------------------" "----------" "--------" "------------" "----------" "----------"
        
        while IFS=$'\t' read -r qid user elapsed rows bytes mem peak query; do
            if [ -n "$qid" ]; then
                # Color code by elapsed time
                local elapsed_color=$GREEN
                if (( $(echo "$elapsed > 30" | bc -l) )); then
                    elapsed_color=$RED
                elif (( $(echo "$elapsed > 10" | bc -l) )); then
                    elapsed_color=$YELLOW
                fi
                
                printf "%-36s | %-10s | ${elapsed_color}%-8s${NC} | %-12s | %-10s | %-10s | %s\n" \
                    "${qid:0:36}" "${user:0:10}" "${elapsed}s" "$rows" "$mem" "$peak" "${query:0:50}..."
            fi
        done <<< "$queries"
    else
        echo -e "  ${GREEN}No running queries${NC}"
    fi
    
    if [ "$WATCH_MODE" = "true" ]; then
        echo ""
        echo -e "  ${YELLOW}Refreshing every ${WATCH_INTERVAL}s. Press Ctrl+C to exit.${NC}"
    fi
}

show_slow() {
    print_header "Slow Queries (>${MIN_DURATION}ms) - Last Hour"
    
    local queries
    queries=$(run_query "
        SELECT 
            query_start_time,
            user,
            round(query_duration_ms/1000, 2) as duration_sec,
            formatReadableQuantity(read_rows) as rows_read,
            formatReadableSize(read_bytes) as bytes_read,
            formatReadableSize(memory_usage) as memory,
            type,
            substring(query, 1, 100) as query_preview
        FROM system.query_log
        WHERE type IN ('QueryFinish', 'ExceptionWhileProcessing')
          AND event_time > now() - INTERVAL 1 HOUR
          AND query_duration_ms > $MIN_DURATION
          AND query NOT LIKE '%system.%'
        ORDER BY query_duration_ms DESC
        LIMIT $LIMIT
        FORMAT TabSeparated
    ")
    
    if [ -n "$queries" ]; then
        printf "%-19s | %-10s | %-10s | %-12s | %-10s | %-12s | Query\n" "Time" "User" "Duration" "Rows" "Memory" "Status"
        printf "%-19s-+-%-10s-+-%-10s-+-%-12s-+-%-10s-+-%-12s-+--------\n" "-------------------" "----------" "----------" "------------" "----------" "------------"
        
        while IFS=$'\t' read -r time user duration rows bytes mem status query; do
            if [ -n "$time" ]; then
                local status_color=$GREEN
                if [ "$status" = "ExceptionWhileProcessing" ]; then
                    status_color=$RED
                    status="ERROR"
                else
                    status="OK"
                fi
                
                printf "%-19s | %-10s | %-10s | %-12s | %-10s | ${status_color}%-12s${NC} | %s\n" \
                    "$time" "${user:0:10}" "${duration}s" "$rows" "$mem" "$status" "${query:0:50}..."
            fi
        done <<< "$queries"
    else
        echo -e "  ${GREEN}No slow queries in the last hour${NC}"
    fi
}

show_errors() {
    print_header "Query Errors - Last Hour"
    
    local errors
    errors=$(run_query "
        SELECT 
            query_start_time,
            user,
            client_hostname,
            exception_code,
            substring(exception, 1, 150) as exception,
            substring(query, 1, 100) as query_preview
        FROM system.query_log
        WHERE type = 'ExceptionWhileProcessing'
          AND event_time > now() - INTERVAL 1 HOUR
        ORDER BY query_start_time DESC
        LIMIT $LIMIT
        FORMAT TabSeparated
    ")
    
    if [ -n "$errors" ]; then
        while IFS=$'\t' read -r time user client code exception query; do
            if [ -n "$time" ]; then
                echo -e "${RED}[$time]${NC} User: $user from $client"
                echo -e "  Error Code: ${YELLOW}$code${NC}"
                echo -e "  Exception: $exception"
                echo -e "  Query: ${query:0:80}..."
                echo ""
            fi
        done <<< "$errors"
    else
        echo -e "  ${GREEN}No query errors in the last hour${NC}"
    fi
}

show_top() {
    print_header "Top Resource-Consuming Queries - Last Hour"
    
    print_section "Top by Total Time"
    run_query "
        SELECT 
            user,
            count() as count,
            round(sum(query_duration_ms)/1000, 2) as total_time_sec,
            round(avg(query_duration_ms)/1000, 2) as avg_time_sec,
            formatReadableQuantity(sum(read_rows)) as total_rows,
            formatReadableSize(sum(memory_usage)) as total_memory
        FROM system.query_log
        WHERE type = 'QueryFinish'
          AND event_time > now() - INTERVAL 1 HOUR
        GROUP BY user
        ORDER BY sum(query_duration_ms) DESC
        LIMIT 10
        FORMAT PrettyCompact
    "
    echo ""
    
    print_section "Top by Memory Usage"
    run_query "
        SELECT 
            query_start_time,
            user,
            formatReadableSize(memory_usage) as memory,
            round(query_duration_ms/1000, 2) as duration_sec,
            substring(query, 1, 80) as query
        FROM system.query_log
        WHERE type = 'QueryFinish'
          AND event_time > now() - INTERVAL 1 HOUR
        ORDER BY memory_usage DESC
        LIMIT 10
        FORMAT PrettyCompact
    "
    echo ""
    
    print_section "Top by Rows Read"
    run_query "
        SELECT 
            query_start_time,
            user,
            formatReadableQuantity(read_rows) as rows,
            formatReadableSize(read_bytes) as bytes,
            round(query_duration_ms/1000, 2) as duration_sec,
            substring(query, 1, 80) as query
        FROM system.query_log
        WHERE type = 'QueryFinish'
          AND event_time > now() - INTERVAL 1 HOUR
        ORDER BY read_rows DESC
        LIMIT 10
        FORMAT PrettyCompact
    "
    echo ""
    
    print_section "Query Types Distribution"
    run_query "
        SELECT 
            query_kind,
            count() as count,
            round(avg(query_duration_ms), 2) as avg_ms,
            formatReadableQuantity(sum(read_rows)) as total_rows
        FROM system.query_log
        WHERE type = 'QueryFinish'
          AND event_time > now() - INTERVAL 1 HOUR
        GROUP BY query_kind
        ORDER BY count DESC
        FORMAT PrettyCompact
    "
}

kill_query() {
    if [ -z "$QUERY_ID" ]; then
        echo -e "${RED}Error: --query-id is required for kill command${NC}"
        exit 1
    fi
    
    echo -e "${YELLOW}Killing query: $QUERY_ID${NC}"
    
    run_query "KILL QUERY WHERE query_id = '$QUERY_ID' ASYNC"
    
    if [ $? -eq 0 ]; then
        echo -e "${GREEN}Kill signal sent successfully${NC}"
        echo ""
        echo "Note: Query cancellation is asynchronous. Check running queries to verify."
    else
        echo -e "${RED}Failed to kill query${NC}"
    fi
}

# ============================================================================
# Main
# ============================================================================

main() {
    case $COMMAND in
        running)
            if [ "$WATCH_MODE" = "true" ]; then
                while true; do
                    show_running
                    sleep "$WATCH_INTERVAL"
                done
            else
                show_running
            fi
            ;;
        slow)
            show_slow
            ;;
        errors)
            show_errors
            ;;
        top)
            show_top
            ;;
        kill)
            kill_query
            ;;
        *)
            echo "Unknown command: $COMMAND"
            exit 1
            ;;
    esac
}

main
