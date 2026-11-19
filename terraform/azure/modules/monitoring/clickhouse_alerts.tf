# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse-Specific Monitoring Alerts
# ============================================================================
# Why: These alerts monitor ClickHouse-specific health indicators that are
#      critical for cluster stability and data integrity. VM-level metrics
#      (CPU, Memory) are not sufficient - ClickHouse can be unhealthy even
#      when the VM appears healthy.
# Reference: BLINDSPOTS.md items #21, #22, #23, #31
# ============================================================================

# ============================================================================
# Keeper Quorum Loss Alert (BLINDSPOTS.md #22 - CRITICAL)
# ============================================================================
# Why: ClickHouse Keeper quorum loss is CRITICAL - it causes:
#      - ReplicatedMergeTree tables become read-only
#      - Distributed DDL commands fail
#      - New inserts may fail
#      - Cluster coordination completely stops
# Detection: Query syslog for Keeper connection failures or read-only warnings.
# Without it: Quorum loss undetected until application failures occur.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "keeper_quorum_loss" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-keeper-quorum-loss"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "CRITICAL: ClickHouse Keeper quorum may be lost. ReplicatedMergeTree tables may be read-only."
  severity            = 0 # Critical (highest severity)
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  # Query for Keeper-related errors in syslog
  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(5m)
      | where SyslogMessage contains "Keeper" or SyslogMessage contains "keeper" or SyslogMessage contains "ZooKeeper" or SyslogMessage contains "zookeeper"
      | where SyslogMessage contains "connection" and (
          SyslogMessage contains "lost" or 
          SyslogMessage contains "failed" or 
          SyslogMessage contains "refused" or
          SyslogMessage contains "timeout" or
          SyslogMessage contains "Cannot connect"
      )
      | summarize KeeperErrors = count() by Computer
      | where KeeperErrors > 5
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# Keeper Node Down Alert
# Why: Monitors for Keeper nodes that are not responding. Even a single Keeper
#      down reduces fault tolerance. 2 out of 3 Keepers down = quorum loss.
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "keeper_node_down" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-keeper-node-down"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: A ClickHouse Keeper node may be down. Reduced fault tolerance."
  severity            = 1 # High
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  # Check for Keeper nodes not reporting logs recently
  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(10m)
      | where Computer contains "keeper"
      | summarize LastSeen = max(TimeGenerated) by Computer
      | where LastSeen < ago(5m)
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    # Azure Monitor requires number_of_evaluation_periods = 1 when query
    # doesn't project TimeGenerated column
    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Replication Lag Alert (BLINDSPOTS.md #21)
# ============================================================================
# Why: Replication lag indicates data inconsistency between replicas.
#      High lag means reads from different replicas return different data.
#      Can indicate network issues, disk I/O problems, or overloaded nodes.
# Detection: Query syslog for replication delay warnings.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "replication_lag" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-replication-lag"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: ClickHouse replication lag detected. Data may be inconsistent across replicas."
  severity            = 2 # Warning
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(10m)
      | where SyslogMessage contains "replica" and (
          SyslogMessage contains "delay" or 
          SyslogMessage contains "lag" or 
          SyslogMessage contains "behind" or
          SyslogMessage contains "Readonly replica"
      )
      | summarize ReplicationIssues = count() by Computer
      | where ReplicationIssues > 3
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# ClickHouse Service Down Alert
# ============================================================================
# Why: Detects when ClickHouse service is not running on a node.
#      Service can crash due to OOM, configuration errors, or corruption.
# Detection: Query syslog for service failure messages.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "clickhouse_service_down" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-clickhouse-service-down"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "CRITICAL: ClickHouse service appears to be down on one or more nodes."
  severity            = 0 # Critical
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(5m)
      | where SyslogMessage contains "clickhouse" and (
          SyslogMessage contains "Stopped" or 
          SyslogMessage contains "Failed" or 
          SyslogMessage contains "exit" or
          SyslogMessage contains "killed" or
          SyslogMessage contains "Killed process"
      )
      | where ProcessName == "systemd" or ProcessName contains "kernel"
      | summarize ServiceIssues = count() by Computer
      | where ServiceIssues > 0
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Query Failure Rate Alert (BLINDSPOTS.md #21)
# ============================================================================
# Why: High query failure rate indicates application issues, resource
#      exhaustion, or configuration problems. Correlates with user experience.
# Detection: Query syslog for exception/error messages from ClickHouse.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "query_failures" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-query-failures"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: High rate of query failures detected in ClickHouse."
  severity            = 2 # Warning
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(15m)
      | where SyslogMessage contains "clickhouse" or ProcessName contains "clickhouse"
      | where SeverityLevel in ("error", "err", "crit")
      | where SyslogMessage contains "exception" or SyslogMessage contains "Exception" or SyslogMessage contains "QUERY_KILLED" or SyslogMessage contains "MEMORY_LIMIT"
      | summarize QueryErrors = count() by Computer
      | where QueryErrors > 10
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Backup Verification Alert (BLINDSPOTS.md #31)
# ============================================================================
# Why: Backups that are never verified may be corrupted or incomplete.
#      This alert triggers if no successful backup verification is detected
#      within the configured threshold.
# Note: Basic backup failure and missing alerts are in backup_alerts.tf.
#       This alert specifically monitors for verification/restore tests.
# Detection: Query syslog for backup verification success/failure messages.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "backup_verification_missing" {
  count               = var.backup_config.enable_verification_alerts ? 1 : 0
  name                = "${var.cluster_name}-backup-verification-missing"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: No successful backup verification detected in the last ${var.backup_config.verification_threshold_days} days. Consider running a restore test."
  severity            = 2 # Warning
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "P1D" # Evaluate daily
  window_duration      = "P1D" # Look at last day
  scopes               = [module.log_analytics_workspace.resource.id]

  # Skip validation due to dynamic day count
  skip_query_validation = true

  # Look for backup verification success markers
  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(${var.backup_config.verification_threshold_days}d)
      | where SyslogMessage has_any ("backup", "restore")
      | where SyslogMessage has_any ("verified", "verification successful", "restore test", "restore completed", "integrity check")
      | summarize VerificationCount = count()
      | where VerificationCount == 0
      | extend AlertReason = "No backup verification in last ${var.backup_config.verification_threshold_days} days"
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Certificate Expiration Alert (BLINDSPOTS.md #23)
# ============================================================================
# Why: TLS certificates must be renewed before expiration to prevent:
#      - Service unavailability (TLS handshake failures)
#      - Client connection failures
#      - Inter-node communication failures
# Detection: Query Key Vault diagnostic logs for certificate expiration events,
#            or use a scheduled query to check certificate validity.
# Note: This uses syslog to detect certificate-related warnings/errors.
#       For more reliable cert monitoring, integrate with Key Vault expiration events.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "certificate_expiration_warning" {
  count               = var.monitoring_config.enable_certificate_alerts ? 1 : 0
  name                = "${var.cluster_name}-certificate-expiration"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: TLS certificate may be expiring soon. Check Key Vault and renew certificates."
  severity            = 2 # Warning
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT1H"
  window_duration      = "PT1H"
  scopes               = [module.log_analytics_workspace.resource.id]

  # Look for certificate-related warnings in syslog
  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(1h)
      | where SyslogMessage contains "certificate" and (
          SyslogMessage contains "expir" or 
          SyslogMessage contains "expired" or
          SyslogMessage contains "invalid" or
          SyslogMessage contains "not valid"
      )
      | summarize CertIssues = count() by Computer
      | where CertIssues > 0
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Out of Memory (OOM) Alert
# ============================================================================
# Why: OOM kills can crash ClickHouse without warning, causing query failures
#      and potential data corruption if writes are interrupted.
# Detection: Query syslog for OOM killer messages from the kernel.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "oom_killer" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-oom-killer"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "CRITICAL: OOM killer activated on ClickHouse node. Service may be down."
  severity            = 0 # Critical
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(5m)
      | where SyslogMessage contains "Out of memory" or SyslogMessage contains "oom-kill" or SyslogMessage contains "Killed process"
      | summarize OOMEvents = count() by Computer
      | where OOMEvents > 0
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Cloud-Init Failure Alert (BLINDSPOTS.md #24)
# ============================================================================
# Why: Cloud-init failures leave VMs in inconsistent state. ClickHouse may
#      not be installed/configured, but the VM appears healthy to Azure.
# Detection: Query syslog for cloud-init error messages.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "cloud_init_failure" {
  count = 0 # DISABLED: Informational alert
  # count               = var.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-cloud-init-failure"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: Cloud-init may have failed. VM may not be properly configured."
  severity            = 1 # High
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT30M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(30m)
      | where ProcessName contains "cloud-init" or SyslogMessage contains "cloud-init"
      | where SeverityLevel in ("error", "err", "crit") or SyslogMessage contains "failed" or SyslogMessage contains "FAILED"
      | summarize CloudInitErrors = count() by Computer
      | where CloudInitErrors > 0
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Cache Thrashing Alert (Azure Blob Storage Performance)
# ============================================================================
# Why: When local cache is too small for the working set, ClickHouse:
#      - Constantly downloads parts from Azure Blob Storage
#      - Evicts recently downloaded data to make room
#      - Re-downloads the same data moments later
#      This "thrashing" saturates network bandwidth and kills performance.
# Detection: Query ClickHouse system.events for DiskCacheEvictions metric via
#            Prometheus endpoint scraped to Log Analytics, or syslog mentions.
# Reference: BLINDSPOTS.md - Monitor DiskCacheEvictions in system.events
# Without it: Performance degrades silently with no alerting. Users experience
#             slow queries without visibility into the root cause.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "cache_thrashing" {
  count = 0 # DISABLED: Informational alert
  # count               = var.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-cache-thrashing"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: High cache eviction rate detected. Local cache may be too small for workload. Consider increasing cache disk size."
  severity            = 2 # Warning
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  # Look for cache eviction warnings in syslog
  # Alternative: Use Prometheus metrics if scraped to Log Analytics
  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(15m)
      | where SyslogMessage contains "cache" and (
          SyslogMessage contains "evict" or 
          SyslogMessage contains "Evicting" or
          SyslogMessage contains "space" or
          SyslogMessage contains "disk is full"
      )
      | summarize CacheEvictions = count() by Computer
      | where CacheEvictions > 50
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}

# ============================================================================
# Low Disk Space Alert
# ============================================================================
# Why: User impact is immediate if disk fills up (writes fail).
# Detection: Query syslog for "No space left on device"
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "low_disk_space" {
  count               = var.monitoring_config.enable_clickhouse_alerts ? 1 : 0
  name                = "${var.cluster_name}-low-disk-space"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "CRITICAL: Disk space is critically low. Writes may be failing."
  severity            = 0 # Critical
  enabled             = true
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(15m)
      | where SyslogMessage contains "No space left on device"
      | summarize DiskErrors = count() by Computer
      | where DiskErrors > 0
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }
}
