# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse Module Variables
# ============================================================================
# This file defines all input variables for the ClickHouse module.
# ============================================================================

variable "cluster_name" {
  description = "Name of the ClickHouse cluster"
  type        = string
}

# ============================================================================
# Subscription ID (chinit ARM scope)
# ============================================================================
# Why: chinit's Azure provider resolves THIS VM's ARM resource id from
#      CHINIT_AZURE_SUBSCRIPTION_ID + CHINIT_AZURE_RESOURCE_GROUP +
#      CHINIT_AZURE_VM_NAME (Private DNS zone operations and the volume
#      grower's managed-disk resize both key on the scope). The wrapper
#      exports the env from this value; the chinit spec stays
#      provider-neutral (names never ride in it).
# Without it: chinit's DNS registration and disk-autoscale fail at the
#             ARM scope lookup ("azure ARM scope not configured").
# ============================================================================
variable "subscription_id" {
  description = "Azure subscription ID (chinit ARM scope env pin)"
  type        = string
}

# ============================================================================
# VM Names per role (chinit ARM scope)
# ============================================================================
# Why: the ARM scope's virtualMachines/<name> segment differs per role
#      (the VMSS resource name for writers/readers, the fixed keeper VM
#      names, the backup replica VM name). The wrapper exports the
#      role's name so the node's own ARM id resolves.
# Without it: the volume grower and Private DNS resolve the wrong VM
#             (or none) and their ARM calls fail.
# ============================================================================
variable "vm_names" {
  description = "Per-role VM names for the chinit ARM scope env"
  type = object({
    writers = string
    readers = string
    keepers = string
    backup  = string
  })
}


# ============================================================================
# Storage Configuration
# ============================================================================
variable "storage_config" {
  description = "Configuration for ClickHouse data storage (account, container, keys)"
  type = object({
    account_name                       = string
    account_id                         = optional(string)
    account_key                        = optional(string)
    container_name                     = string
    lookup_account                     = optional(bool, false)
    use_managed_identity               = optional(bool, true)
    managed_identity_client_id         = optional(string)
    use_managed_identity_for_bootstrap = optional(bool, true)
    # The chinit ordinal-lease Storage Table (<cluster>-node-claims):
    # writers/readers claim identity from it at boot (the Azure mirror
    # of the AWS node-claims DynamoDB table).
    node_claims_table_name = optional(string, "")
  })
}

variable "writer_count" {
  description = "Number of writer nodes"
  type        = number
  default     = 1
}

variable "reader_count" {
  description = "Number of reader nodes"
  type        = number
  default     = 0
}

variable "keeper_count" {
  description = "Number of ClickHouse Keeper nodes"
  type        = number
  default     = 3
}


variable "dns_zone_name" {
  description = "Azure Private DNS Zone name for hostname registration"
  type        = string
  default     = null
}

variable "resource_group_name" {
  description = "Resource group name for DNS operations"
  type        = string
  default     = null
}


# ============================================================================
# Telemetry Configuration (Logs, Metrics, Traces)
# ============================================================================
variable "telemetry_config" {
  description = "Configuration for telemetry (logs, metrics, traces)"
  type = object({
    log_analytics_workspace_id = optional(string)
    data_collection_rule_id    = optional(string)
    otel_endpoint              = optional(string)
    otel_headers               = optional(string)
    opsverse_agent_password    = optional(string)
  })
  default = {}
}

# ============================================================================
# Backup Configuration
# ============================================================================
variable "backup_config" {
  description = "Configuration for backups and backup replica"
  type = object({
    storage_account_name   = string
    storage_container_name = string
    retention_days         = optional(number, 7)
    compression_format     = optional(string, "tar")
    tool_version           = optional(string, "2.5.0")
    replica = optional(object({
      enabled  = optional(bool, false)
      hostname = optional(string)
    }), {})
  })
}

variable "key_vault_name" {
  description = "Azure Key Vault name for password storage"
  type        = string
  default     = null
}


# ============================================================================
# TLS Configuration
# SECURITY: TLS is MANDATORY and always enabled.
# ============================================================================
# ============================================================================
# TLS Configuration
# ============================================================================
variable "tls_config" {
  description = "Configuration for TLS certificates"
  type = object({
    certificate_secret_name    = optional(string)
    private_key_secret_name    = optional(string)
    ca_certificate_secret_name = optional(string)
  })
  default = {}
}

# ============================================================================
# OpsVerse Agent Configuration
# ============================================================================

# ============================================================================
# User Network Access Restrictions
# ============================================================================
# Why: Per ClickHouse security best practices, user access should be restricted
#      to specific IP ranges based on their role. This prevents compromised
#      credentials from being exploitable from arbitrary networks.
# Reference: BLINDSPOTS.md item #3 - User Network Access Too Permissive
# Without it: All users can connect from any IP (::/0), which is a security risk.
#             Compromised credentials can be used from anywhere on the internet.
# ============================================================================

# ============================================================================
# Network Access Configuration
# ============================================================================
variable "network_access_config" {
  description = "Configuration for network access restrictions"
  type = object({
    admin_allowed_networks        = optional(list(string), [])
    writer_allowed_networks       = optional(list(string), [])
    default_user_allowed_networks = optional(list(string), [])
    subnet_cidr                   = optional(string, "10.0.1.0/24")
  })
  default = {}
}

variable "timezone" {
  description = "Timezone for the ClickHouse cluster"
  type        = string
  default     = "UTC"
}

# ============================================================================
# chinit (the node bootstrapper — the cloud-init.sh Go port)
# ============================================================================
# Why: The wrapper upgrades the gallery-baked chinit to this version from
#      the staged artifacts blob (checksum-gated like the release flow). The sha256
#      is the linux-arm64 release checksum; mismatch = refuse to install.
# Without it: The wrapper cannot pin the binary it execs.
# ============================================================================
variable "location" {
  description = "Azure region (feeds the chinit spec's region for backup/log endpoints)"
  type        = string
}




variable "keeper_hostname_prefix" {
  description = "Hostname prefix for keeper nodes (clickhouse-keeper-N in the bash era)"
  type        = string
  default     = "keeper-"
}

