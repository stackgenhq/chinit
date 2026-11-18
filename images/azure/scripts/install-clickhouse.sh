#!/bin/bash
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# ClickHouse Installation Script for Golden Image
# ============================================================================
# This script installs ClickHouse and dependencies for the golden image.
# It mirrors the installation portion of cloud-init.sh but is designed for
# Packer image builds rather than runtime bootstrap.
#
# Environment variables:
#   CLICKHOUSE_VERSION - ClickHouse version to install (required)
#   DEBIAN_FRONTEND    - Set to noninteractive for unattended install
# ============================================================================

set -e

# ============================================================================
# Helper Functions
# ============================================================================

# Timing variables
declare -A TIMERS

log_section() {
    echo ""
    echo "============================================"
    echo "[$(date '+%H:%M:%S')] $1"
    echo "============================================"
}

log_step() {
    echo "[$(date '+%H:%M:%S')] → $1"
}

log_success() {
    echo "[$(date '+%H:%M:%S')] ✓ $1"
}

start_timer() {
    local name="$1"
    TIMERS[$name]=$(date +%s)
    log_step "Starting: $name"
}

end_timer() {
    local name="$1"
    local start=${TIMERS[$name]}
    local end=$(date +%s)
    local duration=$((end - start))
    log_success "$name completed in ${duration}s"
}

# ============================================================================
# Installation Functions
# ============================================================================

install_base_dependencies() {
    log_step "Updating apt package lists..."
    sudo apt-get update

    log_step "Installing base dependencies..."
    sudo apt-get install -y \
        apt-transport-https \
        ca-certificates \
        dirmngr \
        curl \
        wget \
        openssl \
        jq \
        gnupg \
        libxml2-utils \
        netcat-openbsd \
        bind9-dnsutils \
        linux-azure

    log_success "Base dependencies installed"
}

install_clickhouse() {
    local version="${1:?ClickHouse version required}"

    # Pre-seed debconf to skip password prompts - this is the main fix for the hang
    log_step "Pre-configuring debconf to skip password prompts..."
    sudo debconf-set-selections <<< "clickhouse-server clickhouse-server/default-password password "

    # Prevent services from starting during package installation
    log_step "Disabling service auto-start during installation..."
    sudo printf "#!/bin/sh\nexit 101\n" | sudo tee /usr/sbin/policy-rc.d > /dev/null
    sudo chmod +x /usr/sbin/policy-rc.d

    log_step "Adding ClickHouse repository..."
    local KEY_FILE=/usr/share/keyrings/clickhouse-keyring.gpg
    curl -fsSL 'https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key' | \
        sudo gpg --batch --dearmor -o $KEY_FILE

    sudo chmod 644 $KEY_FILE
    echo "deb [signed-by=$KEY_FILE] https://packages.clickhouse.com/deb stable main" | \
        sudo tee /etc/apt/sources.list.d/clickhouse.list

    log_step "Updating apt with ClickHouse repository..."
    sudo apt-get update

    log_step "Installing ClickHouse version ${version} (10min timeout)..."
    # Must explicitly install clickhouse-common-static with version to prevent apt from
    # pulling a newer version that causes dependency conflicts
    timeout 600 sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
        -o Dpkg::Options::="--force-confnew" \
        -o Dpkg::Options::="--force-confdef" \
        clickhouse-common-static="${version}" \
        clickhouse-server="${version}" \
        clickhouse-client="${version}" || {
        echo "ERROR: ClickHouse installation timed out or failed after 10 minutes"
        exit 1
    }

    # Re-enable service auto-start
    sudo rm -f /usr/sbin/policy-rc.d

    log_step "Verifying ClickHouse installation..."
    clickhouse-server --version
    clickhouse-client --version

    log_success "ClickHouse ${version} installed"
}

install_azure_cli() {
    log_step "Installing Azure CLI..."
    curl -sL https://aka.ms/InstallAzureCLIDeb | sudo bash

    log_step "Verifying Azure CLI installation..."
    az --version | head -1

    log_success "Azure CLI installed"
}

install_chinit() {
    local version="${CHINIT_VERSION:-0.2.0}"
    log_step "Installing chinit (node bootstrapper) version $version..."

    local arch
    arch=$(uname -m)
    case "$arch" in
        aarch64|arm64) arch="arm64" ;;
        x86_64|amd64)  arch="amd64" ;;
    esac

    # chinit releases are bare semver tags (v0.2.0+) — the chinit-v* prefix
    # is retired. Asset names are unchanged.
    curl -fsSL --retry 3 \
        -o /tmp/chinit "https://github.com/stackgenhq/chinit/releases/download/v${version}/chinit-linux-${arch}"
    curl -fsSL --retry 3 \
        -o /tmp/chinit-SHA256SUMS "https://github.com/stackgenhq/chinit/releases/download/v${version}/chinit-SHA256SUMS"

    # Checksum gate: the release sums name chinit-linux-<arch>; the staged
    # file is renamed chinit. Mismatch = refuse (never install).
    (cd /tmp && grep "chinit-linux-${arch}" chinit-SHA256SUMS \
        | sed "s|chinit-linux-${arch}|chinit|" | sha256sum -c -)

    sudo install -m 0755 /tmp/chinit /usr/local/bin/chinit
    rm -f /tmp/chinit /tmp/chinit-SHA256SUMS

    log_step "Verifying chinit installation..."
    /usr/local/bin/chinit version
    /usr/local/bin/chinit version | grep -q "${version}" \
        || { echo "ERROR: chinit version mismatch"; exit 1; }

    log_success "chinit $version installed"
}

# Install OpenTelemetry Collector
# Pre-installs otelcol-contrib binary for observability data collection
# Configuration is applied by cloud-init at boot time
install_otel_collector() {
    local version="${OTEL_COLLECTOR_VERSION:-0.118.0}"
    log_step "Installing OpenTelemetry Collector version $version..."
    
    wget -q "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${version}/otelcol-contrib_${version}_linux_amd64.tar.gz" -O /tmp/otelcol.tar.gz
    tar -xzf /tmp/otelcol.tar.gz -C /tmp
    sudo mv /tmp/otelcol-contrib /usr/local/bin/otelcol
    sudo chmod +x /usr/local/bin/otelcol
    rm -f /tmp/otelcol.tar.gz
    
    # Create config directory (will be populated by cloud-init)
    sudo mkdir -p /etc/otelcol
    
    log_step "Verifying OTEL Collector installation..."
    /usr/local/bin/otelcol --version
    
    log_success "OpenTelemetry Collector $version installed"
}

# Install clickhouse-backup tool
# Pre-installs the Altinity clickhouse-backup binary for backup operations
# Configuration is applied by cloud-init at boot time
install_clickhouse_backup() {
    local version="${CLICKHOUSE_BACKUP_VERSION:-2.5.0}"
    log_step "Installing clickhouse-backup version $version..."
    
    wget -q "https://github.com/Altinity/clickhouse-backup/releases/download/v${version}/clickhouse-backup-linux-amd64.tar.gz" -O /tmp/clickhouse-backup.tar.gz
    
    # Extract and find the binary (tarball structure varies by version)
    tar -xzf /tmp/clickhouse-backup.tar.gz -C /tmp
    
    # The binary may be at /tmp/clickhouse-backup or /tmp/build/linux/amd64/clickhouse-backup
    if [ -f /tmp/clickhouse-backup ]; then
        sudo mv /tmp/clickhouse-backup /usr/local/bin/clickhouse-backup
    elif [ -f /tmp/build/linux/amd64/clickhouse-backup ]; then
        sudo mv /tmp/build/linux/amd64/clickhouse-backup /usr/local/bin/clickhouse-backup
    else
        # Find the binary wherever it was extracted
        local binary_path=$(find /tmp -name "clickhouse-backup" -type f 2>/dev/null | head -1)
        if [ -n "$binary_path" ]; then
            sudo mv "$binary_path" /usr/local/bin/clickhouse-backup
        else
            echo "ERROR: clickhouse-backup binary not found after extraction"
            tar -tzf /tmp/clickhouse-backup.tar.gz | head -20
            exit 1
        fi
    fi
    
    sudo chmod +x /usr/local/bin/clickhouse-backup
    rm -rf /tmp/clickhouse-backup.tar.gz /tmp/build
    
    # Create config directory (will be populated by cloud-init)
    sudo mkdir -p /etc/clickhouse-backup
    
    log_step "Verifying clickhouse-backup installation..."
    /usr/local/bin/clickhouse-backup --version
    
    log_success "clickhouse-backup $version installed"
}

# Download OpsVerse agent installer (configuration done at boot time by cloud-init)
install_opsverse_agent() {
    log_step "Downloading OpsVerse agent installer..."
    
    sudo curl -L https://opsverse-public.s3.amazonaws.com/agent/single-node/installer.sh -o /usr/local/bin/opsverse-installer.sh
    sudo chmod +x /usr/local/bin/opsverse-installer.sh
    
    # Create targets directory for cloud-init to populate
    sudo mkdir -p /etc/opsverse/targets
    
    log_success "OpsVerse agent installer downloaded to /usr/local/bin/opsverse-installer.sh"
}

# ============================================================================
# OS Optimization Functions
# ============================================================================

disable_transparent_hugepages() {
    log_step "Disabling Transparent Huge Pages..."

    # Disable THP now
    if [ -f /sys/kernel/mm/transparent_hugepage/enabled ]; then
        echo never | sudo tee /sys/kernel/mm/transparent_hugepage/enabled > /dev/null
    fi
    if [ -f /sys/kernel/mm/transparent_hugepage/defrag ]; then
        echo never | sudo tee /sys/kernel/mm/transparent_hugepage/defrag > /dev/null
    fi

    # Make THP disable persistent across reboots
    sudo bash -c 'cat > /etc/rc.local << "RCLOCAL_EOF"
#!/bin/bash
# Disable Transparent Huge Pages for ClickHouse
if [ -f /sys/kernel/mm/transparent_hugepage/enabled ]; then
    echo never > /sys/kernel/mm/transparent_hugepage/enabled
fi
if [ -f /sys/kernel/mm/transparent_hugepage/defrag ]; then
    echo never > /sys/kernel/mm/transparent_hugepage/defrag
fi
exit 0
RCLOCAL_EOF'
    sudo chmod +x /etc/rc.local

    log_success "Transparent Huge Pages disabled"
}

configure_file_limits() {
    log_step "Configuring file descriptor limits..."

    # System-wide file descriptor limits
    sudo mkdir -p /etc/sysctl.d
    echo "fs.file-max = 2097152" | sudo tee /etc/sysctl.d/99-clickhouse.conf

    # User limits
    sudo bash -c 'cat >> /etc/security/limits.conf << EOF

# ClickHouse file descriptor limits
clickhouse soft nofile 262144
clickhouse hard nofile 262144
root soft nofile 262144
root hard nofile 262144
EOF'

    # Systemd limits for clickhouse service
    sudo mkdir -p /etc/systemd/system/clickhouse-server.service.d
    sudo bash -c 'cat > /etc/systemd/system/clickhouse-server.service.d/limits.conf << EOF
[Service]
LimitNOFILE=262144
EOF'

    log_success "File descriptor limits configured"
}

# Install clickhouse-backup-daily.sh script
install_backup_script() {
    log_step "Installing clickhouse-backup-daily.sh script..."
    
    sudo tee /usr/local/bin/clickhouse-backup-daily.sh > /dev/null <<'EOF'
#!/bin/bash
# clickhouse-backup-daily.sh
# This script performs a daily backup of ClickHouse data using clickhouse-backup.
# It is intended to be run by a systemd timer.

set -euo pipefail

LOG_FILE="/var/log/clickhouse-backup.log"
CONFIG_DIR="/etc/clickhouse-backup"
BACKUP_NAME="daily_$(date +%Y%m%d_%H%M%S)"

exec >> "$LOG_FILE" 2>&1

echo "$(date '+%Y-%m-%d %H:%M:%S') - Starting daily ClickHouse backup: $BACKUP_NAME"

# Ensure clickhouse-backup is installed
if ! command -v clickhouse-backup &> /dev/null; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') - ERROR: clickhouse-backup command not found. Exiting."
    exit 1
fi

# Ensure config directory exists
if [ ! -d "$CONFIG_DIR" ]; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') - ERROR: ClickHouse backup config directory $CONFIG_DIR not found. Exiting."
    exit 1
fi

# Perform backup
if clickhouse-backup --config "$CONFIG_DIR/config.yml" create "$BACKUP_NAME"; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Backup '$BACKUP_NAME' created successfully."
    
    # Upload backup if configured
    if clickhouse-backup --config "$CONFIG_DIR/config.yml" upload "$BACKUP_NAME"; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Backup '$BACKUP_NAME' uploaded successfully."
    else
        echo "$(date '+%Y-%m-%d %H:%M:%S') - ERROR: Failed to upload backup '$BACKUP_NAME'."
        exit 1
    fi
    
    # Delete old backups locally and remotely
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Deleting old backups..."
    clickhouse-backup --config "$CONFIG_DIR/config.yml" delete local --retention 7d || echo "$(date '+%Y-%m-%d %H:%M:%S') - Warning: Failed to delete old local backups."
    clickhouse-backup --config "$CONFIG_DIR/config.yml" delete remote --retention 30d || echo "$(date '+%Y-%m-%d %H:%M:%S') - Warning: Failed to delete old remote backups."
    
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Daily ClickHouse backup completed successfully."
else
    echo "$(date '+%Y-%m-%d %H:%M:%S') - ERROR: Failed to create backup '$BACKUP_NAME'."
    exit 1
fi

exit 0
EOF
    sudo chmod +x /usr/local/bin/clickhouse-backup-daily.sh
    log_success "clickhouse-backup-daily.sh installed"
}

# Install backup systemd service and timer (enabled/started at boot by cloud-init)
install_backup_systemd_units() {
    log_step "Installing backup systemd units..."
    
    sudo tee /etc/systemd/system/clickhouse-backup-daily.service > /dev/null <<'EOF'
[Unit]
Description=ClickHouse Daily Backup
After=clickhouse-server.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/clickhouse-backup-daily.sh
User=root

[Install]
WantedBy=multi-user.target
EOF

    sudo tee /etc/systemd/system/clickhouse-backup-daily.timer > /dev/null <<'EOF'
[Unit]
Description=ClickHouse Daily Backup Timer
Requires=clickhouse-backup-daily.service

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true

[Install]
WantedBy=timers.target
EOF
    
    log_success "Backup systemd units installed"
}

# Install backup health check script for Prometheus metrics
install_backup_health_check() {
    log_step "Installing backup health check..."
    
    sudo tee /usr/local/bin/check-backup-health.sh > /dev/null <<'HEALTH_EOF'
#!/bin/bash
# Backup Health Check Script - exports Prometheus metrics
set -e

METRICS_DIR="/var/lib/prometheus/textfile_collector"
METRICS_FILE="$METRICS_DIR/clickhouse_backup.prom"
BACKUP_LOG="/var/log/clickhouse-backup.log"

mkdir -p "$METRICS_DIR"

declare -A metrics
metrics[clickhouse_backup_last_success_timestamp]=0
metrics[clickhouse_backup_last_failure_timestamp]=0
metrics[clickhouse_backup_failure_count_24h]=0
metrics[clickhouse_backup_replica_lag_seconds]=0
metrics[clickhouse_backup_replica_readonly]=0
metrics[clickhouse_backup_health]=1

if ! command -v clickhouse-backup &> /dev/null; then
    metrics[clickhouse_backup_health]=0
fi

if [ -f "$BACKUP_LOG" ]; then
    last_success=$(grep -i "backup.*completed\|upload.*success" "$BACKUP_LOG" 2>/dev/null | tail -1 | grep -oP '^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}' || echo "")
    [ -n "$last_success" ] && metrics[clickhouse_backup_last_success_timestamp]=$(date -d "$last_success" +%s 2>/dev/null || echo 0)
    
    last_failure=$(grep -i "error\|failed" "$BACKUP_LOG" 2>/dev/null | tail -1 | grep -oP '^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}' || echo "")
    [ -n "$last_failure" ] && metrics[clickhouse_backup_last_failure_timestamp]=$(date -d "$last_failure" +%s 2>/dev/null || echo 0)
    
    metrics[clickhouse_backup_failure_count_24h]=$(grep -c -i "error\|failed" "$BACKUP_LOG" 2>/dev/null | head -1 || echo 0)
fi

if clickhouse-client --query "SELECT 1" &>/dev/null; then
    lag=$(clickhouse-client --format=TabSeparated --query "SELECT max(absolute_delay) FROM system.replicas WHERE is_readonly = 0" 2>/dev/null || echo 0)
    metrics[clickhouse_backup_replica_lag_seconds]=${lag:-0}
    
    readonly=$(clickhouse-client --format=TabSeparated --query "SELECT count() FROM system.replicas WHERE is_readonly = 1" 2>/dev/null || echo 0)
    metrics[clickhouse_backup_replica_readonly]=${readonly:-0}
    [ "$readonly" -gt 0 ] && metrics[clickhouse_backup_health]=0
else
    metrics[clickhouse_backup_health]=0
fi

{
    echo "# HELP clickhouse_backup_last_success_timestamp Unix timestamp of last successful backup"
    echo "# TYPE clickhouse_backup_last_success_timestamp gauge"
    echo "clickhouse_backup_last_success_timestamp ${metrics[clickhouse_backup_last_success_timestamp]}"
    echo "# HELP clickhouse_backup_health Overall backup health (1=healthy, 0=unhealthy)"
    echo "# TYPE clickhouse_backup_health gauge"
    echo "clickhouse_backup_health ${metrics[clickhouse_backup_health]}"
    echo "# HELP clickhouse_backup_replica_lag_seconds Maximum replication lag"
    echo "# TYPE clickhouse_backup_replica_lag_seconds gauge"
    echo "clickhouse_backup_replica_lag_seconds ${metrics[clickhouse_backup_replica_lag_seconds]}"
} > "$METRICS_FILE.tmp"
mv "$METRICS_FILE.tmp" "$METRICS_FILE"

[ "${metrics[clickhouse_backup_health]}" -eq 0 ] && exit 1
exit 0
HEALTH_EOF

    sudo chmod +x /usr/local/bin/check-backup-health.sh
    
    # Create health check systemd units
    sudo tee /etc/systemd/system/clickhouse-backup-health.service > /dev/null <<'EOF'
[Unit]
Description=ClickHouse Backup Health Check
After=clickhouse-server.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/check-backup-health.sh
User=root
EOF

    sudo tee /etc/systemd/system/clickhouse-backup-health.timer > /dev/null <<'EOF'
[Unit]
Description=ClickHouse Backup Health Check Timer

[Timer]
OnCalendar=hourly
Persistent=true

[Install]
WantedBy=timers.target
EOF
    
    log_success "Backup health check installed"
}

# Disable unnecessary systemd services that slow down boot
disable_unnecessary_services() {
    log_step "Disabling unnecessary systemd services..."
    
    # Services safe to disable for a ClickHouse server
    local services=(
        "snapd.service"           # Snap package manager - not needed
        "snapd.socket"
        "multipathd.service"      # Multipath - not needed for Azure VMs
        "ModemManager.service"    # Modem management - not needed
        "accounts-daemon.service" # Account service - not needed for server
        "apport.service"          # Crash reporting - minimal benefit for server
        # =================================================================
        # Boot time optimization: disable services that delay startup
        # Measured impact: apt-daily-upgrade=1m44s, motd-news=1m1s
        # =================================================================
        "apt-daily.service"           # Automatic apt updates - delays boot by ~6s
        "apt-daily.timer"             # Timer for apt-daily
        "apt-daily-upgrade.service"   # Automatic apt upgrades - delays boot by ~1m44s!
        "apt-daily-upgrade.timer"     # Timer for apt-daily-upgrade
        "motd-news.service"           # MOTD news fetching - delays boot by ~1m1s
        "motd-news.timer"             # Timer for motd-news
    )
    
    for svc in "${services[@]}"; do
        if systemctl is-enabled "$svc" 2>/dev/null | grep -q "enabled"; then
            sudo systemctl disable "$svc" 2>/dev/null || true
            log_step "Disabled $svc"
        fi
    done
    
    # Mask services that should never start
    sudo systemctl mask snapd.service 2>/dev/null || true
    # Mask apt-daily services to ensure they don't run during boot
    sudo systemctl mask apt-daily.service apt-daily-upgrade.service 2>/dev/null || true
    sudo systemctl mask apt-daily.timer apt-daily-upgrade.timer 2>/dev/null || true
    # Mask motd-news to prevent network calls during boot
    sudo systemctl mask motd-news.service motd-news.timer 2>/dev/null || true
    
    log_success "Unnecessary services disabled (boot time optimization: ~2m45s saved)"
}

configure_os_optimizations() {
    log_section "Configuring OS Optimizations"
    disable_transparent_hugepages
    configure_file_limits
    configure_kernel_tuning
    disable_unnecessary_services
    log_success "All OS optimizations applied"
}

# Configure kernel parameters for ClickHouse performance
configure_kernel_tuning() {
    log_step "Configuring kernel parameters for ClickHouse..."
    
    sudo bash -c 'cat >> /etc/sysctl.d/99-clickhouse.conf << EOF

# ClickHouse Performance Tuning
# Network buffer sizes for high-throughput replication
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.core.rmem_default = 16777216
net.core.wmem_default = 16777216
net.ipv4.tcp_rmem = 4096 87380 134217728
net.ipv4.tcp_wmem = 4096 87380 134217728

# Connection handling
net.core.somaxconn = 65535
net.core.netdev_max_backlog = 65535
net.ipv4.tcp_max_syn_backlog = 65535

# Memory management (reduce swapping, optimize for ClickHouse)
vm.swappiness = 1
vm.dirty_ratio = 40
vm.dirty_background_ratio = 10
vm.overcommit_memory = 0

# Increase max memory map areas (required for large ClickHouse tables)
vm.max_map_count = 262144
EOF'

    log_success "Kernel parameters configured"
}

# Configure I/O scheduler for SSDs (recommended: mq-deadline or none for NVMe)
configure_io_scheduler() {
    log_step "Configuring I/O scheduler for SSDs..."
    
    # Create udev rule to set scheduler for all block devices
    sudo bash -c 'cat > /etc/udev/rules.d/60-io-scheduler.rules << EOF
# Use mq-deadline for rotational disks, none for SSDs/NVMe
ACTION=="add|change", KERNEL=="sd[a-z]*", ATTR{queue/rotational}=="0", ATTR{queue/scheduler}="mq-deadline"
ACTION=="add|change", KERNEL=="nvme[0-9]*n[0-9]*", ATTR{queue/scheduler}="none"
EOF'
    
    log_success "I/O scheduler configured (mq-deadline for SSD, none for NVMe)"
}

# Pre-install static ClickHouse performance configs
install_clickhouse_performance_configs() {
    log_step "Installing ClickHouse performance configs..."
    
    sudo mkdir -p /etc/clickhouse-server/config.d
    
    # Performance tuning config
    sudo bash -c 'cat > /etc/clickhouse-server/config.d/performance.xml << EOF
<clickhouse>
    <!-- Connection limits -->
    <max_concurrent_queries>150</max_concurrent_queries>
    <max_connections>4096</max_connections>
    <keep_alive_timeout>3</keep_alive_timeout>
    
    <!-- Memory limits (will be overridden by cloud-init for specific node types) -->
    <max_server_memory_usage_to_ram_ratio>0.9</max_server_memory_usage_to_ram_ratio>
    
    <!-- MergeTree optimizations -->
    <merge_tree>
        <!-- Keep data in wide parts to reduce CPU during merges -->
        <min_rows_for_wide_part>0</min_rows_for_wide_part>
        <min_bytes_for_wide_part>0</min_bytes_for_wide_part>
        <!-- Reduce IO amplification -->
        <number_of_free_entries_in_pool_to_lower_max_size_of_merge>0</number_of_free_entries_in_pool_to_lower_max_size_of_merge>
    </merge_tree>
</clickhouse>
EOF'
    
    sudo chown -R clickhouse:clickhouse /etc/clickhouse-server/config.d/
    log_success "ClickHouse performance configs installed"
}

# ============================================================================
# Summary Function
# ============================================================================

print_summary() {
    log_section "Golden Image Build Complete"
    echo "Installed components:"
    echo "  - ClickHouse Server: ${CLICKHOUSE_VERSION}"
    echo "  - ClickHouse Client: ${CLICKHOUSE_VERSION}"
    echo "  - Azure CLI: $(az --version 2>/dev/null | head -1 | awk '{print $2}')"
    echo ""
    echo "OS optimizations applied:"
    echo "  - Transparent Huge Pages: disabled"
    echo "  - File descriptor limit: 262144"
    echo "  - System file-max: 2097152"
    echo "  - Network buffers: optimized for high-throughput"
    echo "  - vm.swappiness: 1 (minimize swapping)"
    echo "  - vm.max_map_count: 262144 (for large tables)"
    echo "  - I/O scheduler: mq-deadline (SSD), none (NVMe)"
    echo ""
    echo "ClickHouse pre-configs:"
    echo "  - Performance tuning (max_connections, MergeTree)"
    echo ""
    echo "Additional tools:"
    echo "  - OpenTelemetry Collector: ${OTEL_COLLECTOR_VERSION:-0.118.0}"
    echo "  - clickhouse-backup: ${CLICKHOUSE_BACKUP_VERSION:-2.5.0}"
    echo "============================================"
}

# ============================================================================
# Main
# ============================================================================

main() {
    local BUILD_START=$(date +%s)
    
    log_section "ClickHouse Golden Image Build Script"
    echo "ClickHouse Version: ${CLICKHOUSE_VERSION}"
    echo "Build started at: $(date '+%Y-%m-%d %H:%M:%S')"

    start_timer "Base Dependencies"
    log_section "Installing Base Dependencies"
    install_base_dependencies
    end_timer "Base Dependencies"

    # Note: Parallel apt installs cause lock conflicts, running sequentially
    start_timer "ClickHouse"
    log_section "Installing ClickHouse"
    install_clickhouse "${CLICKHOUSE_VERSION}"
    end_timer "ClickHouse"

    start_timer "Azure CLI"
    log_section "Installing Azure CLI"
    install_azure_cli
    end_timer "Azure CLI"

    # chinit carries every cluster operation in-process since v0.2.0 —
    # one binary, no separate control CLI.
    start_timer "chinit"
    log_section "Installing chinit (node bootstrapper)"
    install_chinit
    end_timer "chinit"

    start_timer "OS Optimizations"
    configure_os_optimizations
    configure_io_scheduler
    end_timer "OS Optimizations"

    start_timer "ClickHouse Configs"
    install_clickhouse_performance_configs
    end_timer "ClickHouse Configs"

    start_timer "OTEL Collector"
    log_section "Installing OpenTelemetry Collector"
    install_otel_collector
    end_timer "OTEL Collector"

    start_timer "clickhouse-backup"
    log_section "Installing clickhouse-backup"
    install_clickhouse_backup
    end_timer "clickhouse-backup"

    start_timer "OpsVerse Agent"
    log_section "Downloading OpsVerse Agent"
    install_opsverse_agent
    end_timer "OpsVerse Agent"

    start_timer "Backup Scripts"
    log_section "Installing Backup Scripts"
    install_backup_script
    install_backup_systemd_units
    install_backup_health_check
    end_timer "Backup Scripts"

    # Calculate total build time
    local BUILD_END=$(date +%s)
    local TOTAL_TIME=$((BUILD_END - BUILD_START))
    
    print_summary
    
    echo ""
    echo "============================================"
    echo "TIMING SUMMARY"
    echo "============================================"
    echo "Total provisioning time: ${TOTAL_TIME}s"
    echo "Build ended at: $(date '+%Y-%m-%d %H:%M:%S')"
    echo "============================================"
}

# Run main function
main "$@"
