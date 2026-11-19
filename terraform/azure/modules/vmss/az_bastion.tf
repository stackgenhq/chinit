# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Bastion Configuration
# ============================================================================
# Why: Azure Bastion provides secure SSH/RDP access to VMs without exposing
#      them to the public internet. This is the recommended approach for
#      production environments per Azure Security Baseline.
# Cost:
#   - Developer SKU: ~$0.05/hour (~$36/month if always on) - no dedicated subnet
#   - Basic SKU: ~$0.19/hour (~$140/month if always on) - requires public IP
# Without it: SSH requires either public IPs on VMs (security risk) or VPN
#             connectivity which requires additional infrastructure.
# ============================================================================

# Bastion requires a dedicated subnet named "AzureBastionSubnet" (Basic/Standard only)
# Developer SKU uses shared infrastructure and doesn't need a dedicated subnet
resource "azurerm_subnet" "bastion" {
  count                = var.network_config.enable_bastion && var.network_config.bastion_sku != "Developer" ? 1 : 0
  name                 = "AzureBastionSubnet"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = [var.network_config.bastion_subnet_address_prefix]
}

# Public IP for Bastion (required for Basic/Standard SKU only)
resource "azurerm_public_ip" "bastion" {
  count               = var.network_config.enable_bastion && var.network_config.bastion_sku != "Developer" ? 1 : 0
  name                = "${var.cluster_name}-bastion-pip"
  location            = var.location
  resource_group_name = var.resource_group_name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = var.tags
}

# Azure Bastion Host - Developer SKU
# Developer SKU uses shared infrastructure (no dedicated subnet/public IP)
resource "azurerm_bastion_host" "developer" {
  count               = var.network_config.enable_bastion && var.network_config.bastion_sku == "Developer" ? 1 : 0
  name                = "${var.cluster_name}-bastion"
  location            = var.location
  resource_group_name = var.resource_group_name
  sku                 = "Developer"
  virtual_network_id  = azurerm_virtual_network.main.id
  tags                = var.tags

  # Lifecycle: Don't recreate if only tags change
  lifecycle {
    ignore_changes = [tags]
  }
}

# Azure Bastion Host - Basic/Standard SKU
# Requires dedicated subnet and public IP
resource "azurerm_bastion_host" "main" {
  count               = var.network_config.enable_bastion && var.network_config.bastion_sku != "Developer" ? 1 : 0
  name                = "${var.cluster_name}-bastion"
  location            = var.location
  resource_group_name = var.resource_group_name
  sku                 = var.network_config.bastion_sku
  tags                = var.tags

  # Enable Native Client support (SSH via CLI) - Required for Standard SKU
  tunneling_enabled = var.network_config.bastion_sku == "Standard" ? true : false

  ip_configuration {
    name                 = "bastion-ip-config"
    subnet_id            = azurerm_subnet.bastion[0].id
    public_ip_address_id = azurerm_public_ip.bastion[0].id
  }

  # Lifecycle: Don't recreate if only tags change
  lifecycle {
    ignore_changes = [tags]
  }
}
