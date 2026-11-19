# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Security Module Variables
# ============================================================================

variable "resource_group_name" {
  description = "Name of the Azure Resource Group"
  type        = string
}

variable "location" {
  description = "Azure region for resources"
  type        = string
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}

# ============================================================================
# Naming Variables
# ============================================================================
variable "names" {
  description = "Naming outputs from the Azure Naming module"
  type = object({
    network_watcher    = string
    flow_log           = string
    diagnostic_setting = string
  })
}

# ============================================================================
# Network Monitoring Configuration (Network Watcher, Flow Logs, Traffic Analytics)
# ============================================================================
variable "network_monitoring_config" {
  description = "Configuration for network monitoring (Network Watcher, Flow Logs, Traffic Analytics)"
  type = object({
    network_watcher = optional(object({
      enabled             = optional(bool, true)
      resource_group_name = optional(string)
      existing_name       = optional(string)
    }), {})
    flow_logs = optional(object({
      enabled            = optional(bool, true)
      retention_days     = optional(number, 90)
      nsg_id             = optional(string)
      storage_account_id = optional(string)
    }), {})
    traffic_analytics = optional(object({
      enabled               = optional(bool, true)
      interval_minutes      = optional(number, 10)
      workspace_guid        = optional(string)
      workspace_resource_id = optional(string)
    }), {})
  })
  default = {}
}

# ============================================================================
# Microsoft Defender Configuration
# ============================================================================
variable "defender_config" {
  description = "Configuration for Microsoft Defender for Cloud"
  type = object({
    storage_account_id               = optional(string)
    enable_storage_defender          = optional(bool, true)
    malware_scanning_enabled         = optional(bool, true)
    malware_scanning_cap_gb          = optional(number, 5000)
    sensitive_data_discovery_enabled = optional(bool, false)
  })
  default = {}
}

# ============================================================================
# Diagnostic Settings Configuration
# ============================================================================
variable "diagnostics_config" {
  description = "Configuration for Azure Monitor Diagnostic Settings"
  type = object({
    log_analytics_workspace_id = optional(string)
    storage_account_id         = optional(string)
    key_vault = optional(object({
      enabled     = optional(bool, true)
      resource_id = optional(string)
    }), {})
    storage = optional(object({
      enabled     = optional(bool, true)
      resource_id = optional(string)
    }), {})
    automation = optional(object({
      enabled     = optional(bool, true)
      resource_id = optional(string)
    }), {})
  })
  default = {}
}
