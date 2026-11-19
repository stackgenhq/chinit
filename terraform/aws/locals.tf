# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — chinit cluster module (AWS, ecosystem-composed)
# ============================================================================
# The spec/bundle/wrapper rendering lives in modules/clickhouse (the
# ecosystem module wrapping the chinit contract); this file keeps only the
# cluster-level derivations the facade wiring needs.
# ============================================================================

locals {
  name_prefix = var.cluster_name

  # Static private IPs for the fixed-identity roles (the octet bands
  # chinit's StaticHostname maps: keepers +20..29, backup +30; the first
  # four addresses of a subnet are AWS-reserved).
  keeper_ips = [for i in range(var.keeper_count) : cidrhost(var.subnet_cidr, 20 + i)]
  backup_ip  = var.enable_backup ? cidrhost(var.subnet_cidr, 30) : null

  keeper_hostnames = [for i in range(var.keeper_count) : "keeper-${i}"]
  writer_hostnames = [for i in range(var.writer_max) : "writer-${i}"]

  # S3 bucket names (globally unique; the caller can pin them).
  data_bucket_name   = var.data_bucket_name != null ? var.data_bucket_name : "${var.cluster_name}-clickhouse-data"
  backup_bucket_name = var.backup_bucket_name != null ? var.backup_bucket_name : "${var.cluster_name}-clickhouse-backup"

  rotation_enabled = var.enable_secret_rotation

  # Public Golden AMI auto-discovery (cross-account safe): AWS hides tags
  # on public AMIs, so discovery filters on visibility/architecture/name/
  # description (which embeds the antalya/upstream flavor + repo URL).
  desired_flavor = var.cas_enabled ? "antalya" : "upstream"

  common_tags = merge(var.tags, {
    Cluster = var.cluster_name
  })
}
