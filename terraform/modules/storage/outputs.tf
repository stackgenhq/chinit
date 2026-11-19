# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Storage Module Outputs
# ============================================================================
# Why: The outputs are the module's contract with the root: they hand the
#      buckets/prefixes to the clickhouse module (storage-config.xml, the
#      storage layer config), the instance profiles to
#      compute, and the per-tenant policy documents to whatever principal
#      model each tenant ends up with. Everything the root needs from
#      storage flows through these names - nothing reads bucket ids out
#      of state directly.
# Without it: The root, compute module, and clickhouse module would need
#             hard-coded bucket names and role names, breaking the
#             module's reusability and inviting naming drift.
# ============================================================================

# ----------------------------------------------------------------------------
# Bucket identifiers
# ----------------------------------------------------------------------------
output "data_bucket_name" {
  description = "Name of the active unversioned S3 bucket holding ClickHouse parts (supports both non-CAS data/ and CAS cas/ prefixes)."
  value       = aws_s3_bucket.cas[0].id
}

output "data_bucket_arn" {
  description = "ARN of the active data bucket. Reference for bucket policies and IAM resource scopes."
  value       = aws_s3_bucket.cas[0].arn
}

output "backup_bucket_name" {
  description = "Name of the S3 bucket holding clickhouse-backup output."
  value       = aws_s3_bucket.backup.id
}

output "backup_bucket_arn" {
  description = "ARN of the backup bucket."
  value       = aws_s3_bucket.backup.arn
}

# ----------------------------------------------------------------------------
# Prefixes
# ----------------------------------------------------------------------------
output "data_prefix" {
  description = "S3 key prefix holding all ClickHouse parts (empty string = bucket root). Consumed by the clickhouse module to build S3 disk endpoints."
  value       = var.data_prefix
}

output "backup_prefix" {
  description = "S3 key prefix inside the backup bucket where clickhouse-backup writes (empty string = bucket root)."
  value       = var.backup_prefix
}

output "bootstrap_prefix" {
  description = "S3 key prefix (in the data bucket) where the clickhouse module uploads stage-2 cloud-init; node roles have read access to this prefix."
  value       = var.bootstrap_prefix
}

# ----------------------------------------------------------------------------
# Multi-tenancy
# ----------------------------------------------------------------------------
output "tenant_prefixes" {
  description = "Per-tenant key prefixes under the data prefix: data/<tenant>/. ClickHouse writes parts into these keys directly - the prefixes exist for tooling (query cost-per-tenant, list a tenant's parts) and policy scoping, not as provisioned objects."
  value       = local.tenant_prefixes
}

output "tenant_policy_documents" {
  description = "Map of tenant -> IAM policy JSON granting read/write only to data/<tenant>/* plus a ListBucket scoped to that prefix. Attach to per-tenant principals (federated users, cross-account roles, log shippers) to enforce tenant isolation in the shared bucket."
  value       = { for t in var.tenants : t => data.aws_iam_policy_document.tenant[t].json }
}

# ----------------------------------------------------------------------------
# Node IAM roles & instance profiles (writer / reader / keeper / backup)
# ----------------------------------------------------------------------------
# Why: The compute module hands each ASG its instance profile; the outputs
#      expose both the ARN and the name so consumers can use whichever
#      their API wants without string surgery on the ARN.
# Without it: The compute module could not wire roles to instances without
#             hardcoding naming conventions, and any rename here would
#             silently break the cluster's storage access.
# ----------------------------------------------------------------------------
output "writer_role_arn" {
  description = "IAM role ARN for writer nodes."
  value       = aws_iam_role.node["writer"].arn
}

output "writer_role_name" {
  description = "IAM role name for writer nodes."
  value       = aws_iam_role.node["writer"].name
}

output "writer_instance_profile_arn" {
  description = "Instance profile ARN for writer nodes (attach to the writer ASG)."
  value       = aws_iam_instance_profile.node["writer"].arn
}

output "writer_instance_profile_name" {
  description = "Instance profile name for writer nodes."
  value       = aws_iam_instance_profile.node["writer"].name
}

output "reader_role_arn" {
  description = "IAM role ARN for reader nodes."
  value       = aws_iam_role.node["reader"].arn
}

output "reader_role_name" {
  description = "IAM role name for reader nodes."
  value       = aws_iam_role.node["reader"].name
}

output "reader_instance_profile_arn" {
  description = "Instance profile ARN for reader nodes (attach to the reader ASG)."
  value       = aws_iam_instance_profile.node["reader"].arn
}

output "reader_instance_profile_name" {
  description = "Instance profile name for reader nodes."
  value       = aws_iam_instance_profile.node["reader"].name
}

output "keeper_role_arn" {
  description = "IAM role ARN for keeper nodes."
  value       = aws_iam_role.node["keeper"].arn
}

output "keeper_role_name" {
  description = "IAM role name for keeper nodes."
  value       = aws_iam_role.node["keeper"].name
}

output "keeper_instance_profile_arn" {
  description = "Instance profile ARN for keeper nodes (attach to the keeper ASG)."
  value       = aws_iam_instance_profile.node["keeper"].arn
}

output "keeper_instance_profile_name" {
  description = "Instance profile name for keeper nodes."
  value       = aws_iam_instance_profile.node["keeper"].name
}

output "backup_role_arn" {
  description = "IAM role ARN for the backup replica."
  value       = aws_iam_role.node["backup"].arn
}

output "backup_role_name" {
  description = "IAM role name for the backup replica."
  value       = aws_iam_role.node["backup"].name
}

output "backup_instance_profile_arn" {
  description = "Instance profile ARN for the backup replica (attach to the backup ASG)."
  value       = aws_iam_instance_profile.node["backup"].arn
}

output "backup_instance_profile_name" {
  description = "Instance profile name for the backup replica."
  value       = aws_iam_instance_profile.node["backup"].name
}
