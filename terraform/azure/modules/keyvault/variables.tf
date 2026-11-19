# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Key Vault Module Variables
# ============================================================================

variable "resource_group_name" {
  description = "Name of the Azure Resource Group"
  type        = string
}

variable "location" {
  description = "Azure region for resources"
  type        = string
}

# ============================================================================
# Azure Client Config Variables (passed from root module to avoid data source lookups)
# ============================================================================
# Why: Using data sources for client config causes values to be "(known after apply)"
#      on every plan, which forces replacement of role assignments and other resources.
# Without it: terraform plan shows changes on every run, breaking idempotency.
# ============================================================================
# ============================================================================
# Client Configuration
# ============================================================================
variable "client_config" {
  description = "Azure Client Configuration (tenant, object, subscription)"
  type = object({
    tenant_id       = string
    object_id       = string
    subscription_id = string
  })
}

# ============================================================================
# Access Configuration
# ============================================================================
variable "access_config" {
  description = "Configuration for network and identity access"
  type = object({
    managed_identity_principal_id  = optional(string)
    enable_managed_identity_access = optional(bool, false)
    vnet_subnet_ids                = optional(list(string), [])
    allowed_ip_ranges              = optional(list(string), [])
  })
  default = {}
}


variable "cluster_name" {
  description = "Name of the ClickHouse cluster"
  type        = string
}

# ============================================================================
# Passwords & Security Configuration
# ============================================================================
variable "passwords_config" {
  description = "Configuration for passwords and security settings"
  type = object({
    default_password           = optional(string)
    admin_password             = optional(string)
    writer_password            = optional(string)
    soft_delete_retention_days = optional(number, 90)
    purge_protection_enabled   = optional(bool, true)
  })
  default = {}
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}

# ============================================================================
# Automation & Rotation Configuration
# ============================================================================
variable "auto_rotate_config" {
  description = "Configuration for automatic password rotation"
  type = object({
    enabled                = optional(bool, false)
    vm_resource_group_name = optional(string)
    webhook_expiry_hours   = optional(number, 2160)
  })
  default = {}
}

variable "log_analytics_workspace_id" {
  description = "Log Analytics Workspace ID for Key Vault diagnostic settings (optional)"
  type        = string
  default     = null
}

# ============================================================================
# Naming Variables (from Azure Naming Module)
# ============================================================================
# Why: Provides centralized naming convention from the Azure Naming module.
#      All resource names are generated using Azure CAF-compliant patterns
#      and passed from the root module to ensure consistency.
# Without it: Resource names must be manually constructed, leading to
#             inconsistent naming and potential conflicts for globally
#             unique resources like key vaults.
# ============================================================================
variable "names" {
  description = "Naming outputs from the Azure Naming module"
  type = object({
    key_vault               = string
    automation_account      = string
    event_grid_topic        = string
    event_grid_subscription = string
  })
}


