# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# chinit cluster module (Azure) — one self-contained root module
# ============================================================================
# The Azure mirror of ../aws: private DNS, the node-claims Table, Key
# Vault secrets, the storage account (bundle + data + backups), ONE
# user-assigned managed identity with least-privilege RBAC (the IAM-role
# equivalent), the spec/bundle uploads, and the four node roles.
# ============================================================================

# ----------------------------------------------------------------------------
# Network (optional — bring-your-own via vnet_id/subnet_id)
# ----------------------------------------------------------------------------
resource "azurerm_virtual_network" "this" {
  count = var.create_network ? 1 : 0

  name                = "${local.name_prefix}-vnet"
  location            = var.location
  resource_group_name = var.resource_group_name
  address_space       = [var.subnet_cidr]

  tags = local.common_tags
}

resource "azurerm_subnet" "nodes" {
  count = var.create_network ? 1 : 0

  name                 = "${local.name_prefix}-nodes"
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.this[0].name
  address_prefixes     = [var.subnet_cidr]
}

# ----------------------------------------------------------------------------
# Private DNS zone — node discovery (VNet-linked, auto-registration OFF:
# chinit owns every A record through its ARM REST registrar)
# ----------------------------------------------------------------------------
resource "azurerm_private_dns_zone" "nodes" {
  name                = var.dns_zone_name
  resource_group_name = var.resource_group_name

  tags = local.common_tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "nodes" {
  name                  = "${local.name_prefix}-link"
  resource_group_name   = var.resource_group_name
  private_dns_zone_name = azurerm_private_dns_zone.nodes.name
  virtual_network_id    = local.vnet_id
  registration_enabled  = false

  tags = local.common_tags
}

# ----------------------------------------------------------------------------
# Storage account: the config bundle container, the data container (the
# ClickHouse cold tier), the backup container, and the node-claims Table
# ----------------------------------------------------------------------------
resource "azurerm_storage_account" "this" {
  # Storage account names: 3-24 chars, globally unique, lowercase
  # alphanumeric only — derived from the cluster name (validated short
  # enough that the suffix fits).
  name                     = substr(replace("${var.cluster_name}ch", "-", ""), 0, 24)
  resource_group_name      = var.resource_group_name
  location                 = var.location
  account_tier             = "Standard"
  account_replication_type = "LRS"
  account_kind             = "StorageV2"

  # No account keys on the wire: the nodes authenticate with the managed
  # identity (Blob RBAC + Table RBAC), and ClickHouse's disk uses the
  # ManagedIdentityCredential fallback. Allow the key fallback for the
  # console, or set to false for strict keyless operation.
  allow_nested_items_to_be_public = false
  public_network_access_enabled   = true # set false + private endpoints in your own wiring

  tags = local.common_tags
}

resource "azurerm_storage_container" "scripts" {
  name                  = "scripts"
  storage_account_id    = azurerm_storage_account.this.id
  container_access_type = "private"
}

resource "azurerm_storage_container" "data" {
  name                  = "data"
  storage_account_id    = azurerm_storage_account.this.id
  container_access_type = "private"
}

resource "azurerm_storage_container" "backup" {
  name                  = "backups"
  storage_account_id    = azurerm_storage_account.this.id
  container_access_type = "private"
}

# The ordinal-lease Table (chinit's lease identity: PartitionKey
# "readers", RowKey the hostname, lease_until the TTL epoch — the same
# contract the DynamoDB table serves on AWS).
resource "azurerm_storage_table" "node_claims" {
  name               = "${var.cluster_name}-node-claims"
  storage_account_id = azurerm_storage_account.this.id
}

# ----------------------------------------------------------------------------
# Key Vault + password secrets
# ----------------------------------------------------------------------------
resource "azurerm_key_vault" "this" {
  # Key Vault names: 3-24 chars, globally unique, [a-zA-Z0-9-].
  name                       = substr("${replace(var.cluster_name, "-", "")}kv", 0, 24)
  location                   = var.location
  resource_group_name        = var.resource_group_name
  tenant_id                  = data.azurerm_client_config.current.tenant_id
  sku_name                   = "standard"
  purge_protection_enabled   = false
  soft_delete_retention_days = 7

  # RBAC authorization: the managed identity reads secrets via its role
  # assignment below — no access policies to drift.
  rbac_authorization_enabled = true

  tags = local.common_tags
}

data "azurerm_client_config" "current" {}

resource "random_password" "default_password" {
  length  = 24
  special = false
}

resource "random_password" "admin_password" {
  length  = 24
  special = false
}

resource "random_password" "writer_password" {
  length  = 24
  special = false
}

# Secret versions are content-addressed by the random password id: a
# re-run with no change is a no-op; a regenerate rotates the value
# (nodes resolve the CURRENT version at boot).
resource "azurerm_key_vault_secret" "default_password" {
  name         = "${var.cluster_name}-clickhouse-default-password"
  value        = random_password.default_password.result
  key_vault_id = azurerm_key_vault.this.id

  tags = local.common_tags
}

resource "azurerm_key_vault_secret" "admin_password" {
  name         = "${var.cluster_name}-clickhouse-admin-password"
  value        = random_password.admin_password.result
  key_vault_id = azurerm_key_vault.this.id

  tags = local.common_tags
}

resource "azurerm_key_vault_secret" "writer_password" {
  name         = "${var.cluster_name}-clickhouse-writer-password"
  value        = random_password.writer_password.result
  key_vault_id = azurerm_key_vault.this.id

  tags = local.common_tags
}

# ----------------------------------------------------------------------------
# Managed identity — the instance-role equivalent (PLOP: every grant
# below names its essential scope)
# ----------------------------------------------------------------------------
resource "azurerm_user_assigned_identity" "nodes" {
  name                = "${local.name_prefix}-nodes-identity"
  location            = var.location
  resource_group_name = var.resource_group_name

  tags = local.common_tags
}

# Key Vault reads (the spec's secret NAMES resolve over this).
resource "azurerm_role_assignment" "keyvault_secrets_user" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.nodes.principal_id
}

# Blob data plane: the bundle fetch (scripts container), ClickHouse's
# cold-tier disk (data container), and clickhouse-backup (backups
# container). One account-scoped grant covers all three; container
# scoping would need three assignments (SAS at the container level is
# the finer alternative if you want it tighter).
resource "azurerm_role_assignment" "storage_blob_contributor" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.nodes.principal_id
}

# Table data plane: the ordinal-lease claim/renew/release + the
# live-claims query (chinit's TableClaimer/TableClaimLister REST calls).
resource "azurerm_role_assignment" "storage_table_contributor" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Table Data Contributor"
  principal_id         = azurerm_user_assigned_identity.nodes.principal_id
}

# Private DNS writes: chinit upserts/deletes every node's A record via
# the ARM REST surface. Scoped to the zone only.
resource "azurerm_role_assignment" "private_dns_contributor" {
  scope                = azurerm_private_dns_zone.nodes.id
  role_definition_name = "Private DNS Zone Contributor"
  principal_id         = azurerm_user_assigned_identity.nodes.principal_id
}

# ----------------------------------------------------------------------------
# NSGs — client ports to var.client_cidrs, interserver/keeper between
# the fleet (the SG-equivalent; the VMSS/VM NICs reference these)
# ----------------------------------------------------------------------------
resource "azurerm_network_security_group" "nodes" {
  name                = "${local.name_prefix}-nsg"
  location            = var.location
  resource_group_name = var.resource_group_name

  tags = local.common_tags
}

# Client ports (writers + readers). Rendered ONLY when the list is
# non-empty: an empty list means "no client ingress yet" — never an
# open rule.
resource "azurerm_network_security_rule" "client" {
  for_each = length(var.client_cidrs) > 0 ? { for p in [8123, 8443, 9000, 9440] : tostring(p) => p } : {}

  name                        = "client-${each.value}"
  priority                    = 200 + each.value - 8000
  direction                   = "Inbound"
  access                      = "Allow"
  protocol                    = "Tcp"
  source_port_range           = "*"
  destination_port_range      = each.value
  source_address_prefixes     = var.client_cidrs
  destination_address_prefix  = var.subnet_cidr
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.nodes.name
}

# Interserver TLS (9010): the fleet talks to itself inside the subnet.
resource "azurerm_network_security_rule" "interserver" {
  name                        = "interserver-9010"
  priority                    = 300
  direction                   = "Inbound"
  access                      = "Allow"
  protocol                    = "Tcp"
  source_port_range           = "*"
  destination_port_range      = "9010"
  source_address_prefix       = var.subnet_cidr
  destination_address_prefix  = var.subnet_cidr
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.nodes.name
}

# Keeper: coordination 9181 (from the fleet) + Raft 9234 (keepers).
resource "azurerm_network_security_rule" "keeper_client" {
  name                        = "keeper-9181"
  priority                    = 310
  direction                   = "Inbound"
  access                      = "Allow"
  protocol                    = "Tcp"
  source_port_range           = "*"
  destination_port_range      = "9181"
  source_address_prefix       = var.subnet_cidr
  destination_address_prefix  = var.subnet_cidr
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.nodes.name
}

resource "azurerm_network_security_rule" "keeper_raft" {
  name                        = "keeper-raft-9234"
  priority                    = 311
  direction                   = "Inbound"
  access                      = "Allow"
  protocol                    = "Tcp"
  source_port_range           = "*"
  destination_port_range      = "9234"
  source_address_prefix       = var.subnet_cidr
  destination_address_prefix  = var.subnet_cidr
  resource_group_name         = var.resource_group_name
  network_security_group_name = azurerm_network_security_group.nodes.name
}

resource "azurerm_subnet_network_security_group_association" "nodes" {
  count = var.create_network ? 1 : 0

  subnet_id                 = local.subnet_id
  network_security_group_id = azurerm_network_security_group.nodes.id
}
