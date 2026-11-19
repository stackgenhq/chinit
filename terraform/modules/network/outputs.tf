# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Network Module Outputs
# ============================================================================
# Exact names required by the root module and sibling modules (compute,
# storage, dns). Consumed values: vpc/subnet ids, per-role security groups,
# NAT public IP (for allowlisting / NAT egress), and flow-log bucket ARN.
# ============================================================================

output "vpc_id" {
  description = "ID of the VPC hosting the ClickHouse cluster."
  value       = local.vpc_id_effective
}

output "vpc_cidr_block" {
  description = "IPv4 CIDR block of the VPC (for allowlisting the S3 endpoint policy and any peering)."
  value       = local.is_create_mode ? var.vpc_cidr : var.existing_vpc_cidr
}

output "subnet_id" {
  description = "ID of the PRIVATE nodes subnet (all EC2 roles attach here; no IGW route ever)."
  value       = local.nodes_subnet_id
}

output "public_subnet_id" {
  description = "ID of the PUBLIC subnet (internet-facing NLBs + NAT gateway only; no compute is ever placed here)."
  value       = local.is_create_mode ? values(module.vpc[0].subnet_ids_by_role.public)[0] : local.nlb_subnet_ids[0]
}

output "public_subnet_cidr_block" {
  description = "CIDR of the public (NLB/NAT) subnet"
  value       = local.is_create_mode ? local.public_subnet_cidr : local.nlb_subnet_cidrs[0]
}

output "subnet_cidr_block" {
  description = "IPv4 CIDR block of the nodes subnet."
  value       = local.nodes_subnet_cidr_eff
}

output "writers_sg_id" {
  description = "Security group ID for writer nodes (client ports, interserver, keeper access)."
  value       = aws_security_group.writers.id
}

output "readers_sg_id" {
  description = "Security group ID for reader nodes (client ports, interserver, keeper access)."
  value       = aws_security_group.readers.id
}

output "keepers_sg_id" {
  description = "Security group ID for keeper nodes (Raft 9234 self-only)."
  value       = aws_security_group.keepers.id
}

output "backup_sg_id" {
  description = "Security group ID for the backup replica (interserver replication pull only)."
  value       = aws_security_group.backup.id
}

output "nat_gateway_public_ip" {
  description = "Public IP of the NAT gateway's EIP (for egress allowlisting), or null when enable_nat_gateway is false."
  value       = local.nat_public_ip
}

output "flow_log_bucket_arn" {
  description = "ARN of the S3 bucket receiving VPC flow logs, or null when flow logs are disabled. Retention is enforced by the module only for the module-created bucket."
  value       = var.enable_flow_logs ? (var.flow_log_bucket_arn != null ? var.flow_log_bucket_arn : aws_s3_bucket.flow_logs[0].arn) : null
}

output "subnet_availability_zone" {
  description = "AZ of the PRIVATE nodes subnet. Passed to compute as a plain string so the EBS volume AZ does not depend on a data-source read at plan time (the read-during-apply pattern forced volume replacement on every apply - the churn loop). Sourced from the aws-ia/vpc module output (az_count=1 pins the topology to one AZ so writer IPs, EBS volumes, and NLBs stay co-located)."
  value       = local.nodes_az_eff
}

output "multi_az_enabled" {
  description = "The multi-AZ compute placement opt-in this topology was built with (mirrors the root's multi_az_config.enabled; consumers key their placement behavior on it)."
  value       = var.multi_az
}

output "nodes_subnet_ids" {
  description = "Private nodes subnet ids: the Single-AZ shape answers [primary subnet]; the multi-AZ opt-in appends one subnet per extra AZ (writers/readers ASGs spread across them; keepers/backup stay in the primary)."
  value       = local.nodes_subnet_ids
}

output "nodes_subnet_cidrs" {
  description = "CIDRs of the private nodes subnets (see nodes_subnet_ids for the AZ layout). Rendered into the chinit specs' network section so cross-AZ peers pass the ClickHouse <networks> allowlists."
  value       = local.nodes_subnet_cidrs
}

output "nat_gateway_public_ips" {
  description = "All NAT gateway egress IPs (one per AZ under the multi-Az opt-in; a single-element list in the Single-AZ posture). null when NAT is disabled."
  value       = local.is_create_mode && var.enable_nat_gateway ? (var.multi_az ? sort(local.nat_public_ips_single) : local.nat_public_ips_single) : null
}

output "nlb_subnet_ids" {
  description = "Subnet ids to attach the ClickHouse NLBs to (public subnet in create mode; internal/private subnets in existing mode)."
  value       = local.nlb_subnet_ids
}

output "nlb_subnet_cidr_blocks" {
  description = "CIDRs of the NLB subnets (used for SG rules so NLB health-check + forwarded traffic is allowed)."
  value       = local.nlb_subnet_cidrs
}
