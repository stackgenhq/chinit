# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Auto-Scaling for Reader VMSS
# ============================================================================
# Why: Automatically scales reader VMSS instances based on CPU metrics.
#      Enables cost optimization for read-heavy workloads by scaling out when
#      resources are constrained and scaling in when resources are underutilized.
# Note: Memory-based autoscaling requires Azure Monitor agent with custom metrics
#       which is more complex to set up. CPU-based autoscaling covers most use cases.
# Without it: Manual scaling required for reader nodes, leading to
#             over-provisioning or under-provisioning based on workload.
# ============================================================================
resource "azurerm_monitor_autoscale_setting" "readers" {
  count               = var.autoscaling_config.enabled ? 1 : 0
  name                = "${var.names.vmss_readers}-autoscale"
  resource_group_name = var.resource_group_name
  location            = var.location
  target_resource_id  = azurerm_orchestrated_virtual_machine_scale_set.readers.id
  enabled             = true

  profile {
    name = "default"

    capacity {
      default = var.instance_config.reader_count
      minimum = var.autoscaling_config.min_instances
      maximum = var.autoscaling_config.max_instances
    }

    # CPU-based scale out rule
    rule {
      metric_trigger {
        metric_name        = "Percentage CPU"
        metric_resource_id = azurerm_orchestrated_virtual_machine_scale_set.readers.id
        time_grain         = "PT1M"
        statistic          = "Average"
        time_window        = "PT2M"
        time_aggregation   = "Average"
        operator           = "GreaterThan"
        threshold          = var.autoscaling_config.cpu_scale_out_threshold
      }

      scale_action {
        direction = "Increase"
        type      = "ChangeCount"
        value     = 1
        cooldown  = "PT10M"
      }
    }

    # CPU-based scale in rule
    rule {
      metric_trigger {
        metric_name        = "Percentage CPU"
        metric_resource_id = azurerm_orchestrated_virtual_machine_scale_set.readers.id
        time_grain         = "PT1M"
        statistic          = "Average"
        time_window        = "PT2M"
        time_aggregation   = "Average"
        operator           = "LessThan"
        threshold          = var.autoscaling_config.cpu_scale_in_threshold
      }

      scale_action {
        direction = "Decrease"
        type      = "ChangeCount"
        value     = 1
        cooldown  = "PT10M"
      }
    }
  }

  tags = var.tags
}

