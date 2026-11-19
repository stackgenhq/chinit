# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Key Vault Module Outputs
# ============================================================================

output "key_vault_id" {
  description = "ID of the Azure Key Vault"
  value       = azurerm_key_vault.main.id
}

output "key_vault_name" {
  description = "Name of the Azure Key Vault"
  value       = azurerm_key_vault.main.name
}

output "key_vault_uri" {
  description = "URI of the Azure Key Vault"
  value       = azurerm_key_vault.main.vault_uri
}

output "default_password_secret_name" {
  description = "Name of the default password secret in Key Vault"
  value       = azurerm_key_vault_secret.clickhouse_default_password.name
}

output "admin_password_secret_name" {
  description = "Name of the admin password secret in Key Vault"
  value       = azurerm_key_vault_secret.clickhouse_admin_password.name
}

output "writer_password_secret_name" {
  description = "Name of the writer password secret in Key Vault"
  value       = azurerm_key_vault_secret.clickhouse_writer_password.name
}

output "default_password_secret_id" {
  description = "ID of the default password secret in Key Vault"
  value       = azurerm_key_vault_secret.clickhouse_default_password.id
  sensitive   = true
}

output "admin_password_secret_id" {
  description = "ID of the admin password secret in Key Vault"
  value       = azurerm_key_vault_secret.clickhouse_admin_password.id
  sensitive   = true
}

output "writer_password_secret_id" {
  description = "ID of the writer password secret in Key Vault"
  value       = azurerm_key_vault_secret.clickhouse_writer_password.id
  sensitive   = true
}

output "automation_account_name" {
  description = "Name of the Automation Account for password synchronization (if enabled)"
  value       = var.auto_rotate_config.enabled ? azurerm_automation_account.password_sync[0].name : null
}

output "automation_account_id" {
  description = "ID of the Automation Account for password synchronization (if enabled)"
  value       = var.auto_rotate_config.enabled ? azurerm_automation_account.password_sync[0].id : null
}

output "password_sync_enabled" {
  description = "Whether automatic password synchronization is enabled"
  value       = var.auto_rotate_config.enabled
}

# ============================================================================
# TLS Configuration
# ============================================================================
# NOTE: TLS certificates are managed by the ACME module (Let's Encrypt).
#       The ACME module stores certificates in Key Vault with names:
#       - clickhouse-tls-certificate
#       - clickhouse-tls-private-key
#       - clickhouse-tls-ca-certificate
# ============================================================================

output "tls_enabled" {
  description = "Whether TLS is enabled for ClickHouse (always true - TLS is mandatory via Let's Encrypt)"
  value       = true
}
