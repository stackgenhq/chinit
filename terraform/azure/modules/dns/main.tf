# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Private DNS Zone
# ============================================================================
# Why: Provides DNS resolution for VMSS instances, enabling ClickHouse nodes
#      to discover each other using hostnames instead of hardcoded IPs.
#      This is required for cluster configuration and replication.
# Without it: ClickHouse nodes cannot resolve each other's hostnames, causing
#             cluster formation failures, replication breaks, and distributed
#             query failures.
# ============================================================================
resource "azurerm_private_dns_zone" "clickhouse" {
  name                = var.names.dns_zone
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# Link DNS Zone to Virtual Network
# ============================================================================
# Why: Links the private DNS zone to the VNet, enabling VMs in the VNet to
#      resolve DNS names in the zone automatically.
# Without it: VMs cannot resolve DNS names in the zone, making the DNS zone
#             ineffective for VMSS instances.
# NOTE: registration_enabled is false because Azure VMSS auto-registration
#       creates hostnames like "vmss-keepers000000" which don't match our
#       expected pattern "keeper-0". We create explicit A records instead.
# ============================================================================
resource "azurerm_private_dns_zone_virtual_network_link" "clickhouse" {
  name                  = var.names.dns_vnet_link
  resource_group_name   = var.resource_group_name
  private_dns_zone_name = azurerm_private_dns_zone.clickhouse.name
  virtual_network_id    = var.virtual_network_id
  registration_enabled  = true # Auto-register VMs in this VNet
  tags                  = var.tags
}

# ============================================================================
# Explicit DNS A Records for ClickHouse Nodes
# ============================================================================
# Why: CRITICAL FIX for DNS resolution issue. Azure VMSS auto-registration
#      creates hostnames like "vmss-keepers000000", "vmss-readers000001", etc.
#      But ClickHouse cluster configuration expects hostnames like "keeper-0",
#      "reader-0", "writer-0". This mismatch causes DNS resolution failures,
#      preventing cluster nodes from discovering each other.
#
#      By creating explicit A records with predictable names, we ensure that:
#      1. ClickHouse remote_servers config can use "keeper-0.zone.internal"
#      2. Keeper/ZooKeeper config can use "keeper-0.zone.internal"
#      3. Cluster replication works correctly
#
# Without it: ClickHouse nodes fail to resolve each other's hostnames, causing:
#             - "Cannot resolve host keeper-0.clickhouse-cluster.internal"
#             - Keeper/ZooKeeper connection failures
#             - Distributed DDL operations timeout
#             - Replication failures
# ============================================================================

# Manual A Records removed - using Auto-registration with VMSS Flex / Fixed VMs

# ============================================================================
# RBAC for Managed Identity
# ============================================================================
# Why: Allows VMSS instances (via Managed Identity) to update their own DNS
#      records in the Private DNS Zone. cloud-init.sh uses Azure CLI to
#      update the A records with the actual private IP of the instance.
# Without it: cloud-init.sh fails with "AuthorizationFailed" when attempting
#             to update DNS records.
# ============================================================================
# RBAC removed - cloud-init no longer needs to update DNS records
