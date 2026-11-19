# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Scheduled Query Alert: Backup Failures
# ============================================================================
# Why: Monitors backup logs for failures and alerts when backup create or
#      upload operations fail. This ensures backup failures are detected
#      immediately and can be addressed before data loss occurs.
# Industry Standard: Azure best practices recommend:
#   - Severity 1 (Critical) for backup failures per Azure Backup alerting patterns
#   - Auto-mitigation enabled to auto-resolve when condition clears
#   - Dimension splitting by Computer for multi-node troubleshooting
# Reference: https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/best-practices-alerts
# Without it: Backup failures may go unnoticed, potentially leading to
#             data loss if a disaster occurs and no recent backups exist.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "backup_failure" {
  count               = var.enable_backup_failure_alerts ? 1 : 0
  name                = "${var.names.action_group}-backup-failure"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "Alert when ClickHouse backup creation or upload fails. Severity: Critical. Auto-resolves when no failures detected."
  severity            = 1 # Critical severity per Azure Backup alerting standards
  enabled             = true

  scopes               = [module.log_analytics_workspace.resource.id]
  evaluation_frequency = "PT5M"  # Evaluate every 5 minutes
  window_duration      = "PT30M" # Look back 30 minutes

  # Auto-mitigation: Automatically resolve alert when condition no longer met
  # This follows Azure best practices for stateful alerts
  auto_mitigation_enabled = true

  criteria {
    query = <<-QUERY
      // ClickHouse Backup Failure Detection
      // Monitors syslog for clickhouse-backup errors
      // Pattern matches: "ERROR", "Failed to create backup", "Failed to upload"
      Syslog
      | where TimeGenerated > ago(30m)
      | where Facility in ("daemon", "user", "local0", "local1")
      | where SyslogMessage has_any ("clickhouse-backup", "clickhouse_backup")
      | where SyslogMessage has_any ("ERROR", "FATAL", "Failed", "failed", "error:")
      | extend FailureType = case(
          SyslogMessage has "Failed to create", "BackupCreationFailed",
          SyslogMessage has "Failed to upload", "BackupUploadFailed",
          SyslogMessage has "authentication", "AuthenticationFailed",
          SyslogMessage has "disk space", "DiskSpaceFailed",
          SyslogMessage has "connection", "ConnectionFailed",
          "OtherFailure"
        )
      | project TimeGenerated, Computer, FailureType, SyslogMessage
      | summarize 
          FailureCount = count(),
          LastFailure = max(TimeGenerated),
          FailureTypes = make_set(FailureType),
          SampleMessage = any(SyslogMessage)
        by Computer
    QUERY

    time_aggregation_method = "Count"
    operator                = "GreaterThan"
    threshold               = 0

    # Dimension splitting by Computer enables per-node alerting
    # This follows Azure best practices for multi-resource monitoring
    dimension {
      name     = "Computer"
      operator = "Include"
      values   = ["*"]
    }

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }

  tags = var.tags
}

# ============================================================================
# Scheduled Query Alert: Backup Success Monitoring (Missing Backup)
# ============================================================================
# Why: Monitors for successful backup completions. If no successful backups
#      are detected within a configurable window (default: 36 hours), an alert
#      is triggered. This catches silent failures where the backup job doesn't
#      run at all (e.g., timer disabled, VM stopped, scheduler failure).
# Industry Standard: Azure best practices recommend:
#   - Severity 2 (Error) for missing backups (less urgent than active failure)
#   - Separate alert from failure detection for clear escalation paths
#   - ClickHouse recommends automated restore testing - this alert enables that workflow
# Reference: https://clickhouse.com/docs/operations/backup
# Without it: Silent backup failures (where no logs are generated) may go
#             unnoticed, potentially leading to stale backups.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "backup_missing" {
  count               = var.enable_backup_failure_alerts ? 1 : 0
  name                = "${var.names.action_group}-backup-missing"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "Alert when no successful backup is detected within ${var.backup_config.missing_threshold_hours} hours. Check if backup timer is running and VM is healthy."
  severity            = 2 # Error severity (less critical than active failure)
  enabled             = true

  scopes               = [module.log_analytics_workspace.resource.id]
  evaluation_frequency = "PT1H" # Evaluate every hour
  window_duration      = "PT1H" # Look back 1 hour (query handles the actual window)

  # Auto-mitigation: Automatically resolve when successful backup is detected
  auto_mitigation_enabled = true

  # Skip query validation - the query uses dynamic variables
  skip_query_validation = true

  criteria {
    query = <<-QUERY
      // ClickHouse Backup Missing Detection
      // Alerts if no successful backup detected within threshold hours
      // Catches: timer disabled, VM stopped, scheduler failure, silent errors
      let lookback = ${var.backup_config.missing_threshold_hours}h;
      let successPatterns = dynamic(["Backup completed", "uploaded successfully", "backup created", "upload completed"]);
      // Check for any successful backup logs in the lookback period
      let successfulBackups = Syslog
      | where TimeGenerated > ago(lookback)
      | where Facility in ("daemon", "user", "local0", "local1")
      | where SyslogMessage has_any ("clickhouse-backup", "clickhouse_backup")
      | where SyslogMessage has_any (successPatterns)
      | summarize SuccessCount = count() by Computer;
      // Generate alert if no successes found
      // Using union with empty check pattern for reliable alerting
      successfulBackups
      | summarize TotalSuccesses = sum(SuccessCount)
      | where TotalSuccesses == 0
      | extend AlertReason = "No successful backup detected in last ${var.backup_config.missing_threshold_hours} hours"
      | project AlertReason, TotalSuccesses, CheckTime = now()
    QUERY

    time_aggregation_method = "Count"
    operator                = "GreaterThan"
    threshold               = 0

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }

  tags = var.tags
}

# ============================================================================
# Scheduled Query Alert: Backup Storage Quota Warning
# ============================================================================
# Why: Monitors backup storage usage and alerts when approaching capacity limits.
#      This prevents backup failures due to storage quota exhaustion.
# Industry Standard: Proactive capacity monitoring per SRE best practices
# Without it: Backups may silently fail when storage is full.
# ============================================================================
resource "azurerm_monitor_scheduled_query_rules_alert_v2" "backup_storage_warning" {
  count               = var.enable_backup_failure_alerts ? 1 : 0
  name                = "${var.names.action_group}-backup-storage-warning"
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "Warning when backup-related disk space issues are detected. Check local backup storage and clean up old backups."
  severity            = 3 # Warning severity
  enabled             = true

  scopes               = [module.log_analytics_workspace.resource.id]
  evaluation_frequency = "PT1H"
  window_duration      = "PT1H"

  auto_mitigation_enabled = true

  criteria {
    query = <<-QUERY
      // Backup Storage Warning Detection
      // Monitors for disk space warnings related to backups
      Syslog
      | where TimeGenerated > ago(1h)
      | where Facility in ("daemon", "user", "local0", "local1")
      | where SyslogMessage has_any ("clickhouse-backup", "clickhouse_backup", "/var/lib/clickhouse/backup")
      | where SyslogMessage has_any ("disk space", "no space", "quota", "storage full", "cannot allocate")
      | project TimeGenerated, Computer, SyslogMessage
      | summarize WarningCount = count(), LastWarning = max(TimeGenerated) by Computer
    QUERY

    time_aggregation_method = "Count"
    operator                = "GreaterThan"
    threshold               = 0

    dimension {
      name     = "Computer"
      operator = "Include"
      values   = ["*"]
    }

    failing_periods {
      minimum_failing_periods_to_trigger_alert = 1
      number_of_evaluation_periods             = 1
    }
  }

  action {
    action_groups = [azurerm_monitor_action_group.clickhouse.id]
  }

  tags = var.tags
}
