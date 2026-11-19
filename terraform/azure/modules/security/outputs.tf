# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Security Module Outputs
# ============================================================================

output "network_watcher_id" {
  description = "ID of the Network Watcher"
  value       = var.enable_network_watcher ? azurerm_network_watcher.main[0].id : null
}

output "network_watcher_name" {
  description = "Name of the Network Watcher"
  value       = var.enable_network_watcher ? azurerm_network_watcher.main[0].name : var.existing_network_watcher_name
}

output "flow_log_id" {
  description = "ID of the VNet Flow Log"
  value       = var.enable_flow_logs ? azurerm_network_watcher_flow_log.vnet[0].id : null
}

output "keyvault_diagnostic_setting_id" {
  description = "ID of the Key Vault diagnostic setting"
  value       = var.enable_keyvault_diagnostics && var.key_vault_id != null ? azurerm_monitor_diagnostic_setting.keyvault[0].id : null
}

output "storage_diagnostic_setting_id" {
  description = "ID of the Storage diagnostic setting"
  value       = var.enable_storage_diagnostics && var.storage_account_id != null ? azurerm_monitor_diagnostic_setting.storage_blob[0].id : null
}
