# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Security Module - Network Watcher, Flow Logs, Defender, and Diagnostics
# ============================================================================
# This module implements security controls required by Azure Security Baseline:
# - Network Watcher and VNet Flow Logs for traffic monitoring
# - Microsoft Defender for Cloud integration
# - Diagnostic Settings for audit logging
# ============================================================================

# ============================================================================
# Network Watcher
# ============================================================================
# Why: Network Watcher is required for VNet flow logs, packet capture, and
#      network diagnostics. Azure creates one per region automatically, but
#      explicitly managing it ensures it exists and is properly configured.
# Without it: Cannot enable VNet flow logs or network diagnostics.
# ============================================================================
resource "azurerm_network_watcher" "main" {
  count               = var.network_monitoring_config.network_watcher.enabled ? 1 : 0
  name                = var.names.network_watcher
  location            = var.location
  resource_group_name = var.network_monitoring_config.network_watcher.resource_group_name != null ? var.network_monitoring_config.network_watcher.resource_group_name : var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# VNet Flow Logs (Replacing deprecated NSG Flow Logs)
# ============================================================================
# Why: Per Azure security best practices, enable flow logs for network traffic
#      analysis and security auditing. NSG flow logs are deprecated (retiring
#      Sept 2027); VNet flow logs are the recommended replacement.
# Reference: BLINDSPOTS.md #12 - No Network Watcher / NSG Flow Logs
# Without it: No visibility into network traffic patterns. Cannot detect
#             unauthorized access attempts or data exfiltration.
# ============================================================================
resource "azurerm_network_watcher_flow_log" "vnet" {
  count                = var.network_monitoring_config.flow_logs.enabled ? 1 : 0
  name                 = var.names.flow_log
  network_watcher_name = var.network_monitoring_config.network_watcher.enabled ? azurerm_network_watcher.main[0].name : var.network_monitoring_config.network_watcher.existing_name
  resource_group_name  = var.network_monitoring_config.network_watcher.resource_group_name != null ? var.network_monitoring_config.network_watcher.resource_group_name : var.resource_group_name

  # Target the NSG for flow logging
  network_security_group_id = var.network_monitoring_config.flow_logs.nsg_id
  storage_account_id        = var.network_monitoring_config.flow_logs.storage_account_id
  enabled                   = true
  version                   = 2

  retention_policy {
    enabled = true
    days    = var.network_monitoring_config.flow_logs.retention_days
  }

  # Traffic Analytics for enhanced network visibility
  dynamic "traffic_analytics" {
    for_each = var.network_monitoring_config.traffic_analytics.enabled ? [1] : []
    content {
      enabled               = true
      workspace_id          = var.network_monitoring_config.traffic_analytics.workspace_guid
      workspace_region      = var.location
      workspace_resource_id = var.network_monitoring_config.traffic_analytics.workspace_resource_id
      interval_in_minutes   = var.network_monitoring_config.traffic_analytics.interval_minutes
    }
  }

  tags = var.tags

  depends_on = [azurerm_network_watcher.main]
}

# ============================================================================
# Microsoft Defender for Storage
# ============================================================================
# Why: Per Azure Security Baseline, enable Microsoft Defender for Storage to
#      detect anomalous activities and potential threats to storage accounts.
# Reference: BLINDSPOTS.md #19 - No Microsoft Defender Integration
# Without it: Storage accounts lack threat detection and security alerts.
# ============================================================================
resource "azurerm_security_center_storage_defender" "main" {
  count              = var.defender_config.enable_storage_defender ? 1 : 0
  storage_account_id = var.defender_config.storage_account_id

  # Enable all sub-plans for comprehensive protection
  override_subscription_settings_enabled      = true
  malware_scanning_on_upload_enabled          = var.defender_config.malware_scanning_enabled
  malware_scanning_on_upload_cap_gb_per_month = var.defender_config.malware_scanning_cap_gb
  sensitive_data_discovery_enabled            = var.defender_config.sensitive_data_discovery_enabled
}

# ============================================================================
# Microsoft Defender for Key Vault
# ============================================================================
# Why: Per Azure Security Baseline, enable Defender for Key Vault to detect
#      suspicious access patterns and potential data exfiltration attempts.
# Without it: Key Vault lacks threat detection for unauthorized access.
# ============================================================================
# Note: Defender for Key Vault is enabled at subscription level
# This is handled via azurerm_security_center_subscription_pricing

# ============================================================================
# Diagnostic Settings: Key Vault Audit Logs
# ============================================================================
# Why: Per Azure compliance requirements, Key Vault audit logs must be exported
#      to Log Analytics and/or Storage for long-term retention and analysis.
# Reference: BLINDSPOTS.md #20 - No Diagnostic Settings Export
# Without it: Key Vault access events are not retained for compliance auditing.
# ============================================================================
resource "azurerm_monitor_diagnostic_setting" "keyvault" {
  count                      = var.diagnostics_config.key_vault.enabled && var.diagnostics_config.key_vault.resource_id != null ? 1 : 0
  name                       = "${var.names.diagnostic_setting}-keyvault"
  target_resource_id         = var.diagnostics_config.key_vault.resource_id
  log_analytics_workspace_id = var.diagnostics_config.log_analytics_workspace_id
  storage_account_id         = var.diagnostics_config.storage_account_id

  # Audit events - all Key Vault operations
  enabled_log {
    category = "AuditEvent"
  }

  # Azure Policy Evaluation events
  enabled_log {
    category = "AzurePolicyEvaluationDetails"
  }

  # Metrics
  metric {
    category = "AllMetrics"
    enabled  = true
  }
}

# ============================================================================
# Diagnostic Settings: Storage Account Logs
# ============================================================================
# Why: Storage account diagnostic logs capture all data plane operations
#      (read, write, delete) for security auditing and compliance.
# Without it: Storage access events are not logged for security analysis.
# ============================================================================
resource "azurerm_monitor_diagnostic_setting" "storage_blob" {
  count                      = var.diagnostics_config.storage.enabled && var.diagnostics_config.storage.resource_id != null ? 1 : 0
  name                       = "${var.names.diagnostic_setting}-storage-blob"
  target_resource_id         = "${var.diagnostics_config.storage.resource_id}/blobServices/default"
  log_analytics_workspace_id = var.diagnostics_config.log_analytics_workspace_id

  # Storage read operations
  enabled_log {
    category = "StorageRead"
  }

  # Storage write operations
  enabled_log {
    category = "StorageWrite"
  }

  # Storage delete operations
  enabled_log {
    category = "StorageDelete"
  }

  # Transaction metrics
  metric {
    category = "Transaction"
    enabled  = true
  }
}

# ============================================================================
# Diagnostic Settings: Automation Account
# ============================================================================
# Why: Automation account logs capture runbook executions and job status,
#      essential for auditing password sync operations.
# Without it: No audit trail for automated password updates.
# ============================================================================
resource "azurerm_monitor_diagnostic_setting" "automation" {
  count                      = var.diagnostics_config.automation.enabled && var.diagnostics_config.automation.resource_id != null ? 1 : 0
  name                       = "${var.names.diagnostic_setting}-automation"
  target_resource_id         = var.diagnostics_config.automation.resource_id
  log_analytics_workspace_id = var.diagnostics_config.log_analytics_workspace_id

  # Job logs
  enabled_log {
    category = "JobLogs"
  }

  # Job streams
  enabled_log {
    category = "JobStreams"
  }

  # DSC node status (if applicable)
  enabled_log {
    category = "DscNodeStatus"
  }

  # Automation account metrics
  metric {
    category = "AllMetrics"
    enabled  = true
  }
}
