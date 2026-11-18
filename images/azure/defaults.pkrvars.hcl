# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Packer Variables - Shared Golden Image
# ============================================================================
# These defaults match the terraform/variables.tf defaults.
# subscription_id has no default: it is an account identifier, not
# something to bake into a shareable repo.

subscription_id     = "" # pass per environment: -var subscription_id=<sub id>
resource_group_name = "rg-clickhouse-images"
location            = "westus2"
gallery_name        = "sig_clickhousecluster"
image_name          = "clickhouse-node"
clickhouse_version  = "25.12.4.35"
vm_size             = "Standard_D4s_v5"

# image_version is passed via CLI: packer build -var "image_version=1.0.0"

tags = {
  "purpose"    = "golden-image"
  "managed-by" = "packer"
}
