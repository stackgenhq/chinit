# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# DNS Module Variables
# ============================================================================

variable "resource_group_name" {
  description = "Name of the Azure Resource Group"
  type        = string
}

variable "cluster_name" {
  description = "Name of the ClickHouse cluster"
  type        = string
}

variable "virtual_network_id" {
  description = "ID of the virtual network to link the DNS zone to"
  type        = string
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}

# ============================================================================
# Node Configuration Variables
# ============================================================================
# Why: Required for creating explicit DNS A records with predictable names.
#      Azure VMSS auto-registration creates hostnames like "vmss-keepers000000"
#      which don't match ClickHouse's expected pattern "keeper-0".
#      These variables allow us to create A records that map "keeper-0" to the
#      actual private IP addresses of the VMSS instances.
# Without it: DNS resolution fails because the registered hostnames don't match
#             the hostnames used in ClickHouse cluster configuration.
# ============================================================================

# ============================================================================
# Node Configuration Variables
# ============================================================================
variable "instance_counts" {
  description = "Number of nodes for each role"
  type = object({
    writer = optional(number, 1)
    reader = optional(number, 2)
    keeper = optional(number, 3)
  })
  default = {}
}

variable "backup_replica_config" {
  description = "Configuration for backup replica"
  type = object({
    enabled    = optional(bool, false)
    hostname   = optional(string, "backup-replica")
    private_ip = optional(string)
  })
  default = {}
}

# ============================================================================
# Naming Variables (from Azure Naming Module)
# ============================================================================
# Why: Provides centralized naming convention from the Azure Naming module.
#      All resource names are generated using Azure CAF-compliant patterns
#      and passed from the root module to ensure consistency.
# Without it: Resource names must be manually constructed, leading to
#             inconsistent naming across DNS resources.
# ============================================================================
variable "names" {
  description = "Naming outputs from the Azure Naming module"
  type = object({
    dns_zone      = string
    dns_vnet_link = string
  })
}

variable "managed_identity_principal_id" {
  description = "Principal ID of the user-assigned managed identity to grant DNS permissions"
  type        = string
}
