# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Compute Gallery Module - Outputs
# ============================================================================

output "gallery_id" {
  description = "Resource ID of the Azure Compute Gallery"
  value       = azurerm_shared_image_gallery.clickhouse.id
}

output "gallery_name" {
  description = "Name of the Azure Compute Gallery"
  value       = azurerm_shared_image_gallery.clickhouse.name
}

output "image_definition_id" {
  description = "Resource ID of the ClickHouse node image definition"
  value       = azurerm_shared_image.clickhouse_node.id
}

output "image_definition_name" {
  description = "Name of the ClickHouse node image definition"
  value       = azurerm_shared_image.clickhouse_node.name
}

# Output for VMSS consumption
# Use this when a golden image version exists
output "image_reference" {
  description = "Image reference details for VMSS source_image_reference"
  value = {
    publisher = var.image_publisher
    offer     = var.image_offer
    sku       = "clickhouse-${replace(var.clickhouse_version, ".", "-")}"
  }
}
