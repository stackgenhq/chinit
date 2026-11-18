# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Outputs — chinit cluster module (AWS)
# ============================================================================

output "cluster_name" {
  description = "The cluster name (every spec, DNS name and resource carries it)."
  value       = var.cluster_name
}

output "golden_ami_id" {
  description = "The effective Golden AMI ID used by writers, readers, keepers, and backup (either explicitly pinned via var.golden_ami_id or auto-resolved from the public stackgenhq/chinit Golden AMI matching var.cas_enabled)."
  value       = local.golden_ami_id
}

output "dns_zone_name" {
  description = "The private DNS zone nodes register into (writer-N/reader-N/keeper-N/backup-0.<zone>)."
  value       = var.dns_zone_name
}

output "node_claims_table" {
  description = "The DynamoDB ordinal-lease table name (writer-N/reader-N claims)."
  value       = aws_dynamodb_table.node_claims.name
}

output "bundle_bucket" {
  description = "The S3 bucket holding the config bundle (specs + XMLs + chi.yaml)."
  value       = aws_s3_bucket.bootstrap.id
}

output "data_bucket" {
  description = "The S3 data bucket (the ClickHouse cold tier; parts under s3_data_prefix/tiered/<replica>/)."
  value       = aws_s3_bucket.data.id
}

output "backup_bucket" {
  description = "The S3 backup bucket (clickhouse-backup on the backup replica)."
  value       = aws_s3_bucket.backup.id
}

output "config_files_hash" {
  description = "The SHA256 gate over the bundle's hashed members. chinit recomputes this at boot; a mismatch is a fatal error pointing at stale tfvars or an overwritten bucket."
  value       = module.chinit.config_files_hash
}

output "secrets" {
  description = "The Secrets Manager secret NAMES the specs carry (values resolve at boot over the instance role — never in state-adjacent files)."
  value = {
    default_password = aws_secretsmanager_secret.default_password.name
    admin_password   = aws_secretsmanager_secret.admin_password.name
    writer_password  = aws_secretsmanager_secret.writer_password.name
  }
}

output "writers_asg_name" {
  description = "The writers ASG name (scale/refresh via the AWS API or console)."
  value       = aws_autoscaling_group.writers.name
}

output "readers_asg_name" {
  description = "The readers ASG name (spot-capable; scale to reader_max at most — it is the ordinal claim ceiling)."
  value       = aws_autoscaling_group.readers.name
}

output "keeper_ips" {
  description = "The static keeper private IPs (the +20 octet offsets)."
  value       = local.keeper_ips
}

output "keeper_hostnames" {
  description = "Keeper hostnames (keeper-0, keeper-1, ...) — the Raft ensemble inventory."
  value       = local.keeper_hostnames
}

output "node_endpoints" {
  description = "Client-facing hostnames (resolve inside the VPC): writers and readers by role, keepers, backup."
  value = {
    writers = [for i in range(var.writer_max) : "writer-${i}.${var.dns_zone_name}"]
    readers = [for i in range(var.reader_max) : "reader-${i}.${var.dns_zone_name}"]
    keepers = [for h in local.keeper_hostnames : "${h}.${var.dns_zone_name}"]
    backup  = var.enable_backup ? ["backup-0.${var.dns_zone_name}"] : []
  }
}

output "ssm_targets" {
  description = "Tag-based SSM target filters for the fleet (session access: no bastion, no SSH keys)."
  value = {
    writers = "tag:NodeType=writer"
    readers = "tag:NodeType=reader"
    keepers = "tag:NodeType=keeper"
    backup  = "tag:NodeType=backup"
  }
}
