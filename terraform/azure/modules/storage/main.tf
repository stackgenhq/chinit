# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Storage Account Name Generation
# ============================================================================
# Why: Azure storage account names must be globally unique, 3-24 characters,
#      lowercase alphanumeric only. The name is now provided from the Azure
#      Naming module which handles uniqueness automatically with a random suffix.
#      If a custom name is provided via var.storage_account_name, it is used
#      instead of the naming module output.
# Without it: Manual name management is required, potentially leading to
#             naming conflicts with globally unique storage account names.
# ============================================================================
locals {
  # Use custom name if provided, otherwise use the Azure Naming module output
  storage_account_name = var.account_config.name != null ? var.account_config.name : var.names.storage_account
}


# ============================================================================
# Azure Storage Account - Native azurerm Resource
# ============================================================================
# Why: The storage account provides the Azure Blob Storage backend for
#      ClickHouse data. Using native azurerm resource instead of AVM module
#      guarantees idempotent applies without state drift issues.
# Without it: ClickHouse cannot store data. The entire data persistence
#             layer is missing.
# Security: Access is controlled via Azure RBAC with Managed Identity and
#           network rules that deny public access by default. Access is only
#           allowed from specified subnets/IPs or via private endpoints.
# ============================================================================
resource "azurerm_storage_account" "main" {
  name                     = substr(local.storage_account_name, 0, 24)
  resource_group_name      = var.resource_group_name
  location                 = var.location
  account_tier             = var.account_config.tier
  account_replication_type = var.account_config.replication_type
  account_kind             = var.account_config.kind
  min_tls_version          = "TLS1_2"
  access_tier              = var.account_config.access_tier

  # SECURITY: Infrastructure encryption (double encryption) per Azure Security Baseline
  # Provides an additional layer of encryption using platform-managed keys at the
  # infrastructure level, protecting data even if service-level encryption is compromised.
  # Reference: https://learn.microsoft.com/en-us/azure/storage/common/infrastructure-encryption-enable
  infrastructure_encryption_enabled = true

  # SECURITY: Disable public blob access at account level per Azure Security Baseline
  # This is a defense-in-depth control that prevents any container from being made public,
  # even if container_access_type is accidentally set to "blob" or "container".
  # Reference: https://learn.microsoft.com/en-us/azure/storage/blobs/anonymous-read-access-prevent
  allow_nested_items_to_be_public = false

  # Control shared access key availability
  # SECURITY: Per Azure Storage security baseline, disable when using Entra ID/MI
  # Set to false when all data plane operations use Managed Identity
  # WARNING: Disabling breaks SAS tokens and storage account key access
  shared_access_key_enabled = var.network_access_config.allow_shared_key_access

  # Blob retention policies for data protection
  # SECURITY FIX: Increased soft delete retention from 7 to 30 days per Azure best practices
  # Azure recommends 14-90 days based on compliance requirements
  blob_properties {
    delete_retention_policy {
      days = var.data_protection_config.soft_delete.blob_retention_days
    }
    container_delete_retention_policy {
      days = var.data_protection_config.soft_delete.container_retention_days
    }
    # Blob versioning for config/data history (optional with GitOps)
    # Why: Enables tracking of blob changes, useful for:
    #   - Config file history when not using GitOps
    #   - Accidental overwrite recovery
    #   - Audit trail for compliance
    # Without it: Overwritten/deleted blobs are lost (unless soft-delete catches them)
    versioning_enabled = var.data_protection_config.versioning.enabled

    # Change feed for audit trail (SECURITY AUDIT FIX)
    # Why: Per Azure best practices, change feed enables tracking all blob changes
    #      for security auditing, compliance, and event-driven processing.
    # Without it: Cannot audit blob modifications or integrate with event-driven
    #             security monitoring systems.
    change_feed_enabled           = var.data_protection_config.change_feed.enabled
    change_feed_retention_in_days = var.data_protection_config.change_feed.enabled ? var.data_protection_config.change_feed.retention_days : null
  }

  # Network rules - deny public access by default for security
  # Access is controlled via RBAC with Managed Identity and optionally
  # via private endpoints or explicitly allowed subnets/IPs
  # Note: When network_rules_allowed_subnet_ids is empty, network rules are set
  #       to Deny by default. Network rules can be managed separately via
  #       azurerm_storage_account_network_rules resource to break dependency cycles.
  network_rules {
    default_action             = var.network_access_config.default_action
    bypass                     = ["AzureServices"]
    virtual_network_subnet_ids = length(var.network_access_config.allowed_subnet_ids) > 0 ? var.network_access_config.allowed_subnet_ids : []
    ip_rules                   = var.network_access_config.allowed_ip_ranges
  }

  tags = var.tags

  lifecycle {
    ignore_changes = [
      network_rules, # Ignore network_rules changes when managed separately to avoid conflicts
    ]
  }
}

# ============================================================================
# Blob Container for ClickHouse Data
# ============================================================================
# Why: The container is the logical unit within the storage account where
#      ClickHouse stores all its data (table parts, indexes, etc.).
# Without it: ClickHouse cannot write data to blob storage.
# ============================================================================
resource "azurerm_storage_container" "clickhouse_data" {
  name                  = var.storage_container_name != null ? var.storage_container_name : var.names.storage_container
  storage_account_id    = azurerm_storage_account.main.id
  container_access_type = "private"
}

# ============================================================================
# Storage Table for chinit Ordinal Leases
# ============================================================================
# Why: chinit's lease identity claims writer-N/reader-N ordinals from a
#      Storage Table (the Azure mirror of the AWS node-claims DynamoDB
#      table — same lease contract: PartitionKey "readers", RowKey the
#      hostname, lease_until the TTL). The Azure VMSS assigns DYNAMIC
#      private IPs, so the static IP-octet identity chinit uses on AWS
#      cannot resolve a node's name there; the table IS the identity
#      source for the ASG roles.
# Without it: chinit boot Fatals on lease-mode roles (no table to claim
#             from) and the cluster never forms.
# ============================================================================
resource "azurerm_storage_table" "node_claims" {
  name               = "${var.cluster_name}-node-claims"
  storage_account_id = azurerm_storage_account.main.id
}

# ============================================================================
# Storage Account Data Source (for Access Key)
# ============================================================================
# Why: Retrieves the storage account access key needed for ClickHouse to
#      authenticate to Azure Blob Storage when not using managed identity.
# Without it: ClickHouse cannot authenticate to blob storage without managed
#             identity configured.
# ============================================================================
data "azurerm_storage_account" "main" {
  name                = azurerm_storage_account.main.name
  resource_group_name = var.resource_group_name
}

# ============================================================================
# User-Assigned Managed Identity
# ============================================================================
# Why: Provides secure authentication to Azure Storage without storing keys
#      in cloud-init scripts or VM metadata. Managed Identity eliminates the
#      need for storage account keys, reducing security risk.
# Without it: Storage account keys must be stored in cloud-init (visible in
#             logs and metadata), creating a security vulnerability.
# ============================================================================
resource "azurerm_user_assigned_identity" "clickhouse" {
  name                = var.names.managed_identity
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# Grant Storage Blob Data Contributor Role
# ============================================================================
# Why: Grants the managed identity permission to read and write blob data
#      in the storage account. This replaces storage account key authentication.
# Without it: Managed identity cannot access blob storage, causing ClickHouse
#             write failures and data access errors.
# ============================================================================
resource "azurerm_role_assignment" "storage_blob_contributor" {
  scope                            = azurerm_storage_account.main.id
  role_definition_name             = "Storage Blob Data Contributor"
  principal_id                     = azurerm_user_assigned_identity.clickhouse.principal_id
  skip_service_principal_aad_check = true
}

# ============================================================================
# Grant Storage Table Data Contributor Role
# ============================================================================
# Why: chinit's lease claimer reads/writes the node-claims table over its
#      bearer-token REST surface (managed identity). The claim/renew/release
#      operations and the live-claims query all key on this RBAC role.
# Without it: chinit lease identity fails with AuthorizationFailed on the
#             first conditional insert and the boot loops.
# ============================================================================
resource "azurerm_role_assignment" "storage_table_contributor" {
  scope                            = azurerm_storage_account.main.id
  role_definition_name             = "Storage Table Data Contributor"
  principal_id                     = azurerm_user_assigned_identity.clickhouse.principal_id
  skip_service_principal_aad_check = true
}

# ============================================================================
# Lifecycle Management Policy for Blob Versioning
# ============================================================================
# Why: Microsoft explicitly recommends lifecycle management policies when blob
#      versioning is enabled to control costs. Every write operation creates a
#      new version, which can lead to significant storage cost increases without
#      automatic cleanup of old versions.
# Reference: https://learn.microsoft.com/en-us/azure/storage/blobs/versioning-overview
# Without it: Old blob versions accumulate indefinitely, causing storage costs
#             to increase significantly over time.
# ============================================================================
# ============================================================================
# Storage Account Diagnostic Settings (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per Azure Security Baseline and CIS Benchmark, Storage Account must have
#      diagnostic settings enabled to audit all blob access and operations.
#      This is required for compliance (PCI-DSS, HIPAA, SOC2) and security
#      incident investigation.
# Reference: Azure Policy - Storage accounts should have resource logs enabled
# Without it: Cannot audit blob access, detect data exfiltration, or investigate
#             security incidents. Non-compliant with security frameworks.
# ============================================================================
resource "azurerm_monitor_diagnostic_setting" "storage" {
  count                      = var.log_analytics_workspace_id != null ? 1 : 0
  name                       = "diag-${azurerm_storage_account.main.name}"
  target_resource_id         = "${azurerm_storage_account.main.id}/blobServices/default"
  log_analytics_workspace_id = var.log_analytics_workspace_id

  # Storage read/write/delete operations
  enabled_log {
    category = "StorageRead"
  }

  enabled_log {
    category = "StorageWrite"
  }

  enabled_log {
    category = "StorageDelete"
  }

  # Transaction metrics
  enabled_metric {
    category = "Transaction"
  }
}

resource "azurerm_storage_management_policy" "versioning_cleanup" {
  count              = var.data_protection_config.versioning.enabled ? 1 : 0
  storage_account_id = azurerm_storage_account.main.id

  rule {
    name    = "delete-old-versions"
    enabled = true

    filters {
      blob_types = ["blockBlob"]
    }

    actions {
      # Delete non-current (previous) versions after retention period
      # This prevents unlimited version accumulation
      version {
        delete_after_days_since_creation = var.data_protection_config.versioning.retention_days
        # Tier old versions to cool storage before deletion to reduce costs
        # Only applies when blob_version_tier_to_cool is enabled
        change_tier_to_cool_after_days_since_creation = var.data_protection_config.versioning.tier_to_cool ? var.data_protection_config.versioning.tier_to_cool_days : null
      }

      # Also clean up old snapshots if any exist
      snapshot {
        delete_after_days_since_creation_greater_than = var.data_protection_config.versioning.retention_days
      }
    }
  }
}
