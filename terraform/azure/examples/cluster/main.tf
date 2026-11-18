# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Example — a minimal chinit cluster on Azure
#
# Prereqs:
#  - a golden Compute Gallery image built from images/azure (packer
#    build …); pass its resource id as golden_image_id
#  - provider credentials (ARM_* env vars or the provider block's own
#    auth configuration, however you normally do it)

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 3.117, < 5.0"
    }
  }
}

provider "azurerm" {
  features {}
}

resource "azurerm_resource_group" "this" {
  name     = "chinit-demo-rg"
  location = "westus2"
}

module "clickhouse" {
  source = "../.."

  cluster_name = "chinitdemo"
  location     = "westus2"

  resource_group_name = azurerm_resource_group.this.name
  subscription_id     = "00000000-0000-0000-0000-000000000000" # your subscription id

  golden_image_id = "/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.Compute/galleries/<gallery>/images/clickhouse-node/versions/1.0.0"

  # Identity ceilings (VMSS scale ceilings — never live counts)
  writer_max     = 2
  writer_desired = 2
  reader_max     = 6
  reader_desired = 2
  keeper_count   = 3
  enable_backup  = true

  create_network = true
  subnet_cidr    = "10.42.0.0/24"
  client_cidrs   = ["10.42.0.0/24"] # your clients' CIDRs

  dns_zone_name = "clickhouse.internal"
}

output "writer_0" {
  value = "writer-0.${module.clickhouse.dns_zone_name}"
}

output "admin_password_secret" {
  value = module.clickhouse.secrets.admin_password # fetch with az keyvault secret show
}
