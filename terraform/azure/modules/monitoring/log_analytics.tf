# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Log Analytics Workspace (Using Azure Verified Module)
# ============================================================================
# Why: Provides centralized log storage and analysis for ClickHouse cluster
#      logs, system logs, and application logs. Enables log queries, alerts,
#      and integration with Azure Monitor for comprehensive observability.
#      Uses Azure Verified Module (AVM) for best practices and maintainability.
# Without it: Logs remain only on individual VMs, making troubleshooting
#             difficult, compliance auditing impossible, and log analysis
#             across the cluster impossible.
# ============================================================================
module "log_analytics_workspace" {
  source  = "Azure/avm-res-operationalinsights-workspace/azurerm"
  version = "~> 0.5"

  name                = var.names.log_analytics_workspace
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
  enable_telemetry    = false # Disable telemetry to improve idempotency

  # Workspace configuration - AVM module uses prefixed variable names
  log_analytics_workspace_sku               = var.monitoring_config.log_analytics_sku
  log_analytics_workspace_retention_in_days = var.monitoring_config.log_retention_days

  # Enable internet ingestion and query access (can be restricted if needed)
  log_analytics_workspace_internet_ingestion_enabled = "true"
  log_analytics_workspace_internet_query_enabled     = "true"
}

# ============================================================================
# Custom Log Analytics Table for ClickHouse Logs
# ============================================================================
# Why: Creates a custom table to store ClickHouse application logs collected
#      from /var/log/clickhouse-server/*.log files. The Data Collection Rule
#      (DCR) requires this table to exist before it can reference the
#      Custom-ClickHouseLogs stream.
# Without it: The DCR will fail to create/update with an error indicating the
#             destination table doesn't exist. Logs cannot be collected.
# Note: Using azapi_resource because azurerm doesn't support creating custom
#       table schemas - it only manages retention on existing tables.
# ============================================================================
resource "azapi_resource" "clickhouse_logs_table" {
  type      = "Microsoft.OperationalInsights/workspaces/tables@2022-10-01"
  name      = "CHLogs_CL"
  parent_id = module.log_analytics_workspace.resource.id

  body = {
    properties = {
      schema = {
        name = "CHLogs_CL"
        columns = [
          {
            name        = "TimeGenerated"
            type        = "datetime"
            description = "Timestamp when the log entry was generated"
          },
          {
            name        = "RawData"
            type        = "string"
            description = "Raw log content from ClickHouse log files"
          },
          {
            name        = "Computer"
            type        = "string"
            description = "Hostname of the VM that generated the log"
          },
          {
            name        = "FilePath"
            type        = "string"
            description = "Path to the source log file"
          }
        ]
        description = "Custom table for ClickHouse server logs"
      }
      # Note: Retention is immutable on Basic Logs plan (8-day fixed retention)
      # Only set retention when using Analytics tier
      retentionInDays      = var.monitoring_config.enable_basic_logs ? null : var.monitoring_config.log_retention_days
      totalRetentionInDays = var.monitoring_config.enable_basic_logs ? null : var.monitoring_config.log_retention_days
      # FINOPS OPTIMIZATION: Basic Logs tier
      # 50-75% cost savings on log ingestion. Trade-off: 8-day interactive
      # retention, no alert queries, limited KQL. Good for high-volume logs
      # that don't need real-time alerting.
      plan = var.monitoring_config.enable_basic_logs ? "Basic" : "Analytics"
    }
  }

  schema_validation_enabled = false
  response_export_values    = ["*"]


}

# ============================================================================
# Custom Log Analytics Table for Bootstrap/Cloud-Init Logs
# ============================================================================
# Why: Creates a custom table to store bootstrap and cloud-init script logs
#      collected from /var/log/bootstrap.log. These logs contain the output
#      of all echo statements during VM provisioning (e.g., "Installing OpsVerse
#      agent..."). Without this, these logs are only available on the VM itself.
# Without it: Bootstrap script logs remain only on individual VMs, making it
#             impossible to troubleshoot provisioning issues from Azure portal.
# ============================================================================
resource "azapi_resource" "bootstrap_logs_table" {
  type      = "Microsoft.OperationalInsights/workspaces/tables@2022-10-01"
  name      = "BootLogs_CL"
  parent_id = module.log_analytics_workspace.resource.id

  body = {
    properties = {
      schema = {
        name = "BootLogs_CL"
        columns = [
          {
            name        = "TimeGenerated"
            type        = "datetime"
            description = "Timestamp when the log entry was generated"
          },
          {
            name        = "RawData"
            type        = "string"
            description = "Raw log content from bootstrap/cloud-init scripts"
          },
          {
            name        = "Computer"
            type        = "string"
            description = "Hostname of the VM that generated the log"
          },
          {
            name        = "FilePath"
            type        = "string"
            description = "Path to the source log file"
          }
        ]
        description = "Custom table for bootstrap and cloud-init script logs"
      }
      # Note: Retention is immutable on Basic Logs plan (8-day fixed retention)
      # Only set retention when using Analytics tier
      retentionInDays      = var.monitoring_config.enable_basic_logs ? null : var.monitoring_config.log_retention_days
      totalRetentionInDays = var.monitoring_config.enable_basic_logs ? null : var.monitoring_config.log_retention_days
      # FINOPS OPTIMIZATION: Basic Logs tier (same as ClickHouse logs)
      plan = var.monitoring_config.enable_basic_logs ? "Basic" : "Analytics"
    }
  }

  schema_validation_enabled = false
  response_export_values    = ["*"]


}

# ============================================================================
# Data Collection Rule for Azure Monitor Agent
# ============================================================================
# Why: Configures Azure Monitor Agent to collect performance counters,
#      syslog, and custom logs from ClickHouse VMs. Enables centralized
#      metric collection and log aggregation.
# Without it: Azure Monitor Agent cannot collect metrics or logs from VMs.
#             No centralized monitoring data available.
# ============================================================================
resource "azurerm_monitor_data_collection_rule" "clickhouse" {
  name                = var.names.data_collection_rule
  resource_group_name = var.resource_group_name
  location            = var.location
  kind                = "Linux"
  tags                = var.tags

  # Ensure custom tables exist before DCR references them
  depends_on = [
    azapi_resource.clickhouse_logs_table,
    azapi_resource.bootstrap_logs_table,
  ]

  destinations {
    log_analytics {
      workspace_resource_id = module.log_analytics_workspace.resource.id
      name                  = "clickhouse-logs"
    }
  }

  # Stream declaration for custom text logs
  # Why: Defines the schema for the custom log stream. This is required for
  #      custom text logs - the DCR needs to know what columns the log data
  #      will be mapped to in the destination table.
  stream_declaration {
    stream_name = "Custom-ClickHouseLogs"
    column {
      name = "TimeGenerated"
      type = "datetime"
    }
    column {
      name = "RawData"
      type = "string"
    }
    column {
      name = "Computer"
      type = "string"
    }
    column {
      name = "FilePath"
      type = "string"
    }
  }

  # Stream declaration for bootstrap/cloud-init logs
  # Why: Captures all output from bootstrap.sh and cloud-init.sh scripts,
  #      including provisioning steps like "Installing OpsVerse agent..."
  stream_declaration {
    stream_name = "Custom-BootstrapLogs"
    column {
      name = "TimeGenerated"
      type = "datetime"
    }
    column {
      name = "RawData"
      type = "string"
    }
    column {
      name = "Computer"
      type = "string"
    }
    column {
      name = "FilePath"
      type = "string"
    }
  }

  data_sources {
    # Syslog collection for VM logs
    # Why: Collects system logs from all VMs including ClickHouse logs,
    #      authentication logs, cron jobs, and system daemon logs. This
    #      enables centralized log analysis and troubleshooting.
    # Without it: VM logs remain only on individual VMs, making
    #             centralized log analysis and troubleshooting impossible.
    syslog {
      name    = "syslog"
      streams = ["Microsoft-Syslog"]
      facility_names = [
        "auth",     # Authentication logs
        "authpriv", # Private authentication logs
        "cron",     # Scheduled job logs
        "daemon",   # System daemon logs (including ClickHouse)
        "syslog",   # System logging daemon
        "user"      # User-level messages
      ]
      log_levels = [
        "Error",   # Error-level logs
        "Warning", # Warning-level logs
        "Info"     # Informational logs
      ]
    }

    # ClickHouse log file collection
    # Why: Collects ClickHouse application logs directly from log files,
    #      enabling centralized troubleshooting and audit trail for database
    #      operations. ClickHouse writes logs to files, not syslog, so
    #      file-based collection is required to capture all ClickHouse logs.
    # Without it: ClickHouse application logs remain only on individual VMs,
    #             making centralized troubleshooting impossible. Logs are lost
    #             if VMs are deleted, and there's no compliance audit trail.
    log_file {
      name    = "clickhouse-logs-file"
      streams = ["Custom-ClickHouseLogs"]
      file_patterns = [
        "/var/log/clickhouse-server/clickhouse-server.log",
        "/var/log/clickhouse-server/clickhouse-server.err.log"
      ]
      format = "text"
    }

    # Bootstrap/cloud-init log file collection
    # Why: Collects bootstrap and cloud-init script output from log files,
    #      enabling centralized troubleshooting of VM provisioning issues.
    #      These logs contain all echo statements from bootstrap.sh and
    #      cloud-init.sh, such as "Installing OpsVerse agent..." messages.
    # Without it: Provisioning logs remain only on individual VMs, making
    #             it impossible to troubleshoot provisioning failures from
    #             Azure portal without SSH access to the VM.
    log_file {
      name    = "bootstrap-logs-file"
      streams = ["Custom-BootstrapLogs"]
      file_patterns = [
        "/var/log/bootstrap.log",
        "/var/log/cloud-init-output.log"
      ]
      format = "text"
    }
  }

  data_flow {
    streams      = ["Microsoft-Syslog"]
    destinations = ["clickhouse-logs"]
  }

  data_flow {
    streams       = ["Custom-ClickHouseLogs"]
    destinations  = ["clickhouse-logs"]
    output_stream = "Custom-CHLogs_CL"
  }

  data_flow {
    streams       = ["Custom-BootstrapLogs"]
    destinations  = ["clickhouse-logs"]
    output_stream = "Custom-BootLogs_CL"
  }
}

# ============================================================================
# Disk Space Monitoring via Scheduled Query (BLINDSPOTS.md #20)
# ============================================================================
# Why: Azure Monitor doesn't provide a direct disk usage percentage metric for
#      VMs/VMSS. This scheduled query rule queries the Perf table from VM Insights
#      to detect when disk space usage exceeds threshold. Prevents disk exhaustion
#      which can cause ClickHouse write failures and service crashes.
# Requirement: Azure Monitor Agent must be installed on VMs with VM Insights enabled
#              to populate the Perf table with disk space metrics.
# Without it: Disk space exhaustion may go undetected until ClickHouse failures occur.
#             No early warning before disks fill up.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "disk_space_critical" {
  count               = var.enable_disk_alerts ? 1 : 0
  name                = "${var.names.action_group}-disk-space-critical"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "Alert when disk space usage exceeds ${var.alert_thresholds.disk_percent}% on ClickHouse VMs"
  severity            = 1 # Critical
  enabled             = var.enable_disk_alerts
  tags                = var.tags

  # Evaluate every 5 minutes, looking at last 5 minutes of data
  evaluation_frequency = "PT5M"
  window_duration      = "PT5M"
  scopes               = [module.log_analytics_workspace.resource.id]

  # Query Perf table for disk usage percentage
  # Uses InsightsMetrics from Azure Monitor Agent (VM Insights)
  criteria {
    query = <<-QUERY
      InsightsMetrics
      | where Namespace == "LogicalDisk" and Name == "FreeSpacePercentage"
      | where Tags has "clickhouse" or Computer has "writer" or Computer has "reader" or Computer has "keeper" or Computer has "backup"
      | summarize UsedSpacePercentage = 100 - avg(Val) by Computer, tostring(Tags)
      | where UsedSpacePercentage > ${var.alert_thresholds.disk_percent}
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

  # Skip if no results (requires criteria to be met)
  skip_query_validation = false
}

# Disk Space Warning Alert (lower threshold for early warning)
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "disk_space_warning" {
  count               = var.enable_disk_alerts ? 1 : 0
  name                = "${var.names.action_group}-disk-space-warning"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "Warning when disk space usage exceeds 70% on ClickHouse VMs"
  severity            = 2 # Warning
  enabled             = var.enable_disk_alerts
  tags                = var.tags

  evaluation_frequency = "PT15M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      InsightsMetrics
      | where Namespace == "LogicalDisk" and Name == "FreeSpacePercentage"
      | where Tags has "clickhouse" or Computer has "writer" or Computer has "reader" or Computer has "keeper" or Computer has "backup"
      | summarize UsedSpacePercentage = 100 - avg(Val) by Computer, tostring(Tags)
      | where UsedSpacePercentage > 70 and UsedSpacePercentage <= ${var.alert_thresholds.disk_percent}
    QUERY

    time_aggregation_method = "Count"
    threshold               = 0
    operator                = "GreaterThan"

    # Azure requires number_of_evaluation_periods = 1 when query doesn't project TimeGenerated
    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }

  skip_query_validation = false
}

# ============================================================================
# Log Analytics Ingestion Alert (MONITORING RELIABILITY)
# ============================================================================
# Why: Detects when syslog data stops flowing to Log Analytics, indicating
#      a potential Azure Monitor Agent or DCR association issue. This alert
#      catches the scenario where VMs are recreated (e.g., golden image
#      migration) but DCR associations are not properly recreated.
# Severity: Critical (1) - No logs means we lose visibility into the cluster
# Without it: Log collection outages can go undetected for hours/days,
#             resulting in gaps in observability and compliance data.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "no_logs_ingested" {
  name                = "${var.names.action_group}-no-logs-ingested"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "CRITICAL: No syslog data received in the last 15 minutes. Azure Monitor Agent may not be working or DCR associations may be missing."
  severity            = 1 # Critical
  enabled             = true
  tags                = var.tags

  # Evaluate every 5 minutes, looking at last 15 minutes of data
  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  # Query for presence of syslog data from ClickHouse VMs
  criteria {
    query = <<-QUERY
      Syslog
      | where TimeGenerated > ago(15m)
      | where Computer has "clickhouse" or Computer has "writer" or Computer has "reader" or Computer has "keeper" or Computer has "backup"
      | summarize LogCount = count()
    QUERY

    metric_measure_column   = "LogCount"
    time_aggregation_method = "Total"
    threshold               = 1
    operator                = "LessThan"

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }

  skip_query_validation = false
}

# ============================================================================
# ClickHouse Keeper Alerts (BLINDSPOTS.md #21)
# ============================================================================
# Why: ClickHouse Keeper is the critical coordination component.
#      - Frequent leader elections indicate network/consensus instability.
#      - Raft errors indicate data consistency risks.
#      - Socket errors indicate connectivity issues.
# ============================================================================

# Alert: Excessive Leader Elections (Instability)
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "keeper_leader_elections" {
  count               = var.monitoring_config.enable_clickhouse_alerts && !var.monitoring_config.enable_basic_logs ? 1 : 0
  name                = "${var.names.action_group}-keeper-leader-flux"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: ClickHouse Keeper is changing leaders frequently (>3 times in 15m). Indicates cluster instability."
  severity            = 2 # Warning
  enabled             = var.monitoring_config.enable_clickhouse_alerts
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      CHLogs_CL
      | where TimeGenerated > ago(15m)
      | where RawData has "Became leader"
      | summarize EventCount = count() by Computer
      | where EventCount > 3
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

# Alert: Keeper Raft Errors (Data Integrity Risk)
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "keeper_raft_errors" {
  count               = var.monitoring_config.enable_clickhouse_alerts && !var.monitoring_config.enable_basic_logs ? 1 : 0
  name                = "${var.names.action_group}-keeper-raft-errors"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "CRITICAL: ClickHouse Keeper Raft consensus errors detected. Potential data inconsistency or quorum loss."
  severity            = 1 # Critical
  enabled             = var.monitoring_config.enable_clickhouse_alerts
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT5M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      CHLogs_CL
      | where TimeGenerated > ago(5m)
      | where RawData has "RaftInstance" and (RawData has "Error" or RawData has "Fatal" or RawData has "terminate")
      | summarize EventCount = count() by Computer
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

# Alert: Keeper Socket/Connectivity Errors
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "keeper_socket_errors" {
  count               = var.monitoring_config.enable_clickhouse_alerts && !var.monitoring_config.enable_basic_logs ? 1 : 0
  name                = "${var.names.action_group}-keeper-connectivity"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "WARNING: specific ClickHouse Keeper nodes are experiencing persistent connectivity issues."
  severity            = 2 # Warning
  enabled             = var.monitoring_config.enable_clickhouse_alerts
  tags                = var.tags

  evaluation_frequency = "PT5M"
  window_duration      = "PT15M"
  scopes               = [module.log_analytics_workspace.resource.id]

  criteria {
    query = <<-QUERY
      CHLogs_CL
      | where TimeGenerated > ago(15m)
      | where RawData has "coordination" and (RawData has "Socket" or RawData has "Session expired" or RawData has "Connection reset")
      | summarize EventCount = count() by Computer
      | where EventCount > 10
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
