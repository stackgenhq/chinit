# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# DNS + TLS Module Variables
# ============================================================================

variable "cluster_name" {
  description = "ClickHouse cluster name (used in zone and record names)"
  type        = string
}

variable "aws_region" {
  description = "AWS region (used by the ACME Route53 challenge provider)"
  type        = string
}

variable "vpc_id" {
  description = "VPC id to associate the private hosted zone with"
  type        = string
}

variable "private_zone_name" {
  description = "Private hosted zone name for node discovery (e.g. <cluster>.clickhouse.internal)"
  type        = string
}

variable "keeper_hostnames" {
  description = "Static keeper hostnames (keeper-0, keeper-1, ...)"
  type        = list(string)
}

variable "keeper_ips" {
  description = "Static private IPs for keepers, same order as keeper_hostnames"
  type        = list(string)
}

variable "backup_replica_enabled" {
  description = "Whether the dedicated backup replica exists (prod only)"
  type        = bool
  default     = false
}

variable "backup_hostname" {
  description = "Hostname of the backup replica (backup-0)"
  type        = string
  default     = "backup-0"
}

variable "backup_ip" {
  description = "Static private IP of the backup replica"
  type        = string
  default     = null
}

# ----------------------------------------------------------------------------
# Public zone / NLB records
# ----------------------------------------------------------------------------
variable "create_public_records" {
  description = "Create the public A records (writers_record_name/readers_record_name) in public_zone_name. Set false when the public zone is already managed elsewhere (e.g. another cluster owns those records) or when no public exposure is wanted."
  type        = bool
  default     = true
}

variable "public_zone_name" {
  description = "Route53 public hosted zone name for client DNS records (writers.<cluster>.<zone>, readers.<cluster>.<zone>). null = skip public records."
  type        = string
  default     = null
}

variable "writers_lb_dns_name" {
  description = "Writers NLB DNS name for the public A record"
  type        = string
}

variable "writers_lb_zone_id" {
  description = "Writers NLB hosted zone id (alias target)"
  type        = string
}

variable "readers_lb_dns_name" {
  description = "Readers NLB DNS name for the public A record"
  type        = string
}

variable "readers_lb_zone_id" {
  description = "Readers NLB hosted zone id (alias target)"
  type        = string
}

# ----------------------------------------------------------------------------
# TLS mode
# ----------------------------------------------------------------------------
variable "nlb_tls_mode" {
  description = "'passthrough': ACME certs terminate on ClickHouse (8443/9440), NLB is pure TCP passthrough. 'acm': ACM cert terminates at the NLB (frees the nodes from cert management)."
  type        = string
  default     = "passthrough"
}

variable "acm_certificate_arn" {
  description = "Existing ACM certificate ARN to attach to the NLB listeners (acm mode). Preferred over issuing a new cert: the module looks it up and fails the plan if it is not ISSUED. Must live in the deployment region and cover the public record names."
  type        = string
  default     = null
}

variable "acm_base_domain" {
  description = "Base domain used for the SAN when issuing a NEW ACM cert (acm mode with acm_certificate_arn = null)"
  type        = string
  default     = null
}

# ----------------------------------------------------------------------------
# Public record names (single label - must match the ACM cert SAN)
# ----------------------------------------------------------------------------
variable "writers_record_name" {
  description = "Public record name for the writers/ingest NLB (e.g. 'ch-writers' in ch-writers.example.io)"
  type        = string
  default     = "ch-writers"
}

variable "readers_record_name" {
  description = "Public record name for the readers/query NLB (e.g. 'ch' in ch.example.io)"
  type        = string
  default     = "ch"
}

# ----------------------------------------------------------------------------
# ACME (passthrough mode only)
# ----------------------------------------------------------------------------
variable "acme_email" {
  description = "Let's Encrypt account email (passthrough mode)"
  type        = string
  default     = null
}

variable "acme_environment" {
  description = "'staging' (default) for high rate limits while testing; 'production' for trusted certs"
  type        = string
  default     = "staging"
}

variable "public_zone_id" {
  description = "Route53 public hosted zone id for the ACME DNS-01 challenge (passthrough mode)"
  type        = string
  default     = null
}

variable "tls_secret_prefix" {
  description = "Secret name prefix for the TLS material in Secrets Manager"
  type        = string
  default     = "clickhouse"
}

variable "allowed_role_arns" {
  description = "IAM role ARNs (writer/reader/keeper/backup) allowed to read TLS secrets"
  type        = list(string)
  default     = []
}

variable "client_private_zone_name" {
  description = "Optional private hosted zone for private client endpoints (e.g. 'clickhouse.example.io'). Creates the zone, associates it with vpc_id, and creates readers/writers alias records pointing at the (internal) NLBs. Leave null to skip private client DNS entirely."
  type        = string
  default     = null
}

variable "client_readers_record_name" {
  description = "Record label for the internal readers endpoint inside client_private_zone_name (e.g. 'readers' -> readers.<zone>)."
  type        = string
  default     = "readers"
}

variable "client_writers_record_name" {
  description = "Record label for the internal writers endpoint inside client_private_zone_name (e.g. 'writers' -> writers.<zone>)."
  type        = string
  default     = "writers"
}

variable "client_zone_vpc_ids" {
  description = "VPC IDs to associate the private client zone with. Defaults to [vpc_id]. Additional VPCs (e.g. peered) can be listed."
  type        = list(string)
  default     = null
}

variable "create_client_private_zone" {
  description = "Set false to manage the private zone OUTSIDE this stack (zone_id passed via client_private_zone_id). Useful when the zone is shared across stacks."
  type        = bool
  default     = true
}

variable "client_private_zone_id" {
  description = "Existing private hosted zone ID to use instead of creating one (requires create_client_private_zone = false)."
  type        = string
  default     = null
}

variable "tags" {
  description = "Tags applied to all module resources"
  type        = map(string)
  default     = {}
}
