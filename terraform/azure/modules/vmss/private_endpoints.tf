# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Private Endpoints for Storage and Key Vault
# ============================================================================
# Why: Per Azure Security Baseline, use Private Endpoints instead of service
#      endpoints for secure access to PaaS services. Private Endpoints provide:
#      - Private IP addresses within the VNet
#      - Traffic stays on Microsoft backbone network
#      - No public internet exposure
#      - Better security posture for compliance
# Reference: BLINDSPOTS.md #14 - Private Endpoints Disabled by Default
# Without it: Storage and Key Vault traffic traverses public internet even
#             when using service endpoints (service endpoints only restrict
#             access, not traffic path).
# ============================================================================

# ============================================================================
# Private Endpoint for Storage Account (Blob)
# ============================================================================
resource "azurerm_private_endpoint" "storage_blob" {
  count               = var.security_config.enable_private_endpoints ? 1 : 0
  name                = "${var.cluster_name}-storage-pe"
  location            = var.location
  resource_group_name = var.resource_group_name
  subnet_id           = azurerm_subnet.clickhouse.id
  tags                = var.tags

  private_service_connection {
    name                           = "${var.cluster_name}-storage-psc"
    private_connection_resource_id = var.storage_account_id
    subresource_names              = ["blob"]
    is_manual_connection           = false
  }

  # Private DNS Zone integration
  dynamic "private_dns_zone_group" {
    for_each = var.security_config.storage_private_dns_zone_id != null ? [1] : []
    content {
      name                 = "storage-dns-zone-group"
      private_dns_zone_ids = [var.security_config.storage_private_dns_zone_id]
    }
  }
}

# ============================================================================
# Private Endpoint for Key Vault
# ============================================================================
resource "azurerm_private_endpoint" "key_vault" {
  count               = var.security_config.enable_private_endpoints ? 1 : 0
  name                = "${var.cluster_name}-keyvault-pe"
  location            = var.location
  resource_group_name = var.resource_group_name
  subnet_id           = azurerm_subnet.clickhouse.id
  tags                = var.tags

  private_service_connection {
    name                           = "${var.cluster_name}-keyvault-psc"
    private_connection_resource_id = var.key_vault_id
    subresource_names              = ["vault"]
    is_manual_connection           = false
  }

  # Private DNS Zone integration
  dynamic "private_dns_zone_group" {
    for_each = var.security_config.keyvault_private_dns_zone_id != null ? [1] : []
    content {
      name                 = "keyvault-dns-zone-group"
      private_dns_zone_ids = [var.security_config.keyvault_private_dns_zone_id]
    }
  }
}

# ============================================================================
# Private DNS Zones for Private Endpoints
# ============================================================================
# Why: Private DNS zones are required for Private Endpoints to resolve the
#      private IP addresses of PaaS services. Without them, DNS resolution
#      returns public IP addresses.
# ============================================================================
resource "azurerm_private_dns_zone" "storage_blob" {
  count               = var.security_config.enable_private_endpoints && var.security_config.create_private_dns_zones ? 1 : 0
  name                = "privatelink.blob.core.windows.net"
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

resource "azurerm_private_dns_zone" "keyvault" {
  count               = var.security_config.enable_private_endpoints && var.security_config.create_private_dns_zones ? 1 : 0
  name                = "privatelink.vaultcore.azure.net"
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# Private DNS Zone Virtual Network Links
# ============================================================================
# Why: Links private DNS zones to the VNet so VMs can resolve private endpoints.
# ============================================================================
resource "azurerm_private_dns_zone_virtual_network_link" "storage_blob" {
  count                 = var.security_config.enable_private_endpoints && var.security_config.create_private_dns_zones ? 1 : 0
  name                  = "${var.cluster_name}-storage-dns-link"
  resource_group_name   = var.resource_group_name
  private_dns_zone_name = azurerm_private_dns_zone.storage_blob[0].name
  virtual_network_id    = azurerm_virtual_network.main.id
  registration_enabled  = false
  tags                  = var.tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "keyvault" {
  count                 = var.security_config.enable_private_endpoints && var.security_config.create_private_dns_zones ? 1 : 0
  name                  = "${var.cluster_name}-keyvault-dns-link"
  resource_group_name   = var.resource_group_name
  private_dns_zone_name = azurerm_private_dns_zone.keyvault[0].name
  virtual_network_id    = azurerm_virtual_network.main.id
  registration_enabled  = false
  tags                  = var.tags
}
