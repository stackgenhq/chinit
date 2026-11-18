# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Outputs — chinit cluster module (Azure)
# ============================================================================

output "cluster_name" {
  description = "The cluster name."
  value       = var.cluster_name
}

output "dns_zone_name" {
  description = "The private DNS zone nodes register into."
  value       = var.dns_zone_name
}

output "storage_account_name" {
  description = "The storage account (bundle container, data container, backup container, node-claims table)."
  value       = azurerm_storage_account.this.name
}

output "node_claims_table" {
  description = "The Table Storage ordinal-lease table name."
  value       = azurerm_storage_table.node_claims.name
}

output "key_vault_name" {
  description = "The Key Vault holding the three password secrets."
  value       = azurerm_key_vault.this.name
}

output "secrets" {
  description = "The Key Vault secret NAMES the specs carry (values resolve at boot via the managed identity)."
  value = {
    default_password = azurerm_key_vault_secret.default_password.name
    admin_password   = azurerm_key_vault_secret.admin_password.name
    writer_password  = azurerm_key_vault_secret.writer_password.name
  }
}

output "config_files_hash" {
  description = "The SHA256 gate over the bundle's hashed members (chinit recomputes it at boot)."
  value       = module.chinit.config_files_hash
}

output "writer_vmss_name" {
  description = "The writers VMSS resource name."
  value       = azurerm_orchestrated_virtual_machine_scale_set.writers.name
}

output "reader_vmss_name" {
  description = "The readers VMSS resource name."
  value       = azurerm_orchestrated_virtual_machine_scale_set.readers.name
}

output "keeper_hostnames" {
  description = "Keeper hostnames (keeper-0, ...) — the Raft ensemble inventory."
  value       = local.keeper_hostnames
}

output "keeper_ips" {
  description = "The static keeper private IPs."
  value       = local.keeper_ips
}

output "node_endpoints" {
  description = "Client-facing hostnames (resolve inside the VNet)."
  value = {
    writers = [for i in range(var.writer_max) : "writer-${i}.${var.dns_zone_name}"]
    readers = [for i in range(var.reader_max) : "reader-${i}.${var.dns_zone_name}"]
    keepers = [for h in local.keeper_hostnames : "${h}.${var.dns_zone_name}"]
    backup  = var.enable_backup ? ["backup-0.${var.dns_zone_name}"] : []
  }
}

output "admin_ssh_public_key" {
  description = "The SSH public key the module generated for the admin user (null when you supplied one)."
  value       = var.ssh_public_key == null ? tls_private_key.node_access[0].public_key_openssh : var.ssh_public_key
}
