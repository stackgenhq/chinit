# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ACME Module Variables
# ============================================================================

variable "acme_config" {
  description = "ACME configuration for Let's Encrypt"
  type = object({
    email                   = string
    environment             = optional(string, "staging")
    cluster_name            = string
    base_domain             = string
    dns_resource_group_name = string
  })

  validation {
    condition     = contains(["staging", "production"], var.acme_config.environment)
    error_message = "acme_config.environment must be 'staging' or 'production'."
  }
}

variable "client_config" {
  description = "Azure Client Configuration"
  type = object({
    subscription_id = string
    tenant_id       = string
  })
}

variable "key_vault_id" {
  description = "ID of the Key Vault to store certificates"
  type        = string
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}
