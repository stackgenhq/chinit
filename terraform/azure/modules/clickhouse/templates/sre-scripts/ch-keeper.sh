# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-keeper.sh - ClickHouse Keeper Health and Status Monitor
# ============================================================================
# Description: Monitors ClickHouse Keeper cluster health, quorum status,
#              and coordination state. Essential for cluster coordination.
#
# Usage: ch-keeper.sh [command] [options]
#
# Commands:
#   status       Show Keeper status (default)
#   nodes        Show all Keeper nodes
#   stat         Show 4-letter 'stat' command output
#   mntr         Show 4-letter 'mntr' command output
#   conf         Show Keeper configuration
#   connections  Show active Keeper connections
#
# Options:
#   --host HOST   Keeper host (default: localhost)
#   --port PORT   Keeper port (default: 9181)
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

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        status|nodes|stat|mntr|conf|connections)
            COMMAND=$1
            shift
            ;;
        --host)
            KEEPER_HOST=$2
            shift 2
            ;;
        --port)
            KEEPER_PORT=$2
            shift 2
            ;;
        -h|--help)
            echo "ch-keeper.sh - ClickHouse Keeper Health Monitor"
            echo ""
            echo "Usage: ch-keeper.sh [command] [options]"
            echo ""
            echo "Commands:"
            echo "  status       Show Keeper status (default)"
            echo "  nodes        Show all Keeper nodes"
            echo "  stat         Show 4-letter 'stat' output"
            echo "  mntr         Show 4-letter 'mntr' output (Prometheus)"
            echo "  conf         Show Keeper configuration"
            echo "  connections  Show active connections"
            echo ""
            echo "Options:"
            echo "  --host HOST  Keeper host (default: localhost)"
            echo "  --port PORT  Keeper port (default: 9181)"
            echo "  -h, --help   Show this help message"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Alias for ClickHouse queries (using common run_query)
run_ch_query() {
    run_query "$1"
}

# ============================================================================
# Command Functions
# ============================================================================

show_status() {
    print_header "ClickHouse Keeper Status"
    
    # Check if Keeper service is running
    print_section "Service Status"
    if ch_keeper_running; then
        echo -e "  Keeper Service: ${GREEN}● Running${NC}"
    elif ch_server_running; then
        # Keeper might be embedded in ClickHouse server
        echo -e "  ClickHouse Server: ${GREEN}● Running${NC} (Keeper may be embedded)"
    else
        echo -e "  Keeper Service: ${RED}● Not Running${NC}"
        return 1
    fi
    echo ""
    
    # Get ruok response (basic health check)
    print_section "Health Check"
    local ruok
    ruok=$(send_keeper_cmd "ruok")
    if [ "$ruok" = "imok" ]; then
        echo -e "  Health Check (ruok): ${GREEN}imok${NC}"
    else
        echo -e "  Health Check (ruok): ${RED}$ruok${NC}"
    fi
    
    # Get isro (is read-only) response
    local isro
    isro=$(send_keeper_cmd "isro")
    if [ "$isro" = "rw" ]; then
        echo -e "  Read/Write Status:  ${GREEN}rw (read-write)${NC}"
    elif [ "$isro" = "ro" ]; then
        echo -e "  Read/Write Status:  ${YELLOW}ro (read-only)${NC}"
    else
        echo -e "  Read/Write Status:  ${RED}unknown${NC}"
    fi
    echo ""
    
    # Get server stats
    print_section "Server Statistics"
    local mntr
    mntr=$(send_keeper_cmd "mntr")
    
    if [ -n "$mntr" ]; then
        # Parse key metrics
        local version=$(echo "$mntr" | grep "zk_version" | cut -f2)
        local role=$(echo "$mntr" | grep "zk_server_state" | cut -f2)
        local connections=$(echo "$mntr" | grep "zk_num_alive_connections" | cut -f2)
        local outstanding=$(echo "$mntr" | grep "zk_outstanding_requests" | cut -f2)
        local znode_count=$(echo "$mntr" | grep "zk_znode_count" | cut -f2)
        local watch_count=$(echo "$mntr" | grep "zk_watch_count" | cut -f2)
        local ephemerals=$(echo "$mntr" | grep "zk_ephemerals_count" | cut -f2)
        local approx_data=$(echo "$mntr" | grep "zk_approximate_data_size" | cut -f2)
        local uptime=$(echo "$mntr" | grep "zk_uptime" | cut -f2)
        
        printf "  %-25s %s\n" "Version:" "$version"
        
        # Color code role
        if [ "$role" = "leader" ]; then
            printf "  %-25s ${GREEN}%s${NC}\n" "Server State:" "$role"
        elif [ "$role" = "follower" ]; then
            printf "  %-25s ${CYAN}%s${NC}\n" "Server State:" "$role"
        else
            printf "  %-25s ${YELLOW}%s${NC}\n" "Server State:" "$role"
        fi
        
        printf "  %-25s %s\n" "Alive Connections:" "$connections"
        printf "  %-25s %s\n" "Outstanding Requests:" "$outstanding"
        printf "  %-25s %s\n" "ZNode Count:" "$znode_count"
        printf "  %-25s %s\n" "Watch Count:" "$watch_count"
        printf "  %-25s %s\n" "Ephemeral Nodes:" "$ephemerals"
        printf "  %-25s %s bytes\n" "Approx Data Size:" "$approx_data"
        
        if [ -n "$uptime" ]; then
            local uptime_days=$((uptime / 86400000))
            local uptime_hours=$(((uptime % 86400000) / 3600000))
            printf "  %-25s %dd %dh\n" "Uptime:" "$uptime_days" "$uptime_hours"
        fi
    else
        echo -e "  ${YELLOW}Could not retrieve statistics from Keeper${NC}"
    fi
    echo ""
    
    # Check ClickHouse connection to Keeper
    print_section "ClickHouse Connection"
    local zk_conn
    zk_conn=$(run_ch_query "
        SELECT 
            host, port, is_expired, 
            formatReadableTimeDelta(session_uptime_elapsed_seconds) as uptime
        FROM system.zookeeper_connection
    " 2>/dev/null)
    
    if [ -n "$zk_conn" ]; then
        while IFS=$'\t' read -r host port expired uptime; do
            if [ -n "$host" ]; then
                if [ "$expired" = "0" ]; then
                    echo -e "  $host:$port - ${GREEN}Connected${NC} (uptime: $uptime)"
                else
                    echo -e "  $host:$port - ${RED}Session Expired${NC}"
                fi
            fi
        done <<< "$zk_conn"
    else
        echo -e "  ${YELLOW}No ClickHouse connection info available${NC}"
    fi
}

show_nodes() {
    print_header "ClickHouse Keeper Cluster Nodes"
    
    # Get configuration from mntr output
    local conf
    conf=$(send_keeper_cmd "conf")
    
    if [ -n "$conf" ]; then
        print_section "Cluster Configuration"
        echo "$conf"
    fi
    echo ""
    
    # Check each configured Keeper
    print_section "Node Health Check"
    
    # Try to get cluster info from ClickHouse
    local cluster_info
    cluster_info=$(run_ch_query "
        SELECT 
            host_name, host_address, port
        FROM system.clusters 
        WHERE cluster LIKE '%keeper%' OR cluster LIKE '%zookeeper%'
        UNION ALL
        SELECT 
            host_name, '', port
        FROM system.zookeeper_connection
    " 2>/dev/null)
    
    if [ -n "$cluster_info" ]; then
        while IFS=$'\t' read -r name addr port; do
            if [ -n "$name" ]; then
                local host="${addr:-$name}"
                local ruok
                ruok=$(echo "ruok" | nc -w 2 "$host" "${port:-9181}" 2>/dev/null || echo "failed")
                
                if [ "$ruok" = "imok" ]; then
                    echo -e "  $name:$port - ${GREEN}● Healthy${NC}"
                else
                    echo -e "  $name:$port - ${RED}● Unhealthy${NC}"
                fi
            fi
        done <<< "$cluster_info"
    else
        # Fallback to checking local Keeper
        local ruok
        ruok=$(send_keeper_cmd "ruok")
        if [ "$ruok" = "imok" ]; then
            echo -e "  $KEEPER_HOST:$KEEPER_PORT - ${GREEN}● Healthy${NC}"
        else
            echo -e "  $KEEPER_HOST:$KEEPER_PORT - ${RED}● Unhealthy${NC}"
        fi
    fi
}

show_stat() {
    print_header "Keeper 'stat' Output"
    
    local stat
    stat=$(send_keeper_cmd "stat")
    
    if [ -n "$stat" ]; then
        echo "$stat"
    else
        echo -e "${YELLOW}Could not retrieve stat from Keeper${NC}"
    fi
}

show_mntr() {
    print_header "Keeper 'mntr' Output (Prometheus Format)"
    
    local mntr
    mntr=$(send_keeper_cmd "mntr")
    
    if [ -n "$mntr" ]; then
        echo "$mntr"
    else
        echo -e "${YELLOW}Could not retrieve mntr from Keeper${NC}"
    fi
}

show_conf() {
    print_header "Keeper Configuration"
    
    local conf
    conf=$(send_keeper_cmd "conf")
    
    if [ -n "$conf" ]; then
        echo "$conf"
    else
        echo -e "${YELLOW}Could not retrieve configuration from Keeper${NC}"
    fi
    echo ""
    
    # Show config file if available
    if [ -f /etc/clickhouse-keeper/keeper.xml ]; then
        print_section "Keeper Config File"
        cat /etc/clickhouse-keeper/keeper.xml | head -100
    elif [ -f /etc/clickhouse-server/config.d/keeper.xml ]; then
        print_section "Embedded Keeper Config"
        cat /etc/clickhouse-server/config.d/keeper.xml | head -100
    fi
}

show_connections() {
    print_header "Active Keeper Connections"
    
    local cons
    cons=$(send_keeper_cmd "cons")
    
    if [ -n "$cons" ]; then
        echo "$cons"
    else
        echo -e "${YELLOW}Could not retrieve connections from Keeper${NC}"
    fi
    echo ""
    
    # Also show from ClickHouse perspective
    print_section "ClickHouse ZK Watches"
    run_ch_query "
        SELECT 
            count() as total_watches
        FROM system.zookeeper WHERE path = '/'
    " 2>/dev/null || echo "  Could not query ZooKeeper watches"
}

# ============================================================================
# Main
# ============================================================================

main() {
    case $COMMAND in
        status)
            show_status
            ;;
        nodes)
            show_nodes
            ;;
        stat)
            show_stat
            ;;
        mntr)
            show_mntr
            ;;
        conf)
            show_conf
            ;;
        connections)
            show_connections
            ;;
        *)
            echo "Unknown command: $COMMAND"
            exit 1
            ;;
    esac
}

main
