# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Resource Group Name Variable
# ============================================================================
# Why: Identifies the Azure Resource Group where the storage account and
#      container will be created. Required for resource organization and
#      lifecycle management.
# Without it: Storage account cannot be created as it requires a resource
#             group. Terraform will fail with an error when trying to create
#             the storage account without a valid resource group name.
# ============================================================================
variable "resource_group_name" {
  description = "Name of the Azure Resource Group"
  type        = string
}

# ============================================================================
# Location Variable
# ============================================================================
# Why: Specifies the Azure region where the storage account and container
#      will be deployed. This affects latency, data residency, availability,
#      and pricing. Should match the region of other resources (AKS cluster).
# Without it: Storage account cannot be created as Azure requires a location
#             for all resources. Deployment will fail without a valid region.
# ============================================================================
variable "location" {
  description = "Azure region for resources"
  type        = string
}

# ============================================================================
# Cluster Name Variable
# ============================================================================
# Why: Names the chinit ordinal-lease table (<cluster>-node-claims) — the
#      Azure mirror of the AWS node-claims DynamoDB table. The cluster
#      prefix keeps per-cluster lease tables distinct when one subscription
#      hosts more than one stack.
# Without it: The lease table name would need hard-coding per stack.
# ============================================================================
variable "cluster_name" {
  description = "ClickHouse cluster name (prefixes the node-claims table)"
  type        = string
}

# ============================================================================
# Storage Account Configuration
# ============================================================================
variable "account_config" {
  description = "Configuration for the Storage Account (name, tier, replication, kind)"
  type = object({
    name             = optional(string)
    tier             = optional(string, "Standard")
    replication_type = optional(string, "ZRS")
    kind             = optional(string, "StorageV2")
    access_tier      = optional(string, "Hot")
  })
  default = {}
}

# ============================================================================
# Tags Variable
# ============================================================================
# Why: Provides metadata tags for resource organization, cost allocation,
#      automation, and compliance. Tags enable filtering, reporting, and
#      policy enforcement across Azure resources.
# Without it: Resources are created without tags, making cost allocation,
#             resource organization, and automation more difficult. If not
#             provided, defaults to empty map (no tags applied).
# ============================================================================
variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}

# ============================================================================
# Network Access Configuration
# ============================================================================
variable "network_access_config" {
  description = "Configuration for storage network access rules"
  type = object({
    default_action          = optional(string, "Deny")
    allowed_subnet_ids      = optional(list(string), [])
    allowed_ip_ranges       = optional(list(string), [])
    allow_shared_key_access = optional(bool, true)
  })
  default = {}
}

variable "storage_container_name" {
  description = "Name of the blob container for ClickHouse data"
  type        = string
  default     = "clickhouse-data"
}

# ============================================================================
# Naming Variables (from Azure Naming Module)
# ============================================================================
# Why: Provides centralized naming convention from the Azure Naming module.
#      All resource names are generated using Azure CAF-compliant patterns
#      and passed from the root module to ensure consistency.
# Without it: Resource names must be manually constructed, leading to
#             inconsistent naming and potential conflicts for globally
#             unique resources like storage accounts.
# ============================================================================
variable "names" {
  description = "Naming outputs from the Azure Naming module"
  type = object({
    storage_account   = string
    storage_container = string
    managed_identity  = string
  })
}

# ============================================================================
# Data Protection Configuration (Versioning, Soft Delete, Change Feed)
# ============================================================================
variable "data_protection_config" {
  description = "Configuration for data protection features (versioning, soft delete, change feed)"
  type = object({
    versioning = optional(object({
      enabled           = optional(bool, false)
      retention_days    = optional(number, 30)
      tier_to_cool      = optional(bool, false)
      tier_to_cool_days = optional(number, 7)
    }), {})
    soft_delete = optional(object({
      blob_retention_days      = optional(number, 30)
      container_retention_days = optional(number, 30)
    }), {})
    change_feed = optional(object({
      enabled        = optional(bool, true)
      retention_days = optional(number, 30)
    }), {})
  })
  default = {}
}

# ============================================================================
# Diagnostic Settings Configuration (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per Azure Security Baseline, storage accounts must have diagnostic
#      settings enabled to audit all blob access and operations for compliance
#      and security incident investigation.
# Without it: Cannot audit blob access, detect data exfiltration, or investigate
#             security incidents.
# ============================================================================
variable "log_analytics_workspace_id" {
  description = "Log Analytics Workspace ID for storage diagnostic settings (optional)"
  type        = string
  default     = null
}
