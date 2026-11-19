# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Dedicated Backup Replica VM
# ============================================================================
# Why: A dedicated backup replica isolates backup operations from production
#      query traffic. This VM:
#      1. Receives replicated data via ClickHouse's native replication
#      2. Is NOT connected to the load balancer (excluded from query traffic)
#      3. Runs clickhouse-backup without impacting production nodes
#      4. Uses Azure B-series burstable VM for cost optimization (~$30-80/month)
# The backup replica uses special MergeTree settings to minimize resource usage:
#      - always_fetch_merged_part=true (don't merge locally)
#      - replicated_can_become_leader=false (can't become leader)
# Without it: Backups run on production nodes, consuming CPU resources and
#             potentially impacting query performance during backup windows.
# ============================================================================

# Cloud-init change detection for backup replica
locals {
  backup_replica_cloud_init_hash = var.backup_replica_config.enabled ? md5(var.backup_replica_cloud_init) : ""
}

resource "terraform_data" "backup_replica_cloud_init_trigger" {
  count = var.backup_replica_config.enabled ? 1 : 0
  triggers_replace = {
    hash = local.backup_replica_cloud_init_hash
  }
}

# Network Interface for Backup Replica
# Why: Provides network connectivity for the backup replica WITHOUT connecting
#      to any load balancer backend pools. This ensures the backup replica
#      receives replicated data but is excluded from query traffic.
# Without it: Backup replica cannot communicate with the cluster or receive data.
resource "azurerm_network_interface" "backup_replica" {
  count               = var.backup_replica_config.enabled ? 1 : 0
  name                = var.backup_replica_config.names.nic
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = merge(var.tags, { "node-type" = "backup" })

  # NOTE: B-series burstable VMs (Standard_B4ms etc) do NOT support accelerated networking!
  # We must disable it for the backup replica since it uses a cost-optimized B-series VM.
  # See: https://learn.microsoft.com/en-us/azure/virtual-network/accelerated-networking-overview
  accelerated_networking_enabled = false
  ip_configuration {
    name      = "internal"
    subnet_id = azurerm_subnet.clickhouse.id
    # STATIC at the +30 octet offset: chinit's static identity maps the
    # backup replica to private_ip .30 (the AWS parity convention —
    # backup-0). A Dynamic allocation would hand the node an unmapped
    # octet and chinit Fatals ("does not map to a static role offset").
    private_ip_address_allocation = "Static"
    private_ip_address            = cidrhost(var.network_config.subnet_address_prefix, 30)
    # NOTE: No public IP and NOT connected to load balancer backend pool
    # This ensures backup replica is excluded from query traffic
  }
}


# Backup Replica VM (Azure B-series Burstable)
# Why: Azure B-series burstable VMs are cost-effective for the backup replica because:
#      1. Low baseline CPU usage (mostly idle, just receiving replicated data)
#      2. Accumulates CPU credits when idle
#      3. Bursts during backup operations (compression, upload)
#      4. Costs ~$30-80/month vs $200+ for regular compute VMs
# Without it: No dedicated backup infrastructure, backups impact production.
resource "azurerm_linux_virtual_machine" "backup_replica" {
  count               = var.backup_replica_config.enabled ? 1 : 0
  name                = var.backup_replica_config.names.vm
  resource_group_name = var.resource_group_name
  location            = var.location
  size                = var.backup_replica_config.vm_size
  admin_username      = var.vm_admin_username
  tags = merge(var.tags, {
    "node-type"              = "backup"
    "purpose"                = "dedicated-backup-replica"
    "cloud-init-script-hash" = local.backup_replica_cloud_init_hash
  })

  # SECURITY: Host-based encryption is recommended per Azure Security Baseline.
  # Encrypts OS disk, data disks, and temp disks at the host level.
  # Note: Requires Microsoft.Compute/EncryptionAtHost feature registered in subscription.
  #       Register with: az feature register --namespace Microsoft.Compute --name EncryptionAtHost
  encryption_at_host_enabled = var.security_config.enable_encryption_at_host

  # SECURITY: Trusted Launch - Azure Security Baseline requirement
  # Provides defense against boot kits, rootkits, and kernel-level malware.
  # Includes Secure Boot, vTPM, and integrity monitoring.
  # Note: Requires Gen2 VM image (22_04-lts-gen2 is Gen2).
  secure_boot_enabled = var.security_config.enable_trusted_launch
  vtpm_enabled        = var.security_config.enable_trusted_launch

  admin_ssh_key {
    username   = var.vm_admin_username
    public_key = local.ssh_public_key
  }

  # Image source: Use golden image if available, otherwise marketplace image
  # Golden images have ClickHouse pre-installed, reducing bootstrap from ~10 min to ~2 min
  source_image_id = var.golden_image_id

  dynamic "source_image_reference" {
    for_each = var.golden_image_id != null ? [] : [1]
    content {
      publisher = "Canonical"
      offer     = "0001-com-ubuntu-server-jammy"
      sku       = "22_04-lts-gen2"
      version   = "latest"
    }
  }

  os_disk {
    name                 = var.backup_replica_config.names.os_disk
    caching              = var.instance_config.enable_ephemeral_os_disk ? "ReadOnly" : "ReadWrite"
    storage_account_type = var.instance_config.enable_ephemeral_os_disk ? "Standard_LRS" : var.instance_config.disk_storage_account_type

    # FINOPS OPTIMIZATION: Ephemeral OS Disk
    dynamic "diff_disk_settings" {
      for_each = var.instance_config.enable_ephemeral_os_disk ? [1] : []
      content {
        option = "Local"
      }
    }
  }

  network_interface_ids = [azurerm_network_interface.backup_replica[0].id]

  # Bootstrap script for ClickHouse installation and backup configuration
  user_data = base64encode(var.backup_replica_cloud_init)

  # Managed identity for Azure resource access (Key Vault, Storage)
  identity {
    type         = var.managed_identity_id != null ? "UserAssigned" : "SystemAssigned"
    identity_ids = var.managed_identity_id != null ? [var.managed_identity_id] : []
  }

  lifecycle {
    # NOTE: Do NOT use create_before_destroy for single VMs with fixed names.
    # Azure cannot have two VMs with the same name, causing state drift when
    # the new VM is created but state update fails before old VM deletion.
    replace_triggered_by = [
      terraform_data.backup_replica_cloud_init_trigger[0]
    ]
  }
}

# Metadata Disk for Backup Replica
# Why: Provides persistent storage for ClickHouse metadata. Smaller than production
#      nodes since backup replica doesn't need as much metadata storage.
# Without it: ClickHouse metadata is lost on VM restarts.
resource "azurerm_managed_disk" "backup_replica_metadata" {
  count                = var.backup_replica_config.enabled ? 1 : 0
  name                 = var.backup_replica_config.names.meta_disk
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.instance_config.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = var.backup_replica_config.metadata_disk_size_gb
  tags                 = merge(var.tags, { "node-type" = "backup", "disk-type" = "metadata" })

  lifecycle {
    ignore_changes = [disk_size_gb]
  }
}

# Cache Disk for Backup Replica
# Why: Provides local cache storage. Smaller than production nodes since backup
#      replica doesn't serve queries and doesn't need large cache.
# Without it: All data access goes directly to blob storage.
resource "azurerm_managed_disk" "backup_replica_cache" {
  count                = var.backup_replica_config.enabled ? 1 : 0
  name                 = var.backup_replica_config.names.cache_disk
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.instance_config.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = var.backup_replica_config.cache_disk_size_gb
  tags                 = merge(var.tags, { "node-type" = "backup", "disk-type" = "cache" })

  lifecycle {
    ignore_changes = [disk_size_gb]
  }
}

# Attach Metadata Disk to Backup Replica
resource "azurerm_virtual_machine_data_disk_attachment" "backup_replica_metadata" {
  count              = var.backup_replica_config.enabled ? 1 : 0
  managed_disk_id    = azurerm_managed_disk.backup_replica_metadata[0].id
  virtual_machine_id = azurerm_linux_virtual_machine.backup_replica[0].id
  lun                = "0"
  caching            = "ReadWrite"

  timeouts {
    create = "30m"
    update = "30m"
    delete = "20m"
  }
}

# Attach Cache Disk to Backup Replica
resource "azurerm_virtual_machine_data_disk_attachment" "backup_replica_cache" {
  count              = var.backup_replica_config.enabled ? 1 : 0
  managed_disk_id    = azurerm_managed_disk.backup_replica_cache[0].id
  virtual_machine_id = azurerm_linux_virtual_machine.backup_replica[0].id
  lun                = "1"
  caching            = "ReadWrite"

  timeouts {
    create = "30m"
    update = "30m"
    delete = "20m"
  }
}

# Grant Storage Blob Data Contributor role to Backup Replica's system-assigned identity
# Only applies when using system-assigned identity (managed_identity_id is null)
# This role provides read AND write access for backup operations
resource "azurerm_role_assignment" "backup_replica_storage_blob_contributor" {
  count                            = var.backup_replica_config.enabled && var.storage_account_id != null && var.managed_identity_id == null ? 1 : 0
  scope                            = var.storage_account_id
  role_definition_name             = "Storage Blob Data Contributor"
  principal_id                     = azurerm_linux_virtual_machine.backup_replica[0].identity[0].principal_id
  skip_service_principal_aad_check = true
}

# Grant Key Vault Secrets User role to Backup Replica's system-assigned identity
# Why: Allows the backup replica to read ClickHouse passwords from Key Vault.
#      Without this, the backup replica would use different (generated) passwords
#      than production nodes, causing authentication failures.
# Without it: Backup replica can't retrieve passwords, cloud-init falls back to
#             generated passwords, causing authentication mismatch with production.
resource "azurerm_role_assignment" "backup_replica_key_vault_secrets_user" {
  count                            = var.backup_replica_config.enabled && var.key_vault_id != null && var.managed_identity_id == null ? 1 : 0
  scope                            = var.key_vault_id
  role_definition_name             = "Key Vault Secrets User"
  principal_id                     = azurerm_linux_virtual_machine.backup_replica[0].identity[0].principal_id
  skip_service_principal_aad_check = true
}

# Grant Private DNS Zone Contributor role to Backup Replica's system-assigned identity
# Why: Allows the backup replica to register its DNS hostname for cluster communication.
# Without it: Backup replica cannot register its DNS record, preventing cluster formation.
resource "azurerm_role_assignment" "backup_replica_dns_zone_contributor" {
  count                            = var.backup_replica_config.enabled && var.dns_zone_id != null && var.managed_identity_id == null ? 1 : 0
  scope                            = var.dns_zone_id
  role_definition_name             = "Private DNS Zone Contributor"
  principal_id                     = azurerm_linux_virtual_machine.backup_replica[0].identity[0].principal_id
  skip_service_principal_aad_check = true
}
