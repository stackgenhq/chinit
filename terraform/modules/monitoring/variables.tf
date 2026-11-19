# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Monitoring Module Variables
# ============================================================================

variable "cluster_name" {
  description = "ClickHouse cluster name (alarm names, namespaces)"
  type        = string
}

variable "enabled" {
  description = "Master switch for the monitoring module"
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags for monitoring resources"
  type        = map(string)
  default     = {}
}

# CloudWatch Logs management
variable "log_retention_days" {
  description = "Retention (days) for the ClickHouse node log groups. 0 = never expire (compliance regimes requiring full history)"
  type        = number
  default     = 30

  validation {
    condition     = contains([0, 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a valid CloudWatch Logs retention value (see aws_cloudwatch_log_group retention_in_days)."
  }
}

variable "log_alarm_patterns" {
  description = "CloudWatch Logs metric filters over the ClickHouse server/error log groups. Defaults cover the four failure modes that only announce themselves in log lines. Set to [] to disable; null/omitted = use the curated defaults (local.curated_log_alarm_patterns in main.tf)."
  type = list(object({
    name      = string
    pattern   = string
    log_group = string
    threshold = optional(number, 0)
    guidance  = optional(string, "")
  }))
  default = null
}

# Notifications
variable "alert_emails" {
  description = "Email addresses for alarm notifications"
  type        = list(string)
  default     = []
}

variable "alert_webhooks" {
  description = "HTTPS webhook endpoints for alarm notifications"
  type        = list(string)
  default     = []
}

variable "alert_topic_arns" {
  description = "Existing SNS topic ARNs to also notify (besides the module's own topic)"
  type        = list(string)
  default     = []
}

# Fixed-identity slots (writers) + keepers/backup
variable "writer_hostnames" {
  description = "Writer hostname slots (writer-0..N) for per-node alarms. With the writers ASG these are reserved ORDINAL slots from the writer_max ceiling, not live membership (claims are lease-driven); the CW agent emits the claimed slot hostname as the 'host' dimension"
  type        = list(string)
  default     = []
}

variable "keeper_count" {
  description = "Number of keeper nodes. The keeper-leader-unsynced alarm (leader with too few synced followers) only applies to multi-keeper ensembles - a single dev keeper is a leader with zero followers by definition, so the alarm is suppressed when keeper_count < 2."
  type        = number
  default     = 1
}

variable "scale_down_cpu_threshold" {
  description = "Writer CPU percent BELOW which (sustained 6h) the scale-down alarm fires and the writers ASG step-scaling policy removes one writer (-1, bounded by writer_min)"
  type        = number
  default     = 20
}

variable "scale_down_delay_gate_seconds" {
  description = "ReplicasMaxAbsoluteDelay gate on writers scale-in (seconds): while ANY writer replica's absolute replication delay exceeds this, the writers-asg-idle alarm synthesizes 100% (never breaching) — a catching-up replica is mid-fetch and holds parts its peers may not have yet, so scale-in is deferred. 60s keeps normal fleet convergence from triggering; the queue drain after a burst takes minutes, and the gate holds the fleet until every replica is current."
  type        = number
  default     = 60
}

# ----------------------------------------------------------------------------
# Writers ASG — step-scaling actions + at-max guidance
# ----------------------------------------------------------------------------
# Why: writers scale HORIZONTALLY via the writers ASG now. The alarms below
#      act on step-scaling policy ARNs owned by the compute module (the same
#      module that owns the ASG — module cohesion), passed up as outputs and
#      down into here by root. This is a one-way dependency (compute does not
#      know about monitoring), so there is no cycle.
variable "writers_step_up_policy_arn" {
  description = "Step-scaling scale-UP policy ARN from the writers ASG (compute module output). Wired into the alarm_actions of writer-cpu-high / writer-insert-backpressure so those alarms ADD a writer (+1) instead of merely notifying. null = SNS-only alerts (pre-writers-ASG clusters; graceful fallback until b4's root wiring lands)."
  type        = string
  default     = null
}

variable "writers_step_down_policy_arn" {
  description = "Step-scaling scale-DOWN policy ARN from the writers ASG (compute module output). Wired into the AGGREGATE writers-asg-idle alarm (ASG CPUUtilization dimension) so sustained low fleet-average CPU removes a writer (-1, bounded by writer_min). The per-host writer-N-cpu-low alarms are SNS-only guidance: a host-keyed alarm stays ALARM after its own scale-in terminates the host and re-kills every scale-out. null = SNS-only alerts (pre-writers-ASG clusters; graceful fallback)."
  type        = string
  default     = null
}

variable "writers_asg_name" {
  description = "Writers ASG name (the at-max-capacity guidance alarm dimensions on it, mirroring readers_asg_name)."
  type        = string
  default     = null
}

variable "readers_step_up_policy_arn" {
  description = "Step-scaling scale-UP policy ARN from the readers ASG (compute module output). Wired into the alarm_actions of the parts-per-partition alarm so merge/queue backlog ADDS a reader (+1) instead of merely notifying — the 30TB load test finding: replication catch-up and merge backlog are background work that CW cpu_usage_idle dilutes, so target-tracking alone under-scales the readers. null = SNS-only alerts (pre-readers-ASG clusters; graceful fallback)."
  type        = string
  default     = null
}

# Readers ASG
variable "readers_asg_name" {
  description = "Readers ASG name"
  type        = string
}

variable "autoscaling_enabled" {
  description = "Whether target-tracking autoscaling is on (gates the ASG CPU alarm)"
  type        = bool
  default     = false
}

variable "reader_cpu_scale_out" {
  description = "Readers ASG CPU scale-out threshold (percent)"
  type        = number
  default     = 70
}

variable "idle_cpu_threshold" {
  description = "Idle-capacity alert: ASG avg CPU below this for 7 days = scale in"
  type        = number
  default     = 15
}

variable "enable_idle_alerts" {
  description = "Enable the weekly idle-capacity shrink alert"
  type        = bool
  default     = true
}

# Scale-guidance alarms (non-actioning)
variable "enable_vertical_guidance" {
  description = "Enable the writers NLB p99 scale-guidance alarm (the writer CPU/memory alarms are always on; only the NLB latency guidance alarm is gated here)"
  type        = bool
  default     = true
}

# Writer-fleet EMF alarms (replication stall + merge backlog)
variable "enable_writer_alarms" {
  description = "Enable the writer-fleet EMF alarms: replication-stalled (ReplicasMaxAbsoluteDelay — a table-less 'healthy' replica the node-local alarms cannot see) and parts-per-partition-high (the merge-backlog early warning at half the insert hard-reject). Both key on the CW agent's prometheus-bridge metrics (ClickHouse/Server namespace)."
  type        = bool
  default     = true
}

variable "writer_cpu_threshold" {
  description = "Sustained writer CPU percent triggering the writers ASG step-scale-UP alarm (and the at-max guidance alarm writers-asg-cpu-high). Root passes monitoring_config.writer_cpu_threshold; the ASG target-tracking policy separately holds autoscaling_config.writer_cpu_target (default 65 both). The dual signals are intentional: target tracking is continuous and smooth; the step alarm adds a bounded +1 on sustained 30-minute pressure"
  type        = number
  default     = 65
}

variable "writer_memory_threshold" {
  description = "Writer memory percent triggering the memory-high guidance alarm (SNS-only — memory is node-local, not a horizontal scaling signal)"
  type        = number
  default     = 80
}

variable "disk_percent_threshold" {
  description = "Metadata/cache disk percent used triggering alarms"
  type        = number
  default     = 85
}

# NLBs
variable "writers_lb_arn_suffix" {
  description = "Writers NLB ARN suffix (load balancer dimension)"
  type        = string
}

variable "readers_lb_arn_suffix" {
  description = "Readers NLB ARN suffix (load balancer dimension)"
  type        = string
}

# Target-group suffixes: NetworkELB publishes UnHealthyHostCount and
# TargetResponseTime per LoadBalancer+TargetGroup PAIR — the
# LoadBalancer-only dimension never matches a series and the alarm
# sits INSUFFICIENT_DATA forever # alarm blind since creation).
variable "writers_http_tg_arn_suffix" {
  description = "Writers HTTP target-group ARN suffix (target group dimension)"
  type        = string
}

variable "readers_http_tg_arn_suffix" {
  description = "Readers HTTP target-group ARN suffix (target group dimension)"
  type        = string
}

# ClickHouse-level alarms (scraped from the nodes' :9363/metrics endpoint)
# Why: The Altinity operator ships 37 ClickHouse + 9 Keeper alert rules
#      (deploy/prometheus/prometheus-alert-rules-*.yaml) covering failure
#      modes node-level alarms can't see. The subset below translates the
#      high-value rules onto CloudWatch metrics the CW agent now scrapes.
variable "enable_ch_alerts" {
  description = "Enable ClickHouse-level alarms (readonly replicas, replication lag, distributed backlog, parts, queries, uptime)"
  type        = bool
  default     = true
}

variable "ch_alert_thresholds" {
  description = "Thresholds for ClickHouse-level alarms (defaults from Altinity operator alert rules)"
  type = object({
    replica_max_delay_seconds       = optional(number, 300)
    distributed_files_to_insert     = optional(number, 50)
    max_parts_per_partition         = optional(number, 1500) # 1500 warning threshold (parts_to_throw_insert is 5000)
    disk_effective_low_bytes        = optional(number, 17179869184)
    max_running_queries             = optional(number, 100)
    s3queue_lag_seconds             = optional(number, 3600)
    min_uptime_seconds              = optional(number, 180)
    keeper_max_latency_ms           = optional(number, 5000) # Cumulative window MAX in KeeperConnectionStats::getMaxLatency()
    keeper_max_outstanding_requests = optional(number, 10)
    keeper_znode_count              = optional(number, 500000)
    keeper_min_synced_followers     = optional(number, 1) # Evaluated via Maximum across {ClusterName, NodeType=keeper}
    keeper_max_fd_ratio             = optional(number, 0.8)
  })
  default = {}
}

# Backups
variable "backup_bucket_name" {
  description = "S3 backup bucket (backup-freshness alarm)"
  type        = string
}

variable "enable_backup_alerts" {
  description = "Enable the backup-missing alarm"
  type        = bool
  default     = true
}

variable "backup_missing_threshold_hours" {
  description = "Alarm when no backup object lands for this many hours"
  type        = number
  default     = 36
}

# TLS
variable "cert_expiration_date" {
  description = "ACME certificate expiry (null = acm mode, ACM renews itself)"
  type        = string
  default     = null
}

# Budgets
variable "budget_enabled" {
  description = "Create AWS Budgets (first 2 per account are free)"
  type        = bool
  default     = true
}

variable "budget_monthly_amount" {
  description = "Monthly budget amount (USD)"
  type        = number
  default     = 5000
}

variable "budget_alert_thresholds" {
  description = "Actual-spend alert thresholds (percent of budget)"
  type        = list(number)
  default     = [50, 80, 100]
}

variable "budget_forecast_threshold" {
  description = "Forecasted-spend alert threshold (percent); null disables"
  type        = number
  default     = 100
}

variable "budget_alert_emails" {
  description = "Budget notification emails"
  type        = list(string)
  default     = []
}

variable "keeper_hostnames" {
  description = "Keeper node hostnames (the CW agent's host dimension). Drives the keeper CPU alarms that page the human when the autoscale coordinator's ladder ceiling is the binding constraint."
  type        = list(string)
  default     = []
}

variable "keeper_cpu_threshold" {
  description = "Keeper CPU % that triggers the vertical resize alarm (the user's 80%-max rule: keepers scale VERTICALLY at 80% CPU before saturation breaks the raft quorum's latency budget)."
  type        = number
  default     = 80
}
