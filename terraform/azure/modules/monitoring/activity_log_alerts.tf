# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Activity Log Alert: VM Deleted
# ============================================================================
# Why: Alerts when a VM is deleted (terminated). This is critical for tracking
#      unexpected VM terminations that could indicate issues or security events.
# Without it: VM deletions go unnoticed, potentially leading to data loss or
#             undetected security incidents.
# ============================================================================
resource "azurerm_monitor_activity_log_alert" "vm_deleted" {
  count = 0
  # count               = var.enable_vm_lifecycle_alerts && length(var.vm_resource_ids) > 0 ? 1 : 0
  name                = "${var.names.action_group}-vm-deleted"
  resource_group_name = var.resource_group_name
  location            = "global"
  scopes              = [var.resource_group_id]
  description         = "Alert when a VM is deleted/terminated"
  tags                = var.tags

  criteria {
    category       = "Administrative"
    operation_name = "Microsoft.Compute/virtualMachines/delete"
  }

  action {
    action_group_id = azurerm_monitor_action_group.vm_lifecycle[0].id
  }
}

# ============================================================================
# REMOVED: VM Restarted, VM Deallocated, VM Power Off alerts
# ============================================================================
# Why removed: These events are expected during normal maintenance, OS updates,
#              and cost optimization. For a database cluster, they create alert
#              fatigue without indicating critical issues. Deletion and resource
#              health alerts are more important for data integrity.
# ============================================================================

# ============================================================================
# Activity Log Alert: VMSS Deleted
# ============================================================================
# Why: Alerts when a VMSS is deleted. This is a critical event that affects
#      multiple VMs and could significantly impact cluster availability.
# Without it: VMSS deletions go unnoticed.
# ============================================================================
resource "azurerm_monitor_activity_log_alert" "vmss_deleted" {
  count = 0 # DISABLED: Informational alert
  # count               = var.enable_vm_lifecycle_alerts && length(var.vmss_resource_ids) > 0 ? 1 : 0
  name                = "${var.names.action_group}-vmss-deleted"
  resource_group_name = var.resource_group_name
  location            = "global"
  scopes              = [var.resource_group_id]
  description         = "Alert when a VMSS is deleted"
  tags                = var.tags

  criteria {
    category       = "Administrative"
    operation_name = "Microsoft.Compute/virtualMachineScaleSets/delete"
  }

  action {
    action_group_id = azurerm_monitor_action_group.vm_lifecycle[0].id
  }
}

# ============================================================================
# Activity Log Alert: VMSS Instance Deleted
# ============================================================================
# Why: Alerts when individual VMSS instances are deleted. This captures
#      scale-in events, spot evictions, and manual instance terminations.
# Without it: VMSS instance deletions go unnoticed.
# ============================================================================
resource "azurerm_monitor_activity_log_alert" "vmss_instance_deleted" {
  count = 0 # DISABLED: Informational alert
  # count               = var.enable_vm_lifecycle_alerts && length(var.vmss_resource_ids) > 0 ? 1 : 0
  name                = "${var.names.action_group}-vmss-instance-deleted"
  resource_group_name = var.resource_group_name
  location            = "global"
  scopes              = [var.resource_group_id]
  description         = "Alert when a VMSS instance is deleted (scale-in, spot eviction)"
  tags                = var.tags

  criteria {
    category       = "Administrative"
    operation_name = "Microsoft.Compute/virtualMachineScaleSets/virtualMachines/delete"
  }

  action {
    action_group_id = azurerm_monitor_action_group.vm_lifecycle[0].id
  }
}

# ============================================================================
# REMOVED: VMSS Scale Operation, VMSS Reimage alerts
# ============================================================================
# Why removed: Scale operations are expected with autoscaling and can be very
#              noisy. Reimages happen during updates and maintenance. For a
#              database cluster, instance deletion alerts (vmss_instance_deleted)
#              already capture the critical scale-in/spot eviction events.
# ============================================================================

# ============================================================================
# REMOVED: Service Health Alert
# ============================================================================
# Why removed: Service Health alerts notify about Azure-wide platform issues,
#              planned maintenance, and advisories. While useful, they can be
#              noisy and are less critical than direct resource health issues.
#              Resource Health alerts (below) capture VM-specific availability
#              problems which are more actionable for database operations.
# Reference: https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/best-practices-alerts
# ============================================================================

# ============================================================================
# REMOVED: Resource Health Alert
# ============================================================================
# Why removed (2026-01-27): This alert was too noisy for production use.
#   - Fired for all resources in the resource group (VMs, LBs, storage, etc.)
#   - Triggered on transient platform events that self-resolved within seconds
#   - Alert notification arrived 5+ minutes after auto-resolution (Activity Log delay)
#   - Created SRE alert fatigue without actionable information
#
# Original intent: Azure best practice for VM health notifications
# Problem: Scoping to resource group is too broad; transient LB health events
#          during Terraform operations and normal Azure platform maintenance
#          caused multiple false-positive Sev4 alerts per day.
#
# Alternative approaches if health monitoring is needed:
#   1. Use metric-based alerts (VipAvailability < X% for Y minutes)
#   2. Scope to specific resource IDs instead of entire resource group
#   3. Use Azure Resource Health dashboard for investigation, not alerting
#
# Reference: RCA_2026-01-27_LB_HEALTH.md documented the alert fatigue issue.
# ============================================================================
