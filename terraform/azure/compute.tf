# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Compute — the four node roles (Azure)
# ============================================================================
# writers/readers: lease-identity VMSS members (Flexible orchestration;
# Azure assigns dynamic IPs, so the Table claim IS the identity).
# keepers/backup: fixed VMs with STATIC IPs at the +20/+30 octet offsets
# (chinit's StaticHostname maps the last octet — a dynamic allocation
# Fatals the identity settle and raft never forms).
# ============================================================================

# ----------------------------------------------------------------------------
# Writers VMSS
# ----------------------------------------------------------------------------
resource "azurerm_orchestrated_virtual_machine_scale_set" "writers" {
  name                        = "${local.name_prefix}-writers"
  resource_group_name         = var.resource_group_name
  location                    = var.location
  sku_name                    = var.vm_size
  instances                   = var.writer_desired
  platform_fault_domain_count = 1
  upgrade_mode                = "Rolling"

  user_data_base64 = base64encode(module.chinit.bootstrap_script["writer"])

  os_profile {
    linux_configuration {
      admin_username = var.admin_username
      admin_ssh_key {
        username   = var.admin_username
        public_key = local.admin_ssh_public_key
      }
      disable_password_authentication = true
    }
  }

  source_image_id = var.golden_image_id

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.disk_storage_account_type
    disk_size_gb         = var.os_disk_size_gb
  }

  # LUN 0 = metadata (ch-metadata), LUN 1 = cache (ch-cache) — chinit's
  # disk candidates resolve by LUN on first boot, by LABEL after.
  data_disk {
    lun                  = 0
    caching              = "ReadWrite"
    create_option        = "Empty"
    disk_size_gb         = var.metadata_disk_size_gb
    storage_account_type = var.disk_storage_account_type
  }

  data_disk {
    lun                  = 1
    caching              = "ReadWrite"
    create_option        = "Empty"
    disk_size_gb         = var.cache_disk_size_gb
    storage_account_type = var.disk_storage_account_type
  }

  network_interface {
    name    = "${local.name_prefix}-writers-nic"
    primary = true

    ip_configuration {
      name      = "internal"
      primary   = true
      subnet_id = local.subnet_id
    }
  }

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.nodes.id]
  }

  # Scheduled events (spot/eviction notices + planned maintenance): the
  # source chinit's scheduled-events watcher drains on.
  termination_notification {
    enabled = true
    timeout = "PT5M"
  }

  rolling_upgrade_policy {
    max_batch_instance_percent              = 20
    max_unhealthy_instance_percent          = 20
    max_unhealthy_upgraded_instance_percent = 20
    pause_time_between_batches              = "PT5M"
  }

  tags = merge(local.common_tags, { node_type = "writer" })

  depends_on = [module.chinit]
}

# ----------------------------------------------------------------------------
# Readers VMSS — Spot (Standard Spot with Delete eviction: chinit's
# watcher drains on Preempt; the lease TTL releases the ordinal when the
# drain cannot run)
# ----------------------------------------------------------------------------
resource "azurerm_orchestrated_virtual_machine_scale_set" "readers" {
  name                        = "${local.name_prefix}-readers"
  resource_group_name         = var.resource_group_name
  location                    = var.location
  sku_name                    = var.vm_size
  instances                   = var.reader_desired
  platform_fault_domain_count = 1
  upgrade_mode                = "Rolling"

  # Spot: eviction Delete (chinit's scheduled-events watcher drains on
  # the Preempt notice; the lease TTL releases the ordinal when the
  # drain cannot run). Readers are stateless — S3-tiered data +
  # replication is the durability — so a reclaim costs a lease claim
  # and a re-sync, not data.
  priority        = var.reader_spot ? "Spot" : "Regular"
  eviction_policy = var.reader_spot ? "Delete" : null

  user_data_base64 = base64encode(module.chinit.bootstrap_script["reader"])

  os_profile {
    linux_configuration {
      admin_username = var.admin_username
      admin_ssh_key {
        username   = var.admin_username
        public_key = local.admin_ssh_public_key
      }
      disable_password_authentication = true
    }
  }

  source_image_id = var.golden_image_id

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.disk_storage_account_type
    disk_size_gb         = var.os_disk_size_gb
  }

  data_disk {
    lun                  = 0
    caching              = "ReadWrite"
    create_option        = "Empty"
    disk_size_gb         = var.metadata_disk_size_gb
    storage_account_type = var.disk_storage_account_type
  }

  data_disk {
    lun                  = 1
    caching              = "ReadWrite"
    create_option        = "Empty"
    disk_size_gb         = var.cache_disk_size_gb
    storage_account_type = var.disk_storage_account_type
  }

  network_interface {
    name    = "${local.name_prefix}-readers-nic"
    primary = true

    ip_configuration {
      name      = "internal"
      primary   = true
      subnet_id = local.subnet_id
    }
  }

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.nodes.id]
  }

  termination_notification {
    enabled = true
    timeout = "PT5M"
  }

  rolling_upgrade_policy {
    max_batch_instance_percent              = 20
    max_unhealthy_instance_percent          = 50
    max_unhealthy_upgraded_instance_percent = 50
    pause_time_between_batches              = "PT2M"
  }

  tags = merge(local.common_tags, { node_type = "reader" })

  depends_on = [module.chinit]
}

# ----------------------------------------------------------------------------
# Keepers — fixed VMs, STATIC private IPs at +20+i
# ----------------------------------------------------------------------------
resource "azurerm_network_interface" "keeper" {
  count = var.keeper_count

  name                = "${local.name_prefix}-keeper-${count.index}-nic"
  location            = var.location
  resource_group_name = var.resource_group_name

  ip_configuration {
    name      = "internal"
    subnet_id = local.subnet_id
    # STATIC at the +20 offset: chinit's octet map derives keeper-N
    # from the last octet; dynamic allocation Fatals the settle.
    private_ip_address_allocation = "Static"
    private_ip_address            = local.keeper_ips[count.index]
  }

  tags = local.common_tags
}

resource "azurerm_linux_virtual_machine" "keeper" {
  count = var.keeper_count

  name                  = "clickhouse-keeper-${count.index}" # stable: chinit's ARM scope + hostname derive it
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.keeper_vm_size
  admin_username        = var.admin_username
  network_interface_ids = [azurerm_network_interface.keeper[count.index].id]

  admin_ssh_key {
    username   = var.admin_username
    public_key = local.admin_ssh_public_key
  }

  user_data = base64encode(module.chinit.bootstrap_script["keeper"])

  source_image_id = var.golden_image_id

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.disk_storage_account_type
    disk_size_gb         = var.os_disk_size_gb
  }

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.nodes.id]
  }

  tags = merge(local.common_tags, {
    node_type = "keeper"
    Name      = "${local.name_prefix}-keeper-${count.index}"
  })

  depends_on = [module.chinit]
}

# Keeper metadata disk (LUN 0, label ch-metadata): chinit's mount step
# requires a metadata volume on EVERY role (the Critical mount at
# /var/lib/clickhouse) — keepers get a small one; /var/lib/clickhouse
# stays empty (keepers never run clickhouse-server), but the mount must
# exist or the boot Fatals.
resource "azurerm_managed_disk" "keeper_metadata" {
  count = var.keeper_count

  name                 = "clickhouse-keeper-${count.index}-metadata"
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = min(var.metadata_disk_size_gb, 32) # keepers need little

  tags = local.common_tags
}

resource "azurerm_virtual_machine_data_disk_attachment" "keeper_metadata" {
  count = var.keeper_count

  managed_disk_id    = azurerm_managed_disk.keeper_metadata[count.index].id
  virtual_machine_id = azurerm_linux_virtual_machine.keeper[count.index].id
  lun                = 0
  caching            = "ReadWrite"
}

# Keeper raft disk (LUN 2, label ch-keeper): standalone managed disk so
# the log history + server id survive VM replacement.
resource "azurerm_managed_disk" "keeper_raft" {
  count = var.keeper_count

  name                 = "clickhouse-keeper-${count.index}-raft"
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = var.keeper_disk_size_gb

  tags = local.common_tags
}

resource "azurerm_virtual_machine_data_disk_attachment" "keeper_raft" {
  count = var.keeper_count

  managed_disk_id    = azurerm_managed_disk.keeper_raft[count.index].id
  virtual_machine_id = azurerm_linux_virtual_machine.keeper[count.index].id
  lun                = 2
  caching            = "None" # fsync latency
}

# ----------------------------------------------------------------------------
# Backup replica — fixed VM at the +30 offset
# ----------------------------------------------------------------------------
resource "azurerm_network_interface" "backup" {
  count = var.enable_backup ? 1 : 0

  name                = "${local.name_prefix}-backup-nic"
  location            = var.location
  resource_group_name = var.resource_group_name

  ip_configuration {
    name                          = "internal"
    subnet_id                     = local.subnet_id
    private_ip_address_allocation = "Static"
    private_ip_address            = local.backup_ip
  }

  tags = local.common_tags
}

resource "azurerm_linux_virtual_machine" "backup" {
  count = var.enable_backup ? 1 : 0

  name                  = "${local.name_prefix}-backup-0"
  resource_group_name   = var.resource_group_name
  location              = var.location
  size                  = var.vm_size
  admin_username        = var.admin_username
  network_interface_ids = [azurerm_network_interface.backup[0].id]

  admin_ssh_key {
    username   = var.admin_username
    public_key = local.admin_ssh_public_key
  }

  user_data = base64encode(module.chinit.bootstrap_script["backup"])

  source_image_id = var.golden_image_id

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = var.disk_storage_account_type
    disk_size_gb         = var.os_disk_size_gb
  }

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.nodes.id]
  }

  tags = merge(local.common_tags, {
    node_type = "backup"
    Name      = "${local.name_prefix}-backup-0"
  })

  depends_on = [module.chinit]
}

# Backup metadata disk (LUN 0, label ch-metadata): standalone attach —
# azurerm_linux_virtual_machine takes no inline data disks.
resource "azurerm_managed_disk" "backup_metadata" {
  count = var.enable_backup ? 1 : 0

  name                 = "${local.name_prefix}-backup-metadata"
  location             = var.location
  resource_group_name  = var.resource_group_name
  storage_account_type = var.disk_storage_account_type
  create_option        = "Empty"
  disk_size_gb         = var.metadata_disk_size_gb

  tags = local.common_tags
}

resource "azurerm_virtual_machine_data_disk_attachment" "backup_metadata" {
  count = var.enable_backup ? 1 : 0

  managed_disk_id    = azurerm_managed_disk.backup_metadata[0].id
  virtual_machine_id = azurerm_linux_virtual_machine.backup[0].id
  lun                = 0
  caching            = "ReadWrite"
}

# ----------------------------------------------------------------------------
# SSH key for the admin user (module-generated when var.ssh_public_key
# is unset; the private key never lands in state — only the public half
# does, and the private key is in the example's local file)
# ----------------------------------------------------------------------------
resource "tls_private_key" "node_access" {
  count = var.ssh_public_key == null ? 1 : 0

  algorithm = "ED25519"
}
