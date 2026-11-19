# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

#!/bin/bash
# ============================================================================
# ch-common.sh - Common Functions for ClickHouse SRE Scripts
# ============================================================================
# Description: Shared utility functions, constants, and configuration for
#              all ClickHouse SRE troubleshooting scripts.
#
# Usage: Source this file at the beginning of other scripts:
#        SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
#        source "${SCRIPT_DIR}/ch-common.sh"
#
# Author: SRE Team
# Version: 1.0.0
# ============================================================================

# Prevent multiple sourcing
[[ -n "${_CH_COMMON_LOADED:-}" ]] && return 0
_CH_COMMON_LOADED=1

# ============================================================================
# TERMINAL COLORS
# ============================================================================
# ANSI color codes for terminal output
# Usage: echo -e "${RED}Error${NC}" or printf "${GREEN}OK${NC}\n"
# ============================================================================
export RED='\033[0;31m'
export GREEN='\033[0;32m'
export YELLOW='\033[1;33m'
export BLUE='\033[0;34m'
export CYAN='\033[0;36m'
export MAGENTA='\033[0;35m'
export WHITE='\033[1;37m'
export BOLD='\033[1m'
export NC='\033[0m'  # No Color / Reset

# ============================================================================
# CLICKHOUSE CLIENT CONFIGURATION
# ============================================================================
# Default configuration for ClickHouse client connections.
# These can be overridden by environment variables.
# ============================================================================
export CLICKHOUSE_CLIENT="${CLICKHOUSE_CLIENT:-clickhouse-client}"
export CLICKHOUSE_HOST="${CLICKHOUSE_HOST:-localhost}"
export CLICKHOUSE_PORT="${CLICKHOUSE_PORT:-9440}"
export CLICKHOUSE_USER="${CLICKHOUSE_USER:-default}"
export CLICKHOUSE_SECURE="${CLICKHOUSE_SECURE:-true}"

# ============================================================================
# KEEPER CONFIGURATION
# ============================================================================
# Default configuration for ClickHouse Keeper connections.
# ============================================================================
export KEEPER_HOST="${KEEPER_HOST:-localhost}"
export KEEPER_PORT="${KEEPER_PORT:-9181}"

# ============================================================================
# CLICKHOUSE CLIENT FUNCTIONS
# ============================================================================

# Build the clickhouse-client command with proper arguments
# Usage: CH_CMD=$(build_ch_cmd)
# Returns: Full clickhouse-client command string
build_ch_cmd() {
    local cmd="$CLICKHOUSE_CLIENT --host $CLICKHOUSE_HOST --port $CLICKHOUSE_PORT --user $CLICKHOUSE_USER"
    if [ "$CLICKHOUSE_SECURE" = "true" ]; then
        cmd="$cmd --secure"
    fi
    echo "$cmd"
}

# Execute a ClickHouse query
# Usage: run_query "SELECT 1"
# Returns: Query result or empty on error
run_query() {
    local query="$1"
    local ch_cmd
    ch_cmd=$(build_ch_cmd)
    $ch_cmd --query="$query" 2>/dev/null
}

# Execute a ClickHouse query with error output
# Usage: run_query_verbose "SELECT 1"
# Returns: Query result, errors go to stderr
run_query_verbose() {
    local query="$1"
    local ch_cmd
    ch_cmd=$(build_ch_cmd)
    $ch_cmd --query="$query"
}

# Check if ClickHouse is reachable
# Usage: if ch_is_reachable; then echo "OK"; fi
# Returns: 0 if reachable, 1 otherwise
ch_is_reachable() {
    run_query "SELECT 1" >/dev/null 2>&1
}

# ============================================================================
# KEEPER FUNCTIONS
# ============================================================================

# Send a 4-letter command to ClickHouse Keeper
# Usage: send_keeper_cmd "ruok"
# Returns: Command response or empty on error
send_keeper_cmd() {
    local cmd="$1"
    local host="${2:-$KEEPER_HOST}"
    local port="${3:-$KEEPER_PORT}"
    echo "$cmd" | nc -w 5 "$host" "$port" 2>/dev/null
}

# Check if Keeper is healthy
# Usage: if keeper_is_healthy; then echo "OK"; fi
# Returns: 0 if healthy (imok), 1 otherwise
keeper_is_healthy() {
    local response
    response=$(send_keeper_cmd "ruok")
    [ "$response" = "imok" ]
}

# ============================================================================
# LOGGING FUNCTIONS
# ============================================================================

# Log an informational message
# Usage: log_info "Starting process..."
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

# Log a success message
# Usage: log_success "Task completed"
log_success() {
    echo -e "${GREEN}[✓]${NC} $*"
}

# Log a warning message
# Usage: log_warn "Potential issue detected"
log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

# Log an error message
# Usage: log_error "Something went wrong"
log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

# Log a debug message (only if DEBUG=1)
# Usage: log_debug "Variable value: $var"
log_debug() {
    if [ "${DEBUG:-0}" = "1" ]; then
        echo -e "${MAGENTA}[DEBUG]${NC} $*" >&2
    fi
}

# ============================================================================
# OUTPUT FORMATTING FUNCTIONS
# ============================================================================

# Print a header banner
# Usage: print_header "Script Title" "Subtitle (optional)"
print_header() {
    local title="$1"
    local subtitle="${2:-$(date '+%Y-%m-%d %H:%M:%S')}"
    local hostname_str="Host: $(hostname)"
    
    echo ""
    echo -e "${BLUE}╔══════════════════════════════════════════════════════════════════════╗${NC}"
    printf "${BLUE}║${NC}  %-68s  ${BLUE}║${NC}\n" "$title"
    printf "${BLUE}║${NC}  %-68s  ${BLUE}║${NC}\n" "$subtitle"
    printf "${BLUE}║${NC}  %-68s  ${BLUE}║${NC}\n" "$hostname_str"
    echo -e "${BLUE}╚══════════════════════════════════════════════════════════════════════╝${NC}"
    echo ""
}

# Print a section header
# Usage: print_section "Section Name"
print_section() {
    local title="$1"
    echo -e "${CYAN}═══════════════════ $title ═══════════════════${NC}"
}

# Print a subsection header
# Usage: print_subsection "Subsection Name"
print_subsection() {
    local title="$1"
    echo -e "${BLUE}─────────────── $title ───────────────${NC}"
}

# Print a status line with colored status
# Usage: print_status "Check Name" "PASS|WARN|FAIL" "Message"
print_status() {
    local name="$1"
    local status="$2"
    local message="$3"
    
    local color=$GREEN
    local symbol="✓"
    
    case "$status" in
        PASS|OK|pass|ok)
            color=$GREEN
            symbol="✓"
            ;;
        WARN|warn|WARNING|warning)
            color=$YELLOW
            symbol="⚠"
            ;;
        FAIL|fail|ERROR|error|CRITICAL|critical)
            color=$RED
            symbol="✗"
            ;;
        INFO|info)
            color=$BLUE
            symbol="ℹ"
            ;;
    esac
    
    printf "${color}[${symbol}]${NC} %-30s %s\n" "$name:" "$message"
}

# Print a key-value pair
# Usage: print_kv "Key" "Value"
print_kv() {
    local key="$1"
    local value="$2"
    printf "  %-25s %s\n" "$key:" "$value"
}

# Print a colored key-value pair based on threshold
# Usage: print_kv_threshold "CPU Usage" "85" "%" 70 90
#        (yellow if >= 70, red if >= 90)
print_kv_threshold() {
    local key="$1"
    local value="$2"
    local unit="${3:-}"
    local warn_threshold="${4:-70}"
    local crit_threshold="${5:-90}"
    
    local color=$GREEN
    local numeric_value="${value%.*}"  # Remove decimal for comparison
    
    if [ "$numeric_value" -ge "$crit_threshold" ] 2>/dev/null; then
        color=$RED
    elif [ "$numeric_value" -ge "$warn_threshold" ] 2>/dev/null; then
        color=$YELLOW
    fi
    
    printf "  %-25s ${color}%s${unit}${NC}\n" "$key:" "$value"
}

# ============================================================================
# UTILITY FUNCTIONS
# ============================================================================

# Check if a command exists
# Usage: if command_exists "jq"; then ... fi
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Check if running as root
# Usage: if is_root; then ... fi
is_root() {
    [ "$(id -u)" -eq 0 ]
}

# Get the directory where the script is located
# Usage: SCRIPT_DIR=$(get_script_dir)
get_script_dir() {
    cd "$(dirname "${BASH_SOURCE[1]}")" && pwd
}

# Parse common arguments (--help, --version)
# Usage: parse_common_args "$@" "script-name" "1.0.0" "Script description"
parse_common_args() {
    local args=("$@")
    local script_name="${args[-3]}"
    local version="${args[-2]}"
    local description="${args[-1]}"
    
    for arg in "${args[@]:0:$((${#args[@]}-3))}"; do
        case "$arg" in
            -h|--help)
                echo "$script_name - $description"
                echo ""
                echo "Version: $version"
                echo ""
                echo "Environment Variables:"
                echo "  CLICKHOUSE_HOST    ClickHouse server host (default: localhost)"
                echo "  CLICKHOUSE_PORT    ClickHouse server port (default: 9440)"
                echo "  CLICKHOUSE_USER    ClickHouse user (default: default)"
                echo "  CLICKHOUSE_SECURE  Use TLS (default: true)"
                exit 0
                ;;
            -V|--version)
                echo "$script_name version $version"
                exit 0
                ;;
        esac
    done
}

# Format bytes to human readable
# Usage: format_bytes 1073741824  # Returns "1.00 GiB"
format_bytes() {
    local bytes="$1"
    if [ -z "$bytes" ] || [ "$bytes" = "0" ]; then
        echo "0 B"
        return
    fi
    
    local units=("B" "KiB" "MiB" "GiB" "TiB" "PiB")
    local unit=0
    local value=$bytes
    
    while [ "$value" -ge 1024 ] && [ "$unit" -lt 5 ]; do
        value=$((value / 1024))
        unit=$((unit + 1))
    done
    
    echo "${value} ${units[$unit]}"
}

# Format duration in seconds to human readable
# Usage: format_duration 3661  # Returns "1h 1m 1s"
format_duration() {
    local seconds="$1"
    if [ -z "$seconds" ] || [ "$seconds" = "0" ]; then
        echo "0s"
        return
    fi
    
    local days=$((seconds / 86400))
    local hours=$(((seconds % 86400) / 3600))
    local minutes=$(((seconds % 3600) / 60))
    local secs=$((seconds % 60))
    
    local result=""
    [ "$days" -gt 0 ] && result="${days}d "
    [ "$hours" -gt 0 ] && result="${result}${hours}h "
    [ "$minutes" -gt 0 ] && result="${result}${minutes}m "
    [ "$secs" -gt 0 ] && result="${result}${secs}s"
    
    echo "${result:-0s}"
}

# ============================================================================
# SERVICE CHECK FUNCTIONS
# ============================================================================

# Check if ClickHouse server is running
# Usage: if ch_server_running; then ... fi
ch_server_running() {
    systemctl is-active --quiet clickhouse-server 2>/dev/null
}

# Check if ClickHouse Keeper is running
# Usage: if ch_keeper_running; then ... fi
ch_keeper_running() {
    systemctl is-active --quiet clickhouse-keeper 2>/dev/null
}

# ============================================================================
# INITIALIZATION
# ============================================================================

# Initialize the CH_CMD variable for scripts that need it
CH_CMD=$(build_ch_cmd)
export CH_CMD
