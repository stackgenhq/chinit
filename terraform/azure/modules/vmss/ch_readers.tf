# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# VMSS for Reader Nodes - Flexible Orchestration Mode
# ============================================================================
resource "azurerm_orchestrated_virtual_machine_scale_set" "readers" {
  name                        = var.names.vmss_readers
  resource_group_name         = var.resource_group_name
  location                    = var.location
  sku_name                    = var.instance_config.reader_vm_size
  instances                   = var.instance_config.reader_count
  platform_fault_domain_count = 1 # Maximize spread in Flex mode
  upgrade_mode                = "Rolling"

  zones = var.enable_availability_zones ? var.availability_zones : null

  user_data_base64 = base64encode(var.reader_cloud_init)

  os_profile {
    linux_configuration {
      admin_username = var.vm_admin_username
      admin_ssh_key {
        username   = var.vm_admin_username
        public_key = local.ssh_public_key
      }
      disable_password_authentication = true
    }
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

  data_disk {
    lun                  = 0
    caching              = "ReadWrite"
    create_option        = "Empty"
    disk_size_gb         = var.instance_config.metadata_disk_size_gb
    storage_account_type = var.instance_config.disk_storage_account_type
  }

  data_disk {
    lun                  = 1
    caching              = "ReadWrite"
    create_option        = "Empty"
    disk_size_gb         = var.instance_config.cache_disk_size_gb
    storage_account_type = var.instance_config.disk_storage_account_type
  }

  network_interface {
    name                          = "${var.names.vmss_readers}-nic"
    primary                       = true
    enable_accelerated_networking = var.enable_accelerated_networking

    ip_configuration {
      name                                   = "internal"
      primary                                = true
      subnet_id                              = azurerm_subnet.clickhouse.id
      load_balancer_backend_address_pool_ids = [azurerm_lb_backend_address_pool.readers.id]
      application_security_group_ids         = var.enable_application_security_groups ? [azurerm_application_security_group.clickhouse_nodes[0].id] : []
    }
  }

  identity {
    type         = "UserAssigned"
    identity_ids = var.managed_identity_id != null ? [var.managed_identity_id] : [azurerm_user_assigned_identity.vmss[0].id]
  }

  # Spot VM Config
  priority        = var.instance_config.enable_reader_spot_vms ? "Spot" : "Regular"
  eviction_policy = var.instance_config.enable_reader_spot_vms ? var.instance_config.reader_spot_eviction_policy : null
  max_bid_price   = var.instance_config.enable_reader_spot_vms ? var.instance_config.reader_spot_max_bid_price : null

  boot_diagnostics {
    storage_account_uri = var.boot_diagnostics_storage_account_uri
  }

  # Extensions
  extension {
    name                 = "HealthExtension"
    publisher            = "Microsoft.ManagedServices"
    type                 = "ApplicationHealthLinux"
    type_handler_version = "1.0"
    settings = jsonencode({
      protocol    = local.health_probe_protocol
      port        = local.health_probe_port
      requestPath = local.health_probe_path
    })
  }

  encryption_at_host_enabled = var.security_config.enable_encryption_at_host
  dynamic "extension" {
    for_each = var.enable_azure_monitor_agent && var.log_analytics_workspace_id != null ? [1] : []
    content {
      name                 = "AzureMonitorLinuxAgent"
      publisher            = "Microsoft.Azure.Monitor"
      type                 = "AzureMonitorLinuxAgent"
      type_handler_version = "1.0"
      settings = jsonencode({
        workspaceId = var.log_analytics_workspace_id
      })
    }
  }

  dynamic "automatic_instance_repair" {
    for_each = var.enable_health_extension && var.enable_automatic_instance_repair ? [1] : []
    content {
      enabled      = true
      grace_period = var.automatic_repair_grace_period
    }
  }



  termination_notification {
    enabled = true
    timeout = "PT5M"
  }

  # Zero-Downtime Rolling Upgrade Policy
  # Reference: BLINDSPOTS.md #3
  dynamic "rolling_upgrade_policy" {
    for_each = var.rolling_upgrade_config.enabled ? [1] : []
    content {
      max_batch_instance_percent              = var.rolling_upgrade_config.max_batch_percent
      max_unhealthy_instance_percent          = var.rolling_upgrade_config.max_unhealthy_percent
      max_unhealthy_upgraded_instance_percent = var.rolling_upgrade_config.max_unhealthy_upgraded_percent
      pause_time_between_batches              = var.rolling_upgrade_config.pause_time
      cross_zone_upgrades_enabled             = var.rolling_upgrade_config.enable_cross_zone_upgrades
      prioritize_unhealthy_instances_enabled  = var.rolling_upgrade_config.enable_prioritize_unhealthy_instances
      maximum_surge_instances_enabled         = var.rolling_upgrade_config.enable_max_surge
    }
  }

  lifecycle {
    ignore_changes = [
      instances,
      data_disk,
      os_disk,
    ]
  }

  tags = merge(var.tags, {
    "node-type"              = "reader"
    "cloud-init-script-hash" = local.reader_cloud_init_hash
  })
}
