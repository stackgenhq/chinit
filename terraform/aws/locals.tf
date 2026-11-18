# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — chinit cluster module (AWS)
# ============================================================================
# The spec/bundle/wrapper rendering lives in modules/chinit/aws (the
# contract module) — this file keeps only the cluster-level derivations
# the all-in-one resources need.
# ============================================================================

locals {
  name_prefix = var.cluster_name

  # Static private IPs for the fixed-identity roles (the octet bands
  # chinit's StaticHostname maps: keepers +20..29, backup +30; the
  # first four addresses of a subnet are AWS-reserved).
  keeper_ips = [for i in range(var.keeper_count) : cidrhost(var.subnet_cidr, 20 + i)]
  backup_ip  = var.enable_backup ? cidrhost(var.subnet_cidr, 30) : null

  keeper_hostnames = [for i in range(var.keeper_count) : "keeper-${i}"]

  # Effective network ids (bring-your-own or module-created).
  vpc_id    = var.create_network ? aws_vpc.this[0].id : var.vpc_id
  subnet_id = var.create_network ? aws_subnet.nodes[0].id : var.subnet_id

  common_tags = merge(var.tags, {
    Cluster = var.cluster_name
  })
}
