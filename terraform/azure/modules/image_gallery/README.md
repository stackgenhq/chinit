<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Azure Compute Gallery Module

This module creates an Azure Compute Gallery (formerly Shared Image Gallery) for storing pre-baked ClickHouse VM images.

## Purpose

Pre-baked golden images reduce VMSS bootstrap time from ~8-12 minutes to ~2-3 minutes by:
- Pre-installing ClickHouse server and client
- Pre-installing Azure CLI
- Pre-configuring OS optimizations (THP disabled, ulimits)

## Resources Created

| Resource | Description |
|----------|-------------|
| `azurerm_shared_image_gallery` | Container for image definitions and versions |
| `azurerm_shared_image` | Image definition for ClickHouse nodes |

## Usage

```hcl
module "image_gallery" {
  source = "./modules/image_gallery"

  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  gallery_name        = "sig_clickhouse_${var.cluster_name}"
  clickhouse_version  = var.clickhouse_version
  tags                = var.tags
}
```

## Image Versions

Image versions are created by Packer, not Terraform. See `/packer/README.md` for build instructions.

## Outputs

| Output | Description |
|--------|-------------|
| `gallery_id` | Resource ID of the gallery |
| `image_definition_id` | Resource ID for Packer builds |
| `image_reference` | Reference for VMSS consumption |
