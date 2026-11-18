# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — chinit cluster module (Azure)
# ============================================================================
# The spec/bundle/wrapper rendering lives in modules/chinit/azure (the
# contract module) — this file keeps only the cluster-level derivations
# the all-in-one resources need.
# ============================================================================

locals {
  name_prefix = var.cluster_name

  # Static private IPs (the octet bands chinit's StaticHostname maps).
  keeper_ips = [for i in range(var.keeper_count) : cidrhost(var.subnet_cidr, 20 + i)]
  backup_ip  = var.enable_backup ? cidrhost(var.subnet_cidr, 30) : null

  keeper_hostnames = [for i in range(var.keeper_count) : "keeper-${i}"]

  # Effective network ids.
  vnet_id   = var.create_network ? azurerm_virtual_network.this[0].id : var.vnet_id
  subnet_id = var.create_network ? azurerm_subnet.nodes[0].id : var.subnet_id

  # The admin SSH key: caller-provided, or the module-generated one.
  admin_ssh_public_key = var.ssh_public_key != null ? var.ssh_public_key : tls_private_key.node_access[0].public_key_openssh

  common_tags = merge(var.tags, {
    cluster = var.cluster_name
  })
}
