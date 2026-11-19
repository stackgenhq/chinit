<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# ClickHouse SRE Scripts

This directory contains SRE troubleshooting and monitoring scripts for ClickHouse clusters. These scripts are automatically deployed to `~/sre/scripts/` on all ClickHouse VMs during bootstrap.

## Scripts Overview

| Script | Purpose | Quick Reference |
|--------|---------|-----------------|
| `ch-common.sh` | Shared functions and utilities | Sourced by all scripts |
| `ch-health.sh` | Comprehensive health check | `ch-health.sh --json` |
| `ch-status.sh` | Quick service status | `ch-status.sh --watch` |
| `ch-diag.sh` | Diagnostic collector | `ch-diag.sh --include-logs` |
| `ch-replication.sh` | Replication monitoring | `ch-replication.sh --alerts-only` |
| `ch-queries.sh` | Query monitoring/management | `ch-queries.sh running --watch` |
| `ch-keeper.sh` | Keeper cluster health | `ch-keeper.sh status` |
| `ch-disk.sh` | Disk and storage health | `ch-disk.sh cleanup` |

## Common Helper (ch-common.sh)

The `ch-common.sh` file provides shared functions and utilities used by all scripts:

### What It Provides

**Terminal Colors:**
- `$RED`, `$GREEN`, `$YELLOW`, `$BLUE`, `$CYAN`, `$NC` (no color)

**ClickHouse Configuration:**
- Pre-configured environment variables for connection settings
- `$CH_CMD` - ready-to-use clickhouse-client command

**Core Functions:**
- `build_ch_cmd` - Build clickhouse-client command with proper arguments
- `run_query "SQL"` - Execute a ClickHouse query
- `ch_is_reachable` - Check if ClickHouse is accessible
- `ch_server_running` - Check if clickhouse-server service is running
- `ch_keeper_running` - Check if clickhouse-keeper service is running

**Keeper Functions:**
- `send_keeper_cmd "cmd"` - Send 4-letter command to Keeper
- `keeper_is_healthy` - Check if Keeper responds with "imok"

**Logging Functions:**
- `log_info "message"` - Blue informational message
- `log_success "message"` - Green success message
- `log_warn "message"` - Yellow warning message
- `log_error "message"` - Red error message
- `log_debug "message"` - Debug message (only if DEBUG=1)

**Output Formatting:**
- `print_header "Title"` - Print a formatted header banner
- `print_section "Title"` - Print a section separator
- `print_status "Name" "PASS|WARN|FAIL" "Message"` - Print status line
- `print_kv "Key" "Value"` - Print key-value pair

**Utility Functions:**
- `format_duration 3661` - Format seconds to "1h 1m 1s"
- `format_bytes 1073741824` - Format bytes to "1.00 GiB"
- `command_exists "cmd"` - Check if command exists

### Using in Custom Scripts

```bash
#!/bin/bash
# My custom script

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/ch-common.sh"

# Now you can use all common functions
print_header "My Custom Check"

if ch_is_reachable; then
    log_success "Connected to ClickHouse"
    run_query "SELECT version()"
else
    log_error "Cannot connect to ClickHouse"
fi
```

## Quick Start

### Option 1: Remote Execution via Azure CLI (Recommended)

No SSH required - scripts can be run remotely using Azure Run Command:

```bash
# From your local machine (in iac/clickhouse directory)
make sre-list/sks                    # List all nodes
make sre-health/sks                  # Health check (default: writer-0)
make sre-health/sks NODE=reader-0    # Health check on reader
make sre-queries/sks ARGS="slow"     # Slow queries report
make sre-cluster-health/sks          # Quick cluster summary
```

Or use Azure CLI directly:

```bash
az vmss run-command invoke \
  --resource-group $RG \
  --name "vmss-${CLUSTER}-writers" \
  --instance-id 0 \
  --command-id RunShellScript \
  --scripts "/home/clickhouseadmin/sre/scripts/ch-health.sh --json" \
  --query "value[0].message" -o tsv
```

### Option 2: SSH into VM

After SSHing into a ClickHouse VM:

```bash
# Add scripts to PATH
export PATH="$PATH:$HOME/sre/scripts"

# Or add to .bashrc for persistence
echo 'export PATH="$PATH:$HOME/sre/scripts"' >> ~/.bashrc
source ~/.bashrc

# Run health check
ch-health.sh

# Watch service status
ch-status.sh --watch
```

## Script Details

### ch-health.sh - Comprehensive Health Check

Performs comprehensive health checks including:
- Service status
- Connectivity
- Memory usage
- Replication health
- Disk space
- Recent errors

```bash
# Basic health check
ch-health.sh

# JSON output (for automation)
ch-health.sh --json

# Verbose output with error details
ch-health.sh --verbose
```

**Exit Codes:**
- `0` - All checks passed (healthy)
- `1` - One or more checks failed (unhealthy)
- `2` - ClickHouse service not running

### ch-status.sh - Quick Service Status

Provides a quick dashboard-style view of ClickHouse status.

```bash
# One-time status check
ch-status.sh

# Continuous monitoring
ch-status.sh --watch

# Custom refresh interval
ch-status.sh --watch --interval 10
```

### ch-diag.sh - Diagnostic Collector

Collects diagnostic information for troubleshooting and support.

```bash
# Basic diagnostic collection
ch-diag.sh

# Include recent logs
ch-diag.sh --include-logs

# Include configurations (passwords are masked)
ch-diag.sh --include-configs

# Custom output location
ch-diag.sh --output /tmp/my-diag

# Full diagnostic bundle
ch-diag.sh --include-logs --include-configs
```

**Output:** Creates a tarball at `/tmp/ch-diag-<timestamp>.tar.gz`

### ch-replication.sh - Replication Monitor

Monitors replication health across all replicated tables.

```bash
# Full replication status
ch-replication.sh

# Show only tables with issues
ch-replication.sh --alerts-only

# Continuous monitoring
ch-replication.sh --watch

# Custom refresh interval
ch-replication.sh --watch --interval 30
```

### ch-queries.sh - Query Monitor

Monitors and manages running queries.

```bash
# Show running queries
ch-queries.sh running

# Watch running queries
ch-queries.sh running --watch

# Show slow queries (last hour)
ch-queries.sh slow

# Show slow queries with custom threshold
ch-queries.sh slow --min-duration 10000

# Show query errors
ch-queries.sh errors

# Show top resource-consuming queries
ch-queries.sh top

# Kill a query
ch-queries.sh kill --query-id "abc-123-def"
```

### ch-keeper.sh - Keeper Health

Monitors ClickHouse Keeper cluster health.

```bash
# Show Keeper status
ch-keeper.sh status

# Show all Keeper nodes
ch-keeper.sh nodes

# Show 4-letter command outputs
ch-keeper.sh stat
ch-keeper.sh mntr

# Show Keeper configuration
ch-keeper.sh conf

# Show active connections
ch-keeper.sh connections

# Connect to specific Keeper
ch-keeper.sh status --host keeper-0 --port 9181
```

### ch-disk.sh - Disk Monitor

Monitors disk usage, storage policies, and Azure Blob Storage.

```bash
# Show disk status overview
ch-disk.sh status

# Show table sizes
ch-disk.sh tables

# Show partition information
ch-disk.sh partitions

# Show parts information
ch-disk.sh parts

# Show Azure Blob Storage status
ch-disk.sh blob

# Show cleanup recommendations
ch-disk.sh cleanup

# Filter by database
ch-disk.sh tables --database my_database

# Limit results
ch-disk.sh tables --limit 50
```

## Environment Variables

All scripts support the following environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CLICKHOUSE_HOST` | `localhost` | ClickHouse server host |
| `CLICKHOUSE_PORT` | `9440` | ClickHouse server port (TLS) |
| `CLICKHOUSE_USER` | `default` | ClickHouse user |
| `CLICKHOUSE_SECURE` | `true` | Use TLS connection |
| `KEEPER_HOST` | `localhost` | Keeper host (for ch-keeper.sh) |
| `KEEPER_PORT` | `9181` | Keeper port (for ch-keeper.sh) |

Example:

```bash
export CLICKHOUSE_USER=admin
ch-health.sh
```

## Common Troubleshooting Scenarios

### High CPU Usage

```bash
# Check running queries
ch-queries.sh running --watch

# Identify resource-heavy queries
ch-queries.sh top

# Kill problematic query if needed
ch-queries.sh kill --query-id "..."
```

### Disk Space Issues

```bash
# Check disk status
ch-disk.sh status

# Find large tables
ch-disk.sh tables

# Get cleanup recommendations
ch-disk.sh cleanup
```

### Replication Lag

```bash
# Check replication status
ch-replication.sh --alerts-only

# Monitor continuously
ch-replication.sh --watch

# Check Keeper connection
ch-keeper.sh status
```

### Service Won't Start

```bash
# Check service status
systemctl status clickhouse-server

# Check health
ch-health.sh

# Collect diagnostics
ch-diag.sh --include-logs --include-configs

# Check Keeper if cluster mode
ch-keeper.sh status
```

### Slow Queries

```bash
# Find slow queries
ch-queries.sh slow --min-duration 5000

# Check disk I/O
ch-disk.sh status

# Check for merges blocking queries
ch-disk.sh parts
```

## Integration with Monitoring

### JSON Output for Prometheus

```bash
# Health check in JSON format
ch-health.sh --json | jq '.status'

# Parse in monitoring scripts
status=$(ch-health.sh --json | jq -r '.status')
if [ "$status" != "healthy" ]; then
    # Alert
fi
```

### Keeper Metrics

```bash
# Get Prometheus-format metrics
ch-keeper.sh mntr
```

## License

These scripts are part of the ClickHouse on Azure infrastructure and are provided under the same license as the main project.
