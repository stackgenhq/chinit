# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Example Variables File for Packer
# ============================================================================
# Copy this file and customize for your environment:
#   cp example.pkrvars.hcl dev.pkrvars.hcl
#   packer build -var-file=dev.pkrvars.hcl clickhouse.pkr.hcl
# ============================================================================

subscription_id     = "your-subscription-id"
resource_group_name = "rg-clickhouse-images"
location            = "eastus"
gallery_name        = "sig_clickhouse"
image_name          = "clickhouse-node-tl"
image_version       = "1.0.0"
clickhouse_version  = "24.8.4.13"
vm_size             = "Standard_D2s_v5"

tags = {
  "environment" = "dev"
  "managed-by"  = "packer"
  "purpose"     = "golden-image"
}
