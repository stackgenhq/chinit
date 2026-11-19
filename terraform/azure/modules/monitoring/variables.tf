# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Monitoring Module Variables
# ============================================================================

variable "resource_group_name" {
  description = "Name of the Azure Resource Group"
  type        = string
}

variable "location" {
  description = "Azure region for resources"
  type        = string
}

variable "cluster_name" {
  description = "Name of the ClickHouse cluster"
  type        = string
}

variable "vm_resource_ids" {
  description = "List of regular VM resource IDs to monitor (Microsoft.Compute/virtualMachines)"
  type        = list(string)
  default     = []
}

variable "vmss_resource_ids" {
  description = "List of VMSS resource IDs to monitor (Microsoft.Compute/virtualMachineScaleSets)"
  type        = list(string)
  default     = []
}

variable "monitoring_config" {
  description = "Configuration for ClickHouse monitoring"
  type = object({
    enabled                   = optional(bool, true)
    log_analytics_sku         = optional(string, "PerGB2018")
    log_retention_days        = optional(number, 30)
    enable_basic_logs         = optional(bool, false)
    enable_clickhouse_alerts  = optional(bool, true)
    enable_certificate_alerts = optional(bool, true)
  })
}

variable "alert_thresholds" {
  description = "Thresholds for system alerts"
  type = object({
    cpu_percent  = optional(number, 80)
    memory_bytes = optional(number, 1073741824)
    disk_percent = optional(number, 85)
  })
  default = {}
}

variable "backup_config" {
  description = "Backup configuration for alerts"
  type = object({
    missing_threshold_hours     = optional(number, 36)
    enable_verification_alerts  = optional(bool, true)
    verification_threshold_days = optional(number, 7)
  })
  default = {}
}

variable "alert_email_addresses" {
  description = "Map of email addresses to receive alerts"
  type        = map(string)
  default     = {}
}

variable "alert_webhook_urls" {
  description = "Map of webhook URLs to receive alerts"
  type        = map(string)
  default     = {}
}

variable "enable_cpu_alerts" {
  description = "Enable CPU usage alerts"
  type        = bool
  default     = true
}


variable "enable_memory_alerts" {
  description = "Enable memory usage alerts"
  type        = bool
  default     = true
}


variable "enable_disk_alerts" {
  description = "Enable disk usage alerts"
  type        = bool
  default     = true
}


variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}

# ============================================================================
# Naming Variables (from Azure Naming Module)
# ============================================================================
# Why: Provides centralized naming convention from the Azure Naming module.
#      All resource names are generated using Azure CAF-compliant patterns
#      and passed from the root module to ensure consistency.
# Without it: Resource names must be manually constructed, leading to
#             inconsistent naming across monitoring resources.
# ============================================================================
variable "names" {
  description = "Naming outputs from the Azure Naming module"
  type = object({
    log_analytics_workspace = string
    data_collection_rule    = string
    action_group            = string
  })
}

# ============================================================================
# VM Lifecycle Notification Variables
# ============================================================================
# Why: Enables notifications when VMs are terminated, restarted, deallocated,
#      or mutated. Uses Azure Activity Log Alerts to monitor administrative
#      operations on VMs and VMSS.
# Without it: VM lifecycle events go unnoticed, leading to delayed incident
#             response.
# ============================================================================
variable "enable_vm_lifecycle_alerts" {
  description = "Enable notifications for VM lifecycle events (terminate, restart, deallocate)"
  type        = bool
  default     = true
}

variable "vm_lifecycle_notification_emails" {
  description = "List of email addresses to receive VM lifecycle notifications"
  type        = list(string)
  default     = []
}

variable "vm_lifecycle_webhook_url" {
  description = "Webhook URL for VM lifecycle notifications (Slack, PagerDuty, etc.)"
  type        = string
  default     = null
}

variable "resource_group_id" {
  description = "ID of the Azure Resource Group (used for Activity Log Alert scope)"
  type        = string
}

variable "subscription_id" {
  description = "Azure Subscription ID (required for Service Health alerts which must use subscription-level scope)"
  type        = string
}

# ============================================================================
# Maintenance Window Variables
# ============================================================================
# Why: Alert Processing Rules suppress alerts during scheduled maintenance windows
#      to prevent alert fatigue and unnecessary notifications during planned
#      operations. This follows Azure best practices for operational excellence.
# Without it: Alerts fire during planned maintenance, causing alert fatigue and
#             potentially leading to ignored alerts during actual incidents.
# ============================================================================
variable "enable_maintenance_windows" {
  description = "Enable alert processing rules for maintenance windows"
  type        = bool
  default     = false
}

variable "maintenance_window_schedules" {
  description = "List of maintenance window schedules with recurrence settings"
  type = list(object({
    name            = string
    recurrence_type = string
    start_date      = string
    end_date        = string
    start_time      = string
    end_time        = string
    time_zone       = string
    days_of_week    = optional(list(string), [])
    days_of_month   = optional(list(number), [])
  }))
  default = []
}

# ============================================================================
# Backup Failure Alert Variables
# ============================================================================
# Why: Enables monitoring and alerting for backup failures. This ensures
#      backup issues are detected immediately and can be addressed before
#      data loss occurs in a disaster scenario.
# Without it: Backup failures may go unnoticed, potentially leading to
#             data loss if a disaster occurs and no recent backups exist.
# ============================================================================
variable "enable_backup_failure_alerts" {
  description = "Enable alerts for backup failures and missing backups"
  type        = bool
  default     = true
}


# ============================================================================
# ClickHouse-Specific Alert Variables (BLINDSPOTS.md #21, #22, #23, #31)
# ============================================================================
# Why: ClickHouse-specific alerts monitor critical database health indicators
#      that are not captured by VM-level metrics. These are essential for
#      maintaining cluster stability and data integrity.
# Without it: Critical issues like Keeper quorum loss, replication lag, and
#             backup failures may go undetected until service degradation.
# ============================================================================



# ============================================================================
# Azure Monitor Agent Installation Mode
# ============================================================================
# Why: When Azure Monitor Agent is installed as a separate VMSS extension resource,
#      it triggers a rolling upgrade. On fresh deployments, this fails because
#      instances are still initializing (100% unhealthy exceeds max_unhealthy_percent).
#      By installing the agent as an inline VMSS extension (in the VMSS module),
#      the agent is installed during VM creation, avoiding the rolling upgrade issue.
# Without it: Both inline and separate extensions would try to install the agent,
#             causing conflicts and potential duplicate installations.
# ============================================================================
variable "skip_vmss_extension_install" {
  description = "Skip installing Azure Monitor Agent via separate VMSS extension (set to true when using inline VMSS extensions)"
  type        = bool
  default     = true
}

# ============================================================================
# Budget Alert Variables
# ============================================================================
# Why: Budget alerts provide proactive cost monitoring by notifying when
#      spending approaches or exceeds defined thresholds.
# Without it: Costs can exceed expectations without warning.
# ============================================================================
variable "budget_config" {
  description = "Configuration for budget alerts"
  type = object({
    enabled             = bool
    resource_group_id   = optional(string)
    monthly_amount      = optional(number, 500)
    alert_thresholds    = optional(list(number), [50, 80, 100])
    forecast_threshold  = optional(number, 100)
    notification_emails = optional(list(string), [])
  })
  default = {
    enabled = false
  }
}


# ============================================================================
# Load Balancer Alert Variables
# ============================================================================
# Why: Load balancer alerts monitor data path availability and backend health.
#      These are critical for detecting service outages before users are impacted.
# Reference: https://learn.microsoft.com/azure/load-balancer/load-balancer-standard-diagnostics
# ============================================================================
variable "enable_lb_alerts" {
  description = "Enable Load Balancer metric alerts (data path, health probe, SNAT)"
  type        = bool
  default     = true
}

variable "load_balancer_ids" {
  description = "List of Load Balancer resource IDs to monitor"
  type        = list(string)
  default     = []
}

variable "snat_exhaustion_threshold" {
  description = "SNAT connection count threshold for exhaustion alert (typical max is 1024 per frontend IP)"
  type        = number
  default     = 950 # ~95% of default 1024 SNAT ports
}


