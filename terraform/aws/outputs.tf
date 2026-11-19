# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Outputs — chinit cluster module (AWS, ecosystem-composed)
# ============================================================================

output "cluster_name" {
  description = "The cluster name (every spec, DNS name and resource carries it)."
  value       = var.cluster_name
}

output "golden_ami_id" {
  description = "The effective Golden AMI ID used by the fleet (pinned via var.golden_ami_id or auto-resolved from the public stackgenhq/chinit golden AMI matching var.cas_enabled)."
  value       = local.golden_ami_id
}

output "dns_zone_name" {
  description = "The private DNS zone nodes register into (writer-N/reader-N/keeper-N/backup-0.<zone>)."
  value       = var.dns_zone_name
}

output "private_zone_id" {
  description = "The private Route53 zone id."
  value       = module.dns_tls.private_zone_id
}

output "node_claims_table" {
  description = "The DynamoDB ordinal-lease table name (writer-N/reader-N claims)."
  value       = aws_dynamodb_table.node_claims.name
}

output "data_bucket" {
  description = "The S3 data bucket (the durable tier; parts under data/<tenant>/<replica>/)."
  value       = module.storage.data_bucket_name
}

output "backup_bucket" {
  description = "The S3 backup bucket (clickhouse-backup on the backup replica)."
  value       = module.storage.backup_bucket_name
}

output "config_files_hash" {
  description = "The SHA256 gate over the bundle's hashed members. chinit recomputes this at boot; a mismatch aborts the boot (stale bundle / stale tfvars)."
  value       = module.clickhouse.config_files_hash
}

output "secrets" {
  description = "The Secrets Manager secret NAMES the specs carry (values resolve at boot over the instance role — never in user-data)."
  value = {
    default_password = module.secrets.default_password_secret_name
    admin_password   = module.secrets.admin_password_secret_name
    writer_password  = module.secrets.writer_password_secret_name
  }
}

output "writers_asg_name" {
  description = "The writers ASG name (on-demand only; scale/refresh via the AWS API or console, bounded by writer_max)."
  value       = module.compute.writers_asg_name
}

output "readers_asg_name" {
  description = "The readers ASG name (Spot-capable; bounded by reader_max — the ordinal claim ceiling)."
  value       = module.compute.readers_asg_name
}

output "keeper_ips" {
  description = "The static keeper private IPs (the +20 octet offsets)."
  value       = module.compute.keeper_ips
}

output "keeper_hostnames" {
  description = "Keeper hostnames (keeper-0, keeper-1, ...) — the Raft ensemble inventory."
  value       = local.keeper_hostnames
}

output "node_endpoints" {
  description = "Client-facing hostnames (resolve inside the VPC): writers/readers by role slot, keepers, backup."
  value = {
    writers = [for h in local.writer_hostnames : "${h}.${var.dns_zone_name}"]
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

output "alert_topic_arn" {
  description = "The cluster SNS alerts topic (alarms + HIGH/CRITICAL GuardDuty/Security Hub findings land here)."
  value       = module.monitoring.alert_topic_arn
}

output "connect_writer" {
  description = "Ready-to-run clickhouse client command for a writer slot (use: $(tofu output connect_writer --raw))."
  value       = "clickhouse client --host writer-0.${var.dns_zone_name} --port 9000 --user writer --password $(aws secretsmanager get-secret-value --secret-id ${module.secrets.writer_password_secret_name} --query SecretString --output text)"
}
