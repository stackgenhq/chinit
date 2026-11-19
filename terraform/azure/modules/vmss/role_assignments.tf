# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Role Assignments for System-Assigned Managed Identity
# ============================================================================
# Why: Grants system-assigned Managed Identity on VMs permission to read and
#      write blob data in the storage account. This enables:
#      1. ClickHouse to write table data to blob storage (data persistence)
#      2. VMs to download updated cloud-init scripts from blob storage
#      3. ClickHouse backups to be written to blob storage
#      Uses "Storage Blob Data Contributor" role which provides full read/write
#      access, matching the permissions granted to user-assigned identity.
# Without it: VMs cannot write data to blob storage, causing ClickHouse write
#             failures. VMs also cannot download cloud-init scripts, preventing
#             script updates and requiring manual intervention or storage account
#             key access (which is less secure).
# ============================================================================

# Grant Storage Blob Data Contributor role to Writer VMSS' system-assigned identity
# Only applies when using system-assigned identity (managed_identity_id is null)
# This role provides read AND write access, required for ClickHouse to write data
resource "azurerm_role_assignment" "writer_storage_blob_contributor" {
  count                            = var.storage_account_id != null && var.managed_identity_id == null ? 1 : 0
  scope                            = var.storage_account_id
  role_definition_name             = "Storage Blob Data Contributor"
  principal_id                     = azurerm_user_assigned_identity.vmss[0].principal_id
  skip_service_principal_aad_check = true
}

# Grant Storage Blob Data Contributor role to Reader VMSS' system-assigned identity
# Only applies when using system-assigned identity (managed_identity_id is null)
# This role provides read AND write access, required for ClickHouse to write data
resource "azurerm_role_assignment" "reader_storage_blob_contributor" {
  count                            = var.storage_account_id != null && var.managed_identity_id == null ? 1 : 0
  scope                            = var.storage_account_id
  role_definition_name             = "Storage Blob Data Contributor"
  principal_id                     = azurerm_user_assigned_identity.vmss[0].principal_id
  skip_service_principal_aad_check = true
}

# Grant Storage Blob Data Contributor role to Keeper VMs' system-assigned identity
# Only applies when using system-assigned identity (managed_identity_id is null)
resource "azurerm_role_assignment" "keeper_storage_blob_contributor" {
  count                            = var.storage_account_id != null && var.managed_identity_id == null ? var.instance_config.keeper_count : 0
  scope                            = var.storage_account_id
  role_definition_name             = "Storage Blob Data Contributor"
  principal_id                     = azurerm_linux_virtual_machine.keepers[count.index].identity[0].principal_id
  skip_service_principal_aad_check = true
}

# ============================================================================
# Private DNS Zone Contributor Role Assignments
# ============================================================================
# Why: Grants system-assigned Managed Identity on VMs permission to create and
#      update A records in the Private DNS Zone. This enables VMs to register
#      their hostnames (keeper-0, writer-0, reader-0, etc.) during cloud-init
#      for proper cluster communication and service discovery.
# CRITICAL: Without this role, VMs cannot register their DNS records, causing:
#           - Cluster formation failures (nodes cannot resolve each other)
#           - Replication failures (replicas cannot find each other)
#           - Keeper quorum issues (keeper nodes cannot communicate)
# Reference: BLINDSPOTS.md item #2 - Missing DNS Permissions for VM Registration
# ============================================================================

# DNS Zone Contributor role removed for Writers/Readers as we use Auto-Registration
# resource "azurerm_role_assignment" "writer_dns_zone_contributor" { ... }
# resource "azurerm_role_assignment" "reader_dns_zone_contributor" { ... }

# DNS Zone Contributor role removed for Keepers as we use Auto-Registration
# and Fixed VMs don't need manual record management scripts.
# resource "azurerm_role_assignment" "keeper_dns_zone_contributor" { ... }
