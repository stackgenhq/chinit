# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Compute Gallery Module - Variables
# ============================================================================

variable "resource_group_name" {
  description = "Name of the resource group where the gallery will be created"
  type        = string
}

variable "location" {
  description = "Azure region for the gallery"
  type        = string
}

variable "gallery_name" {
  description = "Name of the Azure Compute Gallery (must be globally unique within subscription)"
  type        = string
}

variable "clickhouse_version" {
  description = "ClickHouse version for image tagging and SKU naming"
  type        = string
}

variable "image_publisher" {
  description = "Publisher name for the image definition (organization identifier)"
  type        = string
  default     = "appcd"
}

variable "image_offer" {
  description = "Offer name for the image definition"
  type        = string
  default     = "clickhouse-optimized"
}

variable "purchase_plan" {
  description = "Optional purchase plan for marketplace images (usually not needed for custom images)"
  type = object({
    name      = string
    publisher = string
    product   = string
  })
  default = null
}

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}
