# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Variables — chinit cluster module (AWS, ecosystem-composed)
# ============================================================================
# A deliberately SMALL facade: every input has a secure default, and the
# module composes the ecosystem modules (network, storage, secrets,
# clickhouse, compute, dns_tls, monitoring, security). Power users bypass
# this facade and compose the ecosystem modules directly — the facade is
# the on-ramp, not the ceiling.
# ============================================================================

# ----------------------------------------------------------------------------
# Cluster identity
# ----------------------------------------------------------------------------
variable "cluster_name" {
  description = "Cluster name (carried by every resource, DNS name, spec, and tag)."
  type        = string
}

variable "region" {
  description = "AWS region the cluster runs in."
  type        = string
}

variable "tags" {
  description = "Tags applied to every resource this module creates."
  type        = map(string)
  default     = {}
}

# ----------------------------------------------------------------------------
# Golden AMI
# ----------------------------------------------------------------------------
variable "golden_ami_id" {
  description = "Golden AMI id built from images/aws (packer). null = auto-resolve the latest public stackgenhq/chinit golden AMI for the cas_enabled flavor (us-west-2 carries both public flavors)."
  type        = string
  default     = null
}

# ----------------------------------------------------------------------------
# Fleet shape
# ----------------------------------------------------------------------------
variable "writer_max" {
  description = "Writers ASG max_size — the writer ordinal claim CEILING (writer-N slots in [0, max))."
  type        = number
  default     = 2
}

variable "writer_desired" {
  description = "Writers ASG desired_capacity (>= 1: writer-0 coordinates backup wake + maintenance)."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_desired >= 1 && var.writer_desired <= var.writer_max
    error_message = "writer_desired must be >= 1 (writer-0 stays alive) and <= writer_max."
  }
}

variable "reader_max" {
  description = "Readers ASG max_size — the reader ordinal claim CEILING (reader-N slots in [0, max))."
  type        = number
  default     = 6
}

variable "reader_desired" {
  description = "Readers ASG desired_capacity."
  type        = number
  default     = 2
}

variable "reader_spot" {
  description = "Fraction of reader slots above the on-demand base on Spot (0.0-1.0). 1.0 = every slot above the base is Spot. Spot readers drain gracefully on the EC2 interruption notice (chinit spot-watch)."
  type        = number
  default     = 0.5

  validation {
    condition     = var.reader_spot >= 0 && var.reader_spot <= 1
    error_message = "reader_spot must be between 0.0 and 1.0."
  }
}

variable "reader_spot_on_demand_base_capacity" {
  description = "Reader slots kept on-demand regardless of reader_spot (an always-on query floor)."
  type        = number
  default     = 1
}

variable "keeper_count" {
  description = "Keeper nodes (odd, typically 3 — Raft quorum)."
  type        = number
  default     = 3

  validation {
    condition     = var.keeper_count % 2 == 1 && var.keeper_count >= 1
    error_message = "keeper_count must be an odd number >= 1 (Raft quorum)."
  }
}

variable "enable_backup" {
  description = "Provision the dedicated backup replica (clickhouse-backup, nightly schedule)."
  type        = bool
  default     = true
}

# ----------------------------------------------------------------------------
# Instance shapes (Graviton defaults; NVMe-capable families can drop the
# gp3 cache volume via cache_on_nvme_instance_store)
# ----------------------------------------------------------------------------
variable "instance_type" {
  description = "Writer/reader/backup EC2 instance type (Graviton arm64: m8g/r8gd/c8gd/i4g families; >= 8 GiB RAM recommended)."
  type        = string
  default     = "m8g.large"
}

variable "keeper_instance_type" {
  description = "Keeper EC2 instance type (small is fine: keepers hold no table data)."
  type        = string
  default     = "t4g.medium"
}

variable "root_disk_size_gb" {
  description = "Root EBS volume size (GiB) on every node."
  type        = number
  default     = 16
}

variable "metadata_disk_size_gb" {
  description = "Metadata (hot-tier) gp3 volume size (GiB) on writers/readers/backup. Grows online via chinit disk-autoscale."
  type        = number
  default     = 64
}

variable "metadata_disk_iops" {
  description = "Provisioned gp3 IOPS for the metadata disk."
  type        = number
  default     = 3000
}

variable "metadata_disk_throughput_mbps" {
  description = "Provisioned gp3 throughput (MB/s) for the metadata disk."
  type        = number
  default     = 125
}

variable "cache_disk_size_gb" {
  description = "S3-read-cache gp3 volume size (GiB) on writers/readers. Set to 0 with cache_on_nvme_instance_store on NVMe instance-store families."
  type        = number
  default     = 64
}

variable "keeper_disk_size_gb" {
  description = "Keeper Raft-log gp3 volume size (GiB) — dedicated disk for fsync latency isolation."
  type        = number
  default     = 16
}

variable "cache_on_nvme_instance_store" {
  description = "Drop the gp3 cache volume and use the (free) instance-store NVMe as the S3 cache disk. Only safe when every pool/ladder member carries local NVMe (m*g/r*g/c*g/i*g families); the module guards this."
  type        = bool
  default     = false
}

# ----------------------------------------------------------------------------
# Network (fresh VPC by default; bring-your-own via create_network = false)
# ----------------------------------------------------------------------------
variable "create_network" {
  description = "Create a fresh VPC + private subnet + free S3/DynamoDB gateway endpoints. false = plug into vpc_id/subnet_id (the .20+.30 IP offsets must be free)."
  type        = bool
  default     = true
}

variable "vpc_id" {
  description = "(create_network = false) Existing VPC id."
  type        = string
  default     = null
}

variable "subnet_id" {
  description = "(create_network = false) Existing private subnet id for the nodes."
  type        = string
  default     = null
}

variable "subnet_cidr" {
  description = "Nodes subnet CIDR — the cluster placement domain AND the source of the keeper (+20..29) / backup (+30) static IPs. /24 or larger."
  type        = string
  default     = "10.42.0.0/24"
}

variable "client_cidrs" {
  description = "CIDRs allowed to reach client ports (8123/8443/9000/9440) on writers + readers. Empty = no client ingress (add rules later via the SG ids in the outputs)."
  type        = list(string)
  default     = []
}

variable "dns_zone_name" {
  description = "Private DNS zone for node discovery (writer-N/reader-N/keeper-N/backup-0.<zone>)."
  type        = string
  default     = "clickhouse.internal"
}

# ----------------------------------------------------------------------------
# Storage
# ----------------------------------------------------------------------------
variable "data_bucket_name" {
  description = "S3 data bucket name (null = <cluster_name>-clickhouse-data). Table parts land here; the tiered storage.xml TTL-moves merged parts into it."
  type        = string
  default     = null
}

variable "backup_bucket_name" {
  description = "S3 backup bucket name (null = <cluster_name>-clickhouse-backup)."
  type        = string
  default     = null
}

variable "s3_data_prefix" {
  description = "Key prefix inside the data bucket for table parts (data/<tenant>/<replica>/ renders per tenant)."
  type        = string
  default     = "data"
}

variable "backup_retention_days" {
  description = "Days clickhouse-backup keeps backup objects in the backup bucket."
  type        = number
  default     = 30
}

variable "tenants" {
  description = "Tenant names — each gets its own data/<tenant>/<replica>/ S3 prefix, cache wrapper, and tiered storage policy (a tenant is a prefix + policy, never a disk-resizing project)."
  type        = list(string)
  default     = ["default"]
}

variable "lifecycle_days_to_ia" {
  description = "Days before data-bucket objects transition to Standard-IA (pairs with ClickHouse move_factor; never Glacier — queries must read)."
  type        = number
  default     = 30
}

# ----------------------------------------------------------------------------
# CAS cold tier (Altinity Antalya)
# ----------------------------------------------------------------------------
variable "cas_enabled" {
  description = "Render the shared content-addressed S3 cold tier (cas disks + tiered_cas policies). Requires an Antalya-built golden AMI and a data bucket WITHOUT versioning/object-lock. Cold parts stored once across replicas instead of per-replica."
  type        = bool
  default     = false
}

variable "cas_prefix" {
  description = "Key prefix inside the data bucket for the CAS pool."
  type        = string
  default     = "cas"
}

# ----------------------------------------------------------------------------
# Secrets + rotation
# ----------------------------------------------------------------------------
variable "enable_secret_rotation" {
  description = "Deploy the password-rotation Lambda (Secrets Manager 4-step protocol; the fleet re-renders users.d via SSM). The Lambda needs SSM reachability to the fleet."
  type        = bool
  default     = true
}

variable "rotation_days" {
  description = "Password rotation interval in days (Secrets Manager schedule)."
  type        = number
  default     = 30
}

# ----------------------------------------------------------------------------
# Autoscaling + rolling refresh
# ----------------------------------------------------------------------------
variable "autoscaling_enabled" {
  description = "Horizontal autoscaling: readers track CPU (bounded by reader_max); writers get step policies (the alarms act on them; the ceiling stays writer_max)."
  type        = bool
  default     = true
}

variable "writer_cpu_target" {
  description = "Writers target-tracking CPU percent (the scale-OUT signal; scale-in is owned by the idle alarm)."
  type        = number
  default     = 65
}

variable "refresh_instance_warmup_seconds" {
  description = "Instance-refresh warmup (seconds) — a rolled node must finish chinit's Critical boot steps before the next one rolls."
  type        = number
  default     = 300
}

variable "refresh_min_healthy_percentage" {
  description = "Instance-refresh minimum healthy percentage."
  type        = number
  default     = 50
}

# ----------------------------------------------------------------------------
# Monitoring + security posture (opt-in extras around the always-on core)
# ----------------------------------------------------------------------------
variable "alert_emails" {
  description = "Email subscribers on the cluster SNS alerts topic (alarms, findings, budget alerts)."
  type        = list(string)
  default     = []
}

variable "enable_guardduty" {
  description = "Enable GuardDuty (managed threat detection; S3 data-events protection is on — the cluster's biggest data surface)."
  type        = bool
  default     = true
}

variable "enable_s3_protection" {
  description = "GuardDuty S3 data-events protection (S3 data events billing; ON by default for the data/backup buckets)."
  type        = bool
  default     = true
}

variable "enable_security_hub" {
  description = "Enable Security Hub with the AWS Foundational Security Best Practices + CIS standards."
  type        = bool
  default     = true
}

variable "findings_to_sns_enabled" {
  description = "Route HIGH/CRITICAL GuardDuty + Security Hub findings onto the cluster's SNS alerts topic."
  type        = bool
  default     = true
}

variable "budget_monthly_amount" {
  description = "Monthly USD budget for the cluster's cost tags (0 = budget alerts disabled)."
  type        = number
  default     = 0
}

# ----------------------------------------------------------------------------
# Telemetry (opt-in; no defaults dial out)
# ----------------------------------------------------------------------------
variable "log_shipping" {
  description = "Remote log/metric/trace shipping. Default = local-only (CloudWatch agent + the module's alarms)."
  type = object({
    agent                  = optional(string)
    endpoint               = optional(string)
    logs_endpoint          = optional(string)
    metrics_endpoint       = optional(string)
    traces_endpoint        = optional(string)
    headers                = optional(string)
    metrics_headers        = optional(string)
    traces_headers         = optional(string)
    scrape_interval        = optional(string)
    fluentbit_output       = optional(string)
    otel_trace_probability = optional(number)
    span_log_ttl_days      = optional(number)
    trace_log_ttl_days     = optional(number)
  })
  default = {}
}

variable "tracing_config" {
  description = "ClickHouse-side tracing knobs (OpenTelemetry span sampling + trace/query-log TTLs)."
  type = object({
    otel_trace_probability        = optional(number)
    otel_keeper_trace_probability = optional(string)
    otel_trace_processors         = optional(bool)
    otel_trace_cpu_scheduling     = optional(bool)
    otel_span_log_ttl_days        = optional(number)
    trace_log_ttl_days            = optional(number)
    global_profiler_period_ns     = optional(number)
    log_query_threads             = optional(bool)
  })
  default = {}
}
