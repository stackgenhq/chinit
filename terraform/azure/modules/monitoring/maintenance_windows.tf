# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Alert Processing Rules for Maintenance Windows
# ============================================================================
# Why: Alert Processing Rules (formerly Action Rules) suppress alerts during
#      scheduled maintenance windows to prevent alert fatigue. This follows
#      Azure best practices for operational excellence and ensures teams are
#      not flooded with expected alerts during planned maintenance activities.
# Without it: Alerts fire during planned maintenance (upgrades, backups, etc.),
#             causing alert fatigue and potentially leading to ignored alerts
#             during actual incidents.
# Reference: https://learn.microsoft.com/en-us/azure/azure-monitor/alerts/alerts-processing-rules
# ============================================================================
resource "azurerm_monitor_alert_processing_rule_suppression" "maintenance_windows" {
  for_each = var.enable_maintenance_windows ? {
    for idx, schedule in var.maintenance_window_schedules : schedule.name => schedule
  } : {}

  name                = "${var.names.action_group}-maint-${each.key}"
  resource_group_name = var.resource_group_name
  scopes              = [var.resource_group_id]
  description         = "Suppress alerts during maintenance window: ${each.key}"
  enabled             = true
  tags                = var.tags

  schedule {
    effective_from  = "${each.value.start_date}T${each.value.start_time}"
    effective_until = "${each.value.end_date}T${each.value.end_time}"
    time_zone       = each.value.time_zone

    recurrence {
      dynamic "daily" {
        for_each = each.value.recurrence_type == "Daily" ? [1] : []
        content {
          start_time = each.value.start_time
          end_time   = each.value.end_time
        }
      }

      dynamic "weekly" {
        for_each = each.value.recurrence_type == "Weekly" ? [1] : []
        content {
          start_time   = each.value.start_time
          end_time     = each.value.end_time
          days_of_week = each.value.days_of_week
        }
      }

      dynamic "monthly" {
        for_each = each.value.recurrence_type == "Monthly" ? [1] : []
        content {
          start_time    = each.value.start_time
          end_time      = each.value.end_time
          days_of_month = each.value.days_of_month
        }
      }
    }
  }
}

# ============================================================================
# One-Time Maintenance Window (Ad-hoc Suppression)
# ============================================================================
# Why: Provides a way to create ad-hoc maintenance windows for emergency
#      maintenance or one-time operations. These are created without
#      recurrence and are automatically disabled after the window ends.
# Without it: Teams must either disable alerts manually or accept alert noise
#             during unplanned but necessary maintenance activities.
# ============================================================================
# Note: To use one-time maintenance windows, create a separate variable or
#       use the maintenance_window_schedules with a narrow date range.
#       The current implementation supports scheduled recurring windows.
#       For one-time windows, set start_date == end_date.
