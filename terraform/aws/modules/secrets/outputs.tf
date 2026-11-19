# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Secrets Module Outputs
# ============================================================================
# Why: Outputs the secret identifiers (not values) so the root module and
#      other modules (clickhouse cloud-init, monitoring) can reference
#      secrets by ARN/name without ever handling the password plaintext.
#      The contract: values live in Secrets Manager only — retrieved at
#      runtime via GetSecretValue, never rendered into a root-module output.
# Without it: Consumers would need to re-derive secret names (fragile string
#             duplication) or the module would have to leak plaintext, which
#             would land in CLI output, CI logs, and the root state file.

# ----------------------------------------------------------------------------
# Default user
# ----------------------------------------------------------------------------
# Why: ARN is the canonical reference for IAM policies and Lambda code; name
#      is what the root module passes to cloud-init (its secret-name
#      variables) and what operators type into CLI/debug commands; version id
#      lets consumers pin the exact version Terraform created rather than
#      drift to a later rotated version (sensitive because version metadata
#      of a password secret should never appear in plain CLI output).
# Without it: The root would need to re-derive secret names (fragile string
#             duplication) and there would be no way to correlate the
#             Terraform-created version with what the fleet rendered.
output "default_password_secret_arn" {
  description = "ARN of the default user password secret"
  value       = aws_secretsmanager_secret.default_password.arn
}

output "default_password_secret_name" {
  description = "Name of the default user password secret"
  value       = aws_secretsmanager_secret.default_password.name
}

output "default_password_secret_version_id" {
  description = "Version id of the Terraform-created default password version"
  value       = aws_secretsmanager_secret_version.default_password.version_id
  sensitive   = true
}

# ----------------------------------------------------------------------------
# Admin user
# ----------------------------------------------------------------------------
# Why: Same rationale as the default-user trio; kept as separate named
#      outputs (not a map) so the root module can hand each value to the
#      consumer that needs exactly that user's credential, and so
#      `tofu output default_password_secret_name` reads cleanly in runbooks.
# Without it: Root module would index a map keyed by user — a stringly-typed
#             contract where a typo silently yields null.
output "admin_password_secret_arn" {
  description = "ARN of the admin user password secret"
  value       = aws_secretsmanager_secret.admin_password.arn
}

output "admin_password_secret_name" {
  description = "Name of the admin user password secret"
  value       = aws_secretsmanager_secret.admin_password.name
}

output "admin_password_secret_version_id" {
  description = "Version id of the Terraform-created admin password version"
  value       = aws_secretsmanager_secret_version.admin_password.version_id
  sensitive   = true
}

# ----------------------------------------------------------------------------
# Writer user
# ----------------------------------------------------------------------------
# Why: Same rationale as the default-user trio; the writer secret is the one
#      handed to ingest pipelines and therefore rotated most often, so its
#      identifiers are referenced by name in the most runbooks.
# Without it: Ingest pipelines and runbooks would hard-code the secret name
#             and break on any cluster_name change.
output "writer_password_secret_arn" {
  description = "ARN of the writer user password secret"
  value       = aws_secretsmanager_secret.writer_password.arn
}

output "writer_password_secret_name" {
  description = "Name of the writer user password secret"
  value       = aws_secretsmanager_secret.writer_password.name
}

output "writer_password_secret_version_id" {
  description = "Version id of the Terraform-created writer password version"
  value       = aws_secretsmanager_secret_version.writer_password.version_id
  sensitive   = true
}

output "default_password" {
  description = "Default (read-only) user password value (sensitive; for CLI quick-connect outputs)"
  value       = local.default_password_value
  sensitive   = true
}

output "admin_password" {
  description = "Admin user password value (sensitive; for CLI quick-connect outputs)"
  value       = local.admin_password_value
  sensitive   = true
}

output "writer_password" {
  description = "Writer user password value (sensitive; for CLI quick-connect outputs)"
  value       = local.writer_password_value
  sensitive   = true
}

# ----------------------------------------------------------------------------
# Aggregate map (all three users)
# ----------------------------------------------------------------------------
# Why: A single map keyed by ClickHouse user lets consumers iterate over
#      users (for u in default/admin/writer: fetch ${secret_names[u]}) — and
#      the module contract explicitly asks for a secret_names map for
#      exactly that bulk-render use case.
# Without it: Consumers would juggle three parallel variables and keep their
#             order in sync — the off-by-one bug class for_each eliminates.
output "secret_names" {
  description = "Map of ClickHouse user -> password secret name (default/admin/writer)"
  value = {
    "default" = aws_secretsmanager_secret.default_password.name
    "admin"   = aws_secretsmanager_secret.admin_password.name
    "writer"  = aws_secretsmanager_secret.writer_password.name
  }
}

output "secret_arns" {
  description = "Map of ClickHouse user -> password secret ARN (default/admin/writer)"
  value = {
    "default" = aws_secretsmanager_secret.default_password.arn
    "admin"   = aws_secretsmanager_secret.admin_password.arn
    "writer"  = aws_secretsmanager_secret.writer_password.arn
  }
}

# ----------------------------------------------------------------------------
# TLS cert secret naming (documented convention)
# ----------------------------------------------------------------------------
# Why: The ACME/dns_tls module stores the cluster's TLS material (cert, key,
#      CA chain) in Secrets Manager under names prefixed with this value —
#      the AWS counterpart of the Azure keyvault module storing ACME certs
#      next to the passwords. This module does NOT create those secrets
#      (dns_tls owns them); it owns the NAMING CONVENTION so an operator can
#      discover every cluster secret from one place and so runbooks never
#      hard-code names. The Azure module documented the same coupling via
#      comments; an output makes it machine-readable here.
# Without it: Cert secret names would be discoverable only by reading the
#             dns_tls module source, and the prefix contract the root
#             exposes would be undocumented.
output "tls_secret_names" {
  description = "Reserved names of the TLS certificate secrets owned by the dns_tls module (convention documented by this module; created with the prefix passed to dns_tls)"
  value = {
    "certificate" = "${var.tls_secret_name_prefix}-tls-certificate"
    "private_key" = "${var.tls_secret_name_prefix}-tls-private-key"
    "ca_chain"    = "${var.tls_secret_name_prefix}-tls-ca-certificate"
  }
}

# ----------------------------------------------------------------------------
# Rotation
# ----------------------------------------------------------------------------
# Why: The root module surfaces rotation_lambda_arn in its own outputs so
#      operators can find the function in the console or add a CloudWatch
#      alarm on its errors; rotation_enabled documents the effective
#      setting after coalesce() normalization (a caller passing
#      { enabled = null } still gets the default true).
# Without it: Failed rotations would be undiscoverable — the schedule keeps
#             firing, silently leaving the password stale while the root
#             module shows nothing about rotation at all.
output "rotation_lambda_arn" {
  description = "ARN of the password rotation Lambda (null when rotation is disabled)"
  value       = local.rotation_enabled ? aws_lambda_function.rotation[0].arn : null
}

output "rotation_enabled" {
  description = "Whether password rotation is effectively enabled"
  value       = local.rotation_enabled
}

output "rotation_days" {
  description = "Effective rotation interval in days"
  value       = local.rotation_days
}

# ----------------------------------------------------------------------------
# Manual re-sync fallback
# ----------------------------------------------------------------------------
# Why: If the rotation Lambda fails mid-rotation (e.g. SSM unreachable, or
#      fleet instances have the SSM agent down), the fleet ends up rendered
#      with a password that no longer matches AWSCURRENT. This output is the
#      EXACT AWS CLI command to re-sync every node from the current secret
#      values: it targets the fleet by NodeType tag (writer/reader/backup —
#      keepers run clickhouse-keeper, not clickhouse-server, so they are
#      excluded), needs no instance ids, and renders users.d/zz-<user>-
#      password.xml + restarts clickhouse-server — the same operations the
#      rotation Lambda performs in its setSecret step. The script avoids
#      single quotes so the command can wrap the parameters in single quotes
#      and paste into any POSIX shell without escaping. The fleet must run
#      the SSM agent (preinstalled on the golden AMI and standard Ubuntu
#      AMIs, plus AmazonSSMManagedInstanceCore on every node role).
# Without it: Recovery would require SSH (this cluster disables SSH in favor
#             of SSM Session Manager) and hand-edited XML on every node.
output "ssm_manual_resync_command" {
  description = "Exact AWS CLI command to manually re-sync fleet passwords from Secrets Manager if the rotation Lambda fails"
  value = format(
    "aws ssm send-command --document-name AWS-RunShellScript --targets Key=tag:%s,Values=%s --parameters 'commands=%s' --region %s",
    local.ssm_target_tag_key,
    local.ssm_target_tag_value,
    jsonencode([local.manual_resync_script]),
    data.aws_region.current.region,
  )
}
