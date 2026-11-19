# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Action Group for Alerts
# ============================================================================
# Why: Defines notification channels (email, SMS, webhooks) for metric alerts.
#      Enables proactive alerting when critical issues are detected.
# Without it: Metric alerts cannot send notifications. Issues may go
#             undetected until manual checks are performed.
# ============================================================================
resource "azurerm_monitor_action_group" "clickhouse" {
  name                = var.names.action_group
  resource_group_name = var.resource_group_name
  short_name          = "ch-alert"
  tags                = var.tags

  dynamic "email_receiver" {
    for_each = var.alert_email_addresses
    content {
      name          = "email-${email_receiver.key}"
      email_address = email_receiver.value
    }
  }

  dynamic "webhook_receiver" {
    for_each = var.alert_webhook_urls
    content {
      name        = "webhook-${webhook_receiver.key}"
      service_uri = webhook_receiver.value
    }
  }
}

# ============================================================================
# VM Lifecycle Notifications Action Group
# ============================================================================
# Why: Defines a separate action group for VM lifecycle notifications (email
#      and/or webhook). This enables different notification channels for
#      lifecycle events vs metric alerts. Using CloudEvents-compatible webhooks
#      follows open standards for event-driven architectures.
# Without it: VM lifecycle events cannot be sent to dedicated channels,
#             mixing operational alerts with infrastructure events.
# ============================================================================
resource "azurerm_monitor_action_group" "vm_lifecycle" {
  count               = var.enable_vm_lifecycle_alerts ? 1 : 0
  name                = "${var.names.action_group}-vm-lifecycle"
  resource_group_name = var.resource_group_name
  short_name          = "vm-life"
  tags                = var.tags

  dynamic "email_receiver" {
    for_each = var.vm_lifecycle_notification_emails
    content {
      name                    = "lifecycle-email-${email_receiver.key}"
      email_address           = email_receiver.value
      use_common_alert_schema = true # CloudEvents-compatible alert schema
    }
  }

  dynamic "webhook_receiver" {
    for_each = var.vm_lifecycle_webhook_url != null ? [var.vm_lifecycle_webhook_url] : []
    content {
      name                    = "lifecycle-webhook"
      service_uri             = webhook_receiver.value
      use_common_alert_schema = true # CloudEvents-compatible alert schema
    }
  }
}
