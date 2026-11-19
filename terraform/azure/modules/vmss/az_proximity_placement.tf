# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Proximity Placement Group for ClickHouse Cluster
# ============================================================================
# Why: Proximity Placement Groups ensure VMs are physically co-located in the
#      same data center, reducing network latency by 27-33%. This is critical
#      for distributed databases like ClickHouse that require low-latency
#      inter-node communication for:
#      - Distributed query execution across shards
#      - Replication between writer nodes
#      - ClickHouse Keeper consensus operations
# Reference: Implementation Plan - Azure ClickHouse Infrastructure Optimizations
# Trade-off: PPGs can occasionally cause allocation failures if requested VM
#            sizes are unavailable in the automatically selected data center.
#            PPGs also do NOT span availability zones, which slightly reduces
#            HA scope in exchange for lower latency.
# Without it: VMs may be scattered across different data centers even within
#             the same availability zone, causing 27-33% higher inter-node
#             latency, slower distributed queries, and increased replication lag.
# ============================================================================
resource "azurerm_proximity_placement_group" "clickhouse" {
  count               = var.enable_proximity_placement_group ? 1 : 0
  name                = "${var.cluster_name}-ppg"
  location            = var.location
  resource_group_name = var.resource_group_name

  tags = merge(var.tags, {
    "purpose" = "clickhouse-low-latency"
  })
}
