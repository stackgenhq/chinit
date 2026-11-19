# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Storage Account Name Output
# ============================================================================
# Why: Provides the storage account name for reference by other modules
#      (e.g., ClickHouse module), scripts, or applications that need to
#      access ClickHouse data in Azure Blob Storage.
# Without it: Other modules or scripts cannot programmatically reference
#             the storage account name, requiring manual lookups or
#             hardcoded values for blob storage access.
# ============================================================================
output "storage_account_name" {
  description = "Name of the storage account"
  value       = azurerm_storage_account.main.name
}

# ============================================================================
# Storage Account ID Output
# ============================================================================
# Why: Provides the Azure resource ID of the storage account. This is needed
#      for Azure API calls, resource references, and integration with other
#      Azure services that need to reference the storage account.
# Without it: Cannot programmatically reference the storage account via Azure
#             resource ID, requiring manual lookups or hardcoded IDs for
#             Azure API calls and resource references.
# ============================================================================
output "storage_account_id" {
  description = "ID of the storage account"
  value       = azurerm_storage_account.main.id
}

# ============================================================================
# Storage Account Key Output
# ============================================================================
# Why: Provides the primary access key for authenticating to Azure Blob
#      Storage. This key is needed by the ClickHouse module to configure
#      blob storage access in ClickHouse storage configuration.
# Without it: ClickHouse cannot authenticate to blob storage. Authentication
#             failures will occur, preventing data writes and reads, causing
#             ClickHouse pods to fail or crash.
# ============================================================================
output "storage_account_key" {
  description = "Primary access key for the storage account"
  value       = data.azurerm_storage_account.main.primary_access_key
  sensitive   = true
}

# ============================================================================
# Storage Account Primary Endpoint Output
# ============================================================================
# Why: Provides the primary blob storage endpoint URL for the storage account.
#      This is needed for applications or scripts that need to access blob
#      storage directly via REST API or SDKs.
# Without it: Applications or scripts cannot programmatically determine the
#             storage endpoint, requiring manual lookups or hardcoded URLs
#             for blob storage access.
# ============================================================================
output "storage_account_primary_endpoint" {
  description = "Primary endpoint of the storage account"
  value       = azurerm_storage_account.main.primary_blob_endpoint
}

# ============================================================================
# Storage Container Name Output
# ============================================================================
# Why: Provides the blob container name where ClickHouse data is stored.
#      This output enables other modules (e.g., ClickHouse module), scripts,
#      or applications to reference the container for data access or
#      management operations.
# Without it: Other modules or scripts cannot programmatically reference
#             the container name, requiring manual lookups or hardcoded
#             values for blob storage access.
# ============================================================================
output "storage_container_name" {
  description = "Name of the blob container"
  value       = azurerm_storage_container.clickhouse_data.name
}

# ============================================================================
# Storage Container ID Output
# ============================================================================
# Why: Provides the Azure resource ID of the blob container. This is needed
#      for Azure API calls, resource references, and integration with other
#      Azure services that need to reference the container.
# Without it: Cannot programmatically reference the container via Azure
#             resource ID, requiring manual lookups or hardcoded IDs for
#             Azure API calls and resource references.
# ============================================================================
output "storage_container_id" {
  description = "ID of the blob container"
  value       = azurerm_storage_container.clickhouse_data.id
}

# ============================================================================
# Managed Identity Outputs
# ============================================================================
# Why: Provides the managed identity ID and client ID for use by VMSS instances
#      to authenticate to Azure Storage without storage account keys.
# Without it: VMSS instances cannot use managed identity for storage access,
#             requiring storage account keys to be stored in cloud-init.
# ============================================================================
output "managed_identity_id" {
  description = "ID of the user-assigned managed identity"
  value       = azurerm_user_assigned_identity.clickhouse.id
}

output "managed_identity_client_id" {
  description = "Client ID of the user-assigned managed identity"
  value       = azurerm_user_assigned_identity.clickhouse.client_id
}

output "managed_identity_principal_id" {
  description = "Principal ID of the user-assigned managed identity"
  value       = azurerm_user_assigned_identity.clickhouse.principal_id
}

# ============================================================================
# Node Claims Table Name Output
# ============================================================================
# Why: chinit's lease identity (writers/readers) claims ordinals from this
#      Storage Table at boot; the clickhouse module renders the name into
#      every role's chinit spec (claimer_table_name) and the VMSS wrapper
#      env-pins it for the subcommands.
# Without it: The chinit spec would hard-code the table name and drift from
#             the created resource on rename.
# ============================================================================
output "node_claims_table_name" {
  description = "Name of the chinit ordinal-lease Storage Table"
  value       = azurerm_storage_table.node_claims.name
}

# ============================================================================
# Blob Versioning Output
# ============================================================================
# Why: Indicates whether blob versioning is enabled for the storage account.
#      This is useful for operational awareness and compliance verification.
# Without it: Cannot programmatically check if blob versioning is enabled,
#             requiring manual verification via Azure Portal or CLI.
# ============================================================================
output "blob_versioning_enabled" {
  description = "Whether blob versioning is enabled for config/data history"
  value       = var.data_protection_config.versioning.enabled
}
