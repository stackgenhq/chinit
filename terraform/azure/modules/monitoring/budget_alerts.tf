# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Budget Alerts for Cost Management
# ============================================================================
# Why: Budget alerts provide proactive cost monitoring by notifying when
#      spending approaches or exceeds defined thresholds. This prevents
#      unexpected cost overruns and enables timely cost optimization actions.
# Without it: Costs can exceed expectations without warning, leading to
#             budget overruns and delayed response to cost issues.
# ============================================================================
resource "azurerm_consumption_budget_resource_group" "clickhouse" {
  count             = var.budget_config.enabled ? 1 : 0
  name              = "${var.cluster_name}-budget"
  resource_group_id = var.budget_config.resource_group_id != null ? var.budget_config.resource_group_id : var.resource_group_id

  amount     = var.budget_config.monthly_amount
  time_grain = "Monthly"

  time_period {
    start_date = formatdate("YYYY-MM-01'T'00:00:00Z", timestamp())
    end_date   = formatdate("YYYY-MM-01'T'00:00:00Z", timeadd(timestamp(), "8760h")) # 1 year
  }

  # Actual spending alerts at configured thresholds
  dynamic "notification" {
    for_each = var.budget_config.alert_thresholds
    content {
      enabled        = true
      threshold      = notification.value
      operator       = "GreaterThan"
      threshold_type = "Actual"
      contact_emails = length(var.budget_config.notification_emails) > 0 ? var.budget_config.notification_emails : values(var.alert_email_addresses)
    }
  }

  # Forecasted spending alert
  notification {
    enabled        = true
    threshold      = var.budget_config.forecast_threshold
    operator       = "GreaterThan"
    threshold_type = "Forecasted"
    contact_emails = length(var.budget_config.notification_emails) > 0 ? var.budget_config.notification_emails : values(var.alert_email_addresses)
  }

  lifecycle {
    ignore_changes = [
      time_period, # Ignore time period changes on subsequent applies
    ]
  }
}

