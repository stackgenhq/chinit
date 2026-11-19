# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Monitoring Module Outputs
# ============================================================================

output "log_analytics_workspace_id" {
  description = "ID of the Log Analytics Workspace"
  value       = module.log_analytics_workspace.resource.id
}

output "log_analytics_workspace_name" {
  description = "Name of the Log Analytics Workspace"
  value       = module.log_analytics_workspace.resource.name
}

output "log_analytics_workspace_guid" {
  description = "GUID (workspace ID) of the Log Analytics Workspace. Required for Traffic Analytics."
  value       = module.log_analytics_workspace.resource.workspace_id
}

output "data_collection_rule_id" {
  description = "ID of the Data Collection Rule"
  value       = azurerm_monitor_data_collection_rule.clickhouse.id
}

output "action_group_id" {
  description = "ID of the Action Group for alerts"
  value       = azurerm_monitor_action_group.clickhouse.id
}

# ============================================================================
# VM Lifecycle Notification Outputs
# ============================================================================

output "vm_lifecycle_action_group_id" {
  description = "ID of the Action Group for VM lifecycle notifications"
  value       = var.enable_vm_lifecycle_alerts ? azurerm_monitor_action_group.vm_lifecycle[0].id : null
}

output "vm_lifecycle_alerts_enabled" {
  description = "Whether VM lifecycle alerts are enabled"
  value       = var.enable_vm_lifecycle_alerts
}

# service_health_alert_id removed - alert disabled to reduce noise for DB operations

# resource_health_alert_id removed - alert disabled to reduce SRE alert fatigue
# See activity_log_alerts.tf for details on why this was removed (2026-01-27)

# ============================================================================
# Maintenance Window Outputs
# ============================================================================

output "maintenance_window_rule_ids" {
  description = "Map of maintenance window names to their Alert Processing Rule IDs"
  value = var.enable_maintenance_windows ? {
    for name, rule in azurerm_monitor_alert_processing_rule_suppression.maintenance_windows :
    name => rule.id
  } : {}
}

output "maintenance_windows_enabled" {
  description = "Whether maintenance window alert processing rules are enabled"
  value       = var.enable_maintenance_windows
}

output "maintenance_window_schedules" {
  description = "List of configured maintenance window schedules"
  value       = var.enable_maintenance_windows ? var.maintenance_window_schedules : []
}

# ============================================================================
# Backup Failure Alert Outputs
# ============================================================================

output "backup_failure_alert_id" {
  description = "ID of the backup failure alert"
  value       = var.enable_backup_failure_alerts ? azurerm_monitor_scheduled_query_rules_alert_v2.backup_failure[0].id : null
}

output "backup_missing_alert_id" {
  description = "ID of the missing backup alert"
  value       = var.enable_backup_failure_alerts ? azurerm_monitor_scheduled_query_rules_alert_v2.backup_missing[0].id : null
}

output "backup_storage_warning_alert_id" {
  description = "ID of the backup storage warning alert"
  value       = var.enable_backup_failure_alerts ? azurerm_monitor_scheduled_query_rules_alert_v2.backup_storage_warning[0].id : null
}

output "backup_failure_alerts_enabled" {
  description = "Whether backup failure alerts are enabled"
  value       = var.enable_backup_failure_alerts
}
