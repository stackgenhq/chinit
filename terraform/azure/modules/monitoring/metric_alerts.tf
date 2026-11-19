# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Metric Alert: High CPU Usage (Regular VMs)
# ============================================================================
# Why: Alerts when CPU usage exceeds threshold on regular VMs, indicating
#      potential performance issues or need for scaling.
# Without it: High CPU usage may go undetected, causing query performance
#             degradation and potential timeouts.
# ============================================================================
resource "azurerm_monitor_metric_alert" "high_cpu_vms" {
  count                    = length(var.vm_resource_ids) > 0 && var.enable_cpu_alerts ? 1 : 0
  name                     = "${var.names.action_group}-high-cpu-vms"
  resource_group_name      = var.resource_group_name
  scopes                   = var.vm_resource_ids
  description              = "Alert when CPU usage exceeds ${var.alert_thresholds.cpu_percent}% on VMs"
  severity                 = 2
  frequency                = "PT1M"
  window_size              = "PT5M"
  enabled                  = var.enable_cpu_alerts
  target_resource_type     = "Microsoft.Compute/virtualMachines"
  target_resource_location = var.location

  criteria {
    metric_namespace = "Microsoft.Compute/virtualMachines"
    metric_name      = "Percentage CPU"
    aggregation      = "Average"
    operator         = "GreaterThan"
    threshold        = var.alert_thresholds.cpu_percent
  }

  action {
    action_group_id = azurerm_monitor_action_group.clickhouse.id
  }

  tags = var.tags
}

# ============================================================================
# Metric Alert: High CPU Usage (VMSS)
# ============================================================================
# Why: Alerts when CPU usage exceeds threshold on VMSS, indicating potential
#      performance issues or need for scaling.
# Without it: High CPU usage may go undetected, causing query performance
#             degradation and potential timeouts.
# ============================================================================
resource "azurerm_monitor_metric_alert" "high_cpu_vmss" {
  count = length(var.vmss_resource_ids) > 0 && var.enable_cpu_alerts ? 1 : 0
  # count                    = 0 # DISABLED: Informational alert
  name                     = "${var.names.action_group}-high-cpu-vmss"
  resource_group_name      = var.resource_group_name
  scopes                   = var.vmss_resource_ids
  description              = "Alert when CPU usage exceeds ${var.alert_thresholds.cpu_percent}% on VMSS"
  severity                 = 2
  frequency                = "PT1M"
  window_size              = "PT5M"
  enabled                  = var.enable_cpu_alerts
  target_resource_type     = "Microsoft.Compute/virtualMachineScaleSets"
  target_resource_location = var.location

  criteria {
    metric_namespace = "Microsoft.Compute/virtualMachineScaleSets"
    metric_name      = "Percentage CPU"
    aggregation      = "Average"
    operator         = "GreaterThan"
    threshold        = var.alert_thresholds.cpu_percent
  }

  action {
    action_group_id = azurerm_monitor_action_group.clickhouse.id
  }

  tags = var.tags
}

# ============================================================================
# Metric Alert: High Memory Usage (Regular VMs)
# ============================================================================
# Why: Alerts when memory usage exceeds threshold on regular VMs, indicating
#      potential memory pressure or need for larger VM sizes.
# Without it: High memory usage may cause OOM kills or query failures,
#             impacting cluster stability.
# ============================================================================
resource "azurerm_monitor_metric_alert" "high_memory_vms" {
  count = length(var.vm_resource_ids) > 0 && var.enable_memory_alerts ? 1 : 0
  # count                    = 0 # DISABLED: Informational alert
  name                     = "${var.names.action_group}-high-memory-vms"
  resource_group_name      = var.resource_group_name
  scopes                   = var.vm_resource_ids
  description              = "Alert when available memory falls below threshold on VMs"
  severity                 = 2
  frequency                = "PT1M"
  window_size              = "PT5M"
  enabled                  = var.enable_memory_alerts
  target_resource_type     = "Microsoft.Compute/virtualMachines"
  target_resource_location = var.location

  criteria {
    metric_namespace = "Microsoft.Compute/virtualMachines"
    metric_name      = "Available Memory Bytes"
    aggregation      = "Average"
    operator         = "LessThan"
    threshold        = var.alert_thresholds.memory_bytes
  }

  action {
    action_group_id = azurerm_monitor_action_group.clickhouse.id
  }

  tags = var.tags
}

# ============================================================================
# Metric Alert: High Memory Usage (VMSS)
# ============================================================================
# Why: Alerts when memory usage exceeds threshold on VMSS, indicating
#      potential memory pressure or need for larger VM sizes.
# Without it: High memory usage may cause OOM kills or query failures,
#             impacting cluster stability.
# ============================================================================
resource "azurerm_monitor_metric_alert" "high_memory_vmss" {
  count = length(var.vmss_resource_ids) > 0 && var.enable_memory_alerts ? 1 : 0
  # count                    = 0 # DISABLED: Informational alert
  name                     = "${var.names.action_group}-high-memory-vmss"
  resource_group_name      = var.resource_group_name
  scopes                   = var.vmss_resource_ids
  description              = "Alert when available memory falls below threshold on VMSS"
  severity                 = 2
  frequency                = "PT1M"
  window_size              = "PT5M"
  enabled                  = var.enable_memory_alerts
  target_resource_type     = "Microsoft.Compute/virtualMachineScaleSets"
  target_resource_location = var.location

  criteria {
    metric_namespace = "Microsoft.Compute/virtualMachineScaleSets"
    metric_name      = "Available Memory Bytes"
    aggregation      = "Average"
    operator         = "LessThan"
    threshold        = var.alert_thresholds.memory_bytes
  }

  action {
    action_group_id = azurerm_monitor_action_group.clickhouse.id
  }

  tags = var.tags
}

# ============================================================================
# Metric Alert: High Disk Usage (Regular VMs)
# ============================================================================
# Why: Alerts when disk usage exceeds threshold on regular VMs, preventing
#      disk space exhaustion that could cause write failures.
# NOTE: Disk usage percentage metric is not directly available in Azure Monitor
#       for VMs. This alert is disabled by default. Consider using log-based
#       alerts with VM Insights or custom scripts to monitor disk space.
# Without it: Disk space exhaustion may cause ClickHouse write failures,
#             metadata corruption, or service crashes.
# ============================================================================
# Disabled - disk usage percentage metric not available for VMs
# resource "azurerm_monitor_metric_alert" "high_disk_vms" {
#   ...
# }

# ============================================================================
# Metric Alert: High Disk Usage (VMSS)
# ============================================================================
# Why: Alerts when disk usage exceeds threshold on VMSS, preventing disk
#      space exhaustion that could cause write failures.
# NOTE: Disk usage percentage metric is not directly available in Azure Monitor
#       for VMSS. This alert is disabled by default. Consider using log-based
#       alerts with VM Insights or custom scripts to monitor disk space.
# Without it: Disk space exhaustion may cause ClickHouse write failures,
#             metadata corruption, or service crashes.
# ============================================================================
# Disabled - disk usage percentage metric not available for VMSS
# resource "azurerm_monitor_metric_alert" "high_disk_vmss" {
#   ...
# }

# ============================================================================
# Load Balancer Data Path Availability Alert (CRITICAL - Sev1)
# ============================================================================
# Why: Data Path Availability = 0% means complete outage - no traffic can
#      reach backend instances. This is a Sev1 critical alert requiring
#      immediate response.
# Note: Azure doesn't support multi-resource alerts for Load Balancers,
#       so we create separate alerts for each LB using for_each.
# Reference: https://learn.microsoft.com/azure/load-balancer/load-balancer-standard-diagnostics
# ============================================================================
resource "azurerm_monitor_metric_alert" "lb_data_path_critical" {
  for_each = var.enable_lb_alerts ? toset(var.load_balancer_ids) : toset([])

  name                = "${var.names.action_group}-lb-datapath-${replace(basename(each.value), "/[^a-zA-Z0-9-]/", "")}"
  resource_group_name = var.resource_group_name
  scopes              = [each.value]
  description         = "CRITICAL: Load Balancer data path availability is severely degraded (${basename(each.value)})"
  severity            = 1 # Sev1 - Critical, page immediately
  frequency           = "PT1M"
  window_size         = "PT5M"
  enabled             = var.enable_lb_alerts

  criteria {
    metric_namespace = "Microsoft.Network/loadBalancers"
    metric_name      = "VipAvailability"
    aggregation      = "Average"
    operator         = "LessThan"
    threshold        = 25 # < 25% = Unavailable per Azure definition
  }

  action {
    action_group_id = azurerm_monitor_action_group.clickhouse.id
  }

  tags = var.tags
}

# ============================================================================
# Load Balancer Health Probe Degraded Alert (HIGH - Sev2)
# ============================================================================
# Why: Health Probe Status < 100% means some backends are unhealthy.
#      Alert after 5 minutes to filter out transient blips from VM reboots
#      or deployment operations.
# ============================================================================
resource "azurerm_monitor_metric_alert" "lb_health_probe_degraded" {
  for_each = var.enable_lb_alerts ? toset(var.load_balancer_ids) : toset([])

  name                = "${var.names.action_group}-lb-health-${replace(basename(each.value), "/[^a-zA-Z0-9-]/", "")}"
  resource_group_name = var.resource_group_name
  scopes              = [each.value]
  description         = "Load Balancer health probe degraded - >30% backends unhealthy >5min (${basename(each.value)})"
  severity            = 1 # Sev1 - High
  frequency           = "PT1M"
  window_size         = "PT5M"
  enabled             = var.enable_lb_alerts

  criteria {
    metric_namespace = "Microsoft.Network/loadBalancers"
    metric_name      = "DipAvailability"
    aggregation      = "Average"
    operator         = "LessThan"
    threshold        = 70 # Alert if availability < 70% (i.e. > 30% of instances are down)
  }

  action {
    action_group_id = azurerm_monitor_action_group.clickhouse.id
  }

  tags = var.tags
}
