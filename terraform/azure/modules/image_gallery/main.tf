# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Compute Gallery Module - Main Configuration
# ============================================================================
# Why: Creates an Azure Compute Gallery (formerly Shared Image Gallery) to store
#      pre-baked ClickHouse VM images. These golden images have ClickHouse, Azure
#      CLI, and OS optimizations pre-installed, reducing VMSS bootstrap time from
#      ~8-12 minutes to ~2-3 minutes.
# Without it: VMSS instances must install ClickHouse, Azure CLI, and configure
#             OS optimizations on every boot, increasing startup time significantly.
# ============================================================================

# ============================================================================
# Azure Compute Gallery
# ============================================================================
# Why: The gallery is a container for image definitions and versions. It enables
#      versioned image management, regional replication, and role-based access
#      control for VM images.
# ============================================================================
resource "azurerm_shared_image_gallery" "clickhouse" {
  name                = var.gallery_name
  resource_group_name = var.resource_group_name
  location            = var.location
  description         = "Azure Compute Gallery for pre-baked ClickHouse VM images"

  tags = var.tags
}

# ============================================================================
# Image Definition for ClickHouse Nodes
# ============================================================================
# Why: Defines the characteristics of VM images for ClickHouse nodes. All node
#      types (writers, readers, keepers) use the same base image since the
#      difference is only in runtime configuration, not installed software.
# Key settings:
#   - os_type: Linux (Ubuntu 22.04 LTS)
#   - hyper_v_generation: V2 (required for Gen2 VMs, Trusted Launch, ephemeral disks)
#   - architecture: x64
# ============================================================================
resource "azurerm_shared_image" "clickhouse_node" {
  name                = "clickhouse-node-tl"
  gallery_name        = azurerm_shared_image_gallery.clickhouse.name
  resource_group_name = var.resource_group_name
  location            = var.location
  os_type             = "Linux"
  hyper_v_generation  = "V2"
  architecture        = "x64"
  description         = "Pre-baked Ubuntu 22.04 LTS with ClickHouse ${var.clickhouse_version} pre-installed"

  identifier {
    publisher = var.image_publisher
    offer     = var.image_offer
    sku       = "clickhouse-${replace(var.clickhouse_version, ".", "-")}"
  }

  # Recommended machine configuration for image consumers
  # This helps users select appropriate VM sizes
  dynamic "purchase_plan" {
    for_each = var.purchase_plan != null ? [var.purchase_plan] : []
    content {
      name      = purchase_plan.value.name
      publisher = purchase_plan.value.publisher
      product   = purchase_plan.value.product
    }
  }

  tags = var.tags
}

# ============================================================================
# Optional: Image Version Placeholder
# ============================================================================
# Why: The actual image versions are created by Packer, not Terraform.
#      This data source can be used to reference the latest version.
# Note: Uncomment when image versions exist
# ============================================================================
# data "azurerm_shared_image_version" "latest" {
#   count               = var.get_latest_version ? 1 : 0
#   name                = "latest"
#   image_name          = azurerm_shared_image.clickhouse_node.name
#   gallery_name        = azurerm_shared_image_gallery.clickhouse.name
#   resource_group_name = var.resource_group_name
# }
