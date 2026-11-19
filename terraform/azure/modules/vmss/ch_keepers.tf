# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Fixed Virtual Machines for Keeper Nodes
# ============================================================================
# Why: Keeper/ZooKeeper requires strictly stable identities (IDs 0, 1, 2) and
#      quorum maintenance. VMSS Uniform/Flex can cause identity thrashing during
#      scaling or upgrades. Fixed VMs provide 100% deterministic naming and
#      zonal placement suitable for stateful consensus clusters.
# ============================================================================

resource "azurerm_network_interface" "keepers" {
  count                          = var.instance_config.keeper_count
  name                           = "${var.names.vmss_keepers}-nic-${count.index}"
  resource_group_name            = var.resource_group_name
  location                       = var.location
  accelerated_networking_enabled = var.enable_accelerated_networking

  ip_configuration {
    name      = "internal"
    subnet_id = azurerm_subnet.clickhouse.id
    # STATIC at the +20 octet offset: chinit's static identity maps the
    # private IP's LAST OCTET to the node's role name (keepers at
    # +20..+29 — the AWS parity convention). A Dynamic allocation
    # would hand the keeper whatever the subnet has free and chinit's
    # octet map Fatals ("does not map to a static role offset") — the
    # keeper never settles identity and raft never forms.
    private_ip_address_allocation = "Static"
    private_ip_address            = cidrhost(var.network_config.subnet_address_prefix, 20 + count.index)
    # ASGs handled via association resource below
  }

  tags = var.tags
}

resource "azurerm_linux_virtual_machine" "keepers" {
  count                 = var.instance_config.keeper_count
  name                  = "clickhouse-keeper-${count.index}" # Stable Name: clickhouse-keeper-0
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.instance_config.keeper_vm_size
  admin_username        = var.vm_admin_username
  network_interface_ids = [azurerm_network_interface.keepers[count.index].id]

  # Deterministic Zone Placement: 0->Zone1, 1->Zone2, 2->Zone3
  zone = element(var.availability_zones, count.index)

  admin_ssh_key {
    username   = var.vm_admin_username
    public_key = local.ssh_public_key
  }

  user_data = base64encode(var.keeper_cloud_init)

  # Security Features
  secure_boot_enabled        = var.security_config.enable_trusted_launch
  vtpm_enabled               = var.security_config.enable_trusted_launch
  encryption_at_host_enabled = var.security_config.enable_encryption_at_host

  source_image_reference {
    publisher = "Canonical"
    offer     = "0001-com-ubuntu-server-jammy"
    sku       = "22_04-lts-gen2"
    version   = "latest"
  }

  os_disk {
    name                 = "clickhouse-keeper-${count.index}-os"
    caching              = var.instance_config.enable_ephemeral_os_disk ? "ReadOnly" : "ReadWrite"
    storage_account_type = var.instance_config.enable_ephemeral_os_disk ? "Standard_LRS" : var.instance_config.disk_storage_account_type
    dynamic "diff_disk_settings" {
      for_each = var.instance_config.enable_ephemeral_os_disk ? [1] : []
      content {
        option    = "Local"
        placement = "ResourceDisk"
      }
    }
  }

  identity {
    type         = var.managed_identity_id != null ? "UserAssigned" : "SystemAssigned"
    identity_ids = var.managed_identity_id != null ? [var.managed_identity_id] : []
  }

  boot_diagnostics {
    storage_account_uri = var.boot_diagnostics_storage_account_uri
  }

  lifecycle {
    ignore_changes = [
      source_image_reference # Don't replace on minor image updates
    ]
    replace_triggered_by = [
      terraform_data.keeper_cloud_init_trigger
    ]
  }

  tags = merge(var.tags, {
    "node-type"              = "keeper"
    "cloud-init-script-hash" = local.keeper_cloud_init_hash
  })
}

# ============================================================================
# Managed Disks for Keeper Data (LUN 0: Snapshots)
# ============================================================================
resource "azurerm_managed_disk" "keeper_metadata" {
  count                = var.instance_config.keeper_count
  name                 = "clickhouse-keeper-${count.index}-metadata"
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.instance_config.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = var.instance_config.metadata_disk_size_gb
  zone                 = element(var.availability_zones, count.index)
  tags                 = var.tags
}

resource "azurerm_virtual_machine_data_disk_attachment" "keeper_metadata" {
  count              = var.instance_config.keeper_count
  managed_disk_id    = azurerm_managed_disk.keeper_metadata[count.index].id
  virtual_machine_id = azurerm_linux_virtual_machine.keepers[count.index].id
  lun                = 0
  caching            = "ReadWrite"
}

# ============================================================================
# Managed Disks for Keeper Logs (LUN 2: Transaction Logs)
# ============================================================================
resource "azurerm_managed_disk" "keeper_logs" {
  count                = var.instance_config.keeper_count
  name                 = "clickhouse-keeper-${count.index}-logs"
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.instance_config.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = var.keeper_log_disk_size_gb
  zone                 = element(var.availability_zones, count.index)
  tags                 = var.tags
}

resource "azurerm_virtual_machine_data_disk_attachment" "keeper_logs" {
  count              = var.instance_config.keeper_count
  managed_disk_id    = azurerm_managed_disk.keeper_logs[count.index].id
  virtual_machine_id = azurerm_linux_virtual_machine.keepers[count.index].id
  lun                = 2
  caching            = "None" # Important for fsync latency
}


# ============================================================================
# Network Interface ASG Association
# ============================================================================
resource "azurerm_network_interface_application_security_group_association" "keepers" {
  count                         = var.enable_application_security_groups ? var.instance_config.keeper_count : 0
  network_interface_id          = azurerm_network_interface.keepers[count.index].id
  application_security_group_id = azurerm_application_security_group.keeper_nodes[0].id
}

# ============================================================================
# Load Balancer Association for Keepers
# ============================================================================
resource "azurerm_network_interface_backend_address_pool_association" "keepers" {
  count                   = var.instance_config.keeper_count
  network_interface_id    = azurerm_network_interface.keepers[count.index].id
  ip_configuration_name   = "internal"
  backend_address_pool_id = azurerm_lb_backend_address_pool.keepers.id
}

