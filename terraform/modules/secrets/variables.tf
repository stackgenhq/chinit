# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Secrets Module Variables — AWS ClickHouse Cluster
# ============================================================================
# Cluster identity + password inputs, adopted to AWS-native primitives:
# Secrets Manager, IAM role ARNs instead of managed-identity principal ids.
# ============================================================================

variable "cluster_name" {
  description = "Name of the ClickHouse cluster (used in secret names: <cluster_name>-default-password etc.)"
  type        = string
}

# ----------------------------------------------------------------------------
# Credentials
# ----------------------------------------------------------------------------
# Why: Matches the root module's `credentials_config` object exactly so the
#      root can pass its variable through 1:1. null = generate a random password.
#      Kept sensitive so a `null_password = null` plan/apply never renders a
#      generated value into CLI output or state-file excerpts.
# Without it: Root module would need to re-shape the object, and each password
#             would drift from the schema the root documents.
variable "credentials_config" {
  description = "ClickHouse user passwords. null values are generated (16 chars, alphanumeric only)."
  type = object({
    default_password = optional(string)
    admin_password   = optional(string)
    writer_password  = optional(string)
  })
  default   = {}
  sensitive = true
}

# ----------------------------------------------------------------------------
# TLS cert secret naming
# ----------------------------------------------------------------------------
# Why: The ACME/dns_tls module stores the rendered TLS material in Secrets
#      Manager under names prefixed with this value
#      (<prefix>-tls-certificate, <prefix>-tls-private-key,
#      <prefix>-tls-ca-certificate), storing ACME certs next to the
#      passwords. This module
#      does NOT create those secrets (dns_tls owns them); it owns the naming
#      convention so every cluster secret is discoverable from one prefix
#      and runbooks never hard-code names (exposed as the tls_secret_names
#      output).
# Without it: Password secrets and TLS secrets would live under unrelated
#             naming schemes, making discovery
#             (aws secretsmanager list-secrets) messy and cross-module
#             references error-prone.
variable "tls_secret_name_prefix" {
  description = "Prefix for TLS certificate secret names (dns_tls creates <prefix>-tls-certificate/-private-key/-ca-certificate)"
  type        = string
  default     = "clickhouse"
}

variable "tags" {
  description = "A mapping of tags to assign to all resources"
  type        = map(string)
  default     = {}
}

# ----------------------------------------------------------------------------
# Instance-profile access
# ----------------------------------------------------------------------------
# Why: ClickHouse nodes fetch passwords at boot (cloud-init renders
#      /etc/clickhouse-server/users.d/*.xml from the secret) and on every
#      password rotation. AWS Secrets Manager secret policies are
#      resource-based policies evaluated IN ADDITION to IAM identity policies,
#      so a resource policy is required to grant cross-role GetSecretValue.
#      The roles (writer/reader/keeper/backup) are created in the storage or
#      compute module; this module only receives the finished ARNs.
# Without it: Instance roles would get AccessDenied on GetSecretValue even
#             though their identity policy allows it; cloud-init would fail to
#             render users.d and the cluster would come up without any users.
variable "allowed_role_arns" {
  description = "IAM instance-profile role ARNs granted GetSecretValue on all password secrets (writer/reader/keeper/backup roles)"
  type        = list(string)
  default     = []
}

# ----------------------------------------------------------------------------
# Rotation
# ----------------------------------------------------------------------------
# Why: Rotation needs to be configurable in two axes: on/off (dev profiles may
#      want static passwords to keep local playground setups simple) and cadence
#      (days). AWS native rotation only ships predefined Lambdas for RDS
#      (RotationStrategy), so custom passwords require a custom Lambda — hence
#      the rotation object drives both the rotation schedule and the Lambda.
# Without it: Passwords would be static forever (compliance drift) or forced on
#             dev environments that intentionally want stable credentials.
variable "rotation" {
  description = "Password rotation settings. When enabled, a rotation Lambda is deployed and scheduled."
  type = object({
    enabled       = optional(bool, true)
    rotation_days = optional(number, 30)
  })
  default = {}

  validation {
    condition     = var.rotation.rotation_days == null || (var.rotation.rotation_days >= 1 && var.rotation.rotation_days <= 365)
    error_message = "rotation.rotation_days must be between 1 and 365."
  }
}
