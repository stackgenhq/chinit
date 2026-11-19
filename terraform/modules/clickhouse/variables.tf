# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse Module Variables
# ============================================================================

variable "cluster_name" {
  description = "ClickHouse cluster name"
  type        = string
}

variable "clickhouse_version" {
  description = "Target ClickHouse version (used to gate 26.8+ user profile settings such as s3_allow_server_credentials_in_user_queries)"
  type        = string
  default     = "26.8.36.3"
}


variable "timezone" {
  description = "Timezone for the ClickHouse cluster"
  type        = string
  default     = "UTC"
}

variable "aws_region" {
  description = "AWS region"
  type        = string
}

variable "dns_zone_name" {
  description = "Private DNS zone name for node discovery"
  type        = string
}


variable "keeper_hostnames" {
  description = "Keeper hostnames (keeper-0, keeper-1, ...)"
  type        = list(string)
}

variable "writer_hostnames" {
  description = "Reserved writer ordinal slot list (writer-0..writer-{writer_max-1}) — the writer slot CEILING (writer_max = the writers ASG max), not the live fleet size. chinit discovers live writer membership from the node_claims lease table; the list length feeds writer_max (ordinal claim ceiling) and the CR slot ceiling."
  type        = list(string)
}

variable "reader_count" {
  description = "Reader ordinal claim ceiling — the readers ASG max (reader_max). Legacy variable name: root passes the ASG max here, not the baseline count, so leased readers can always claim an ordinal on scale-out."
  type        = number
}

# ----------------------------------------------------------------------------
# ClickHouse logging hygiene: the CloudWatch
# remote pipe ships only `ch_log_level`; verbose lines stay in-cluster in
# system.text_log (ZSTD-compressed, TTL-dropped) with ch_text_log_*.
# ----------------------------------------------------------------------------

variable "ch_log_level" {
  description = "External sink <level> in server-logger.xml (rotating file the CW agent ships at metered per-GB ingest). `warning` is the cost posture — S3Queue poll/claim, MergeTree and replication chatter stay in system.text_log instead. Poco levels: trace|debug|information|warning|error|critical."
  type        = string
  default     = "warning"

  validation {
    condition     = contains(["trace", "debug", "information", "warning", "error", "critical"], var.ch_log_level)
    error_message = "ch_log_level must be a Poco log level: trace|debug|information|warning|error|critical."
  }
}

variable "ch_text_log_level" {
  description = "system.text_log <level> — the in-cluster verbose log. Must sit at or above ch_log_level (the root logger rises to the max of both, Loggers.cpp); `information` keeps SSM/SQL-debuggable detail in the cluster."
  type        = string
  default     = "information"

  validation {
    condition     = contains(["trace", "debug", "information", "warning", "error", "critical"], var.ch_text_log_level)
    error_message = "ch_text_log_level must be a Poco log level: trace|debug|information|warning|error|critical."
  }
}

variable "ch_text_log_ttl_days" {
  description = "system.text_log TTL days (TTL event_date + INTERVAL N DAY DELETE). 0 = rows kept forever — never recommended (unbounded table)."
  type        = number
  default     = 7

  validation {
    condition     = var.ch_text_log_ttl_days >= 0
    error_message = "ch_text_log_ttl_days must be >= 0 (0 keeps rows forever)."
  }
}

variable "insert_quorum" {
  description = "The ingest profile's insert_quorum value. MUST be derived from the fleet's GUARANTEED-ALIVE floor (writers ASG min + readers ASG min + the backup's slot when enabled), never from 'auto': auto computes the majority over every DECLARED replica slot (writer_max + reader_max + backup), so a scale-out ceiling of 10 slots demands 6 acks while the ASG minimums guarantee only ~4 — every insert fails TOO_FEW_LIVE_REPLICAS through churn (e.g. writer_max 5 / max_readers 4 / backup 1 → auto=6 > floor=4). floor/2+1 keeps synchronous-replication durability against the fleet the ASG minimums actually promise, and stays insertable at min capacity."
  type        = number
  default     = 2
}

variable "tenants" {
  description = "Tenant list - each gets an isolated S3 prefix and storage policy"
  type        = list(string)
}

# S3
variable "s3_data_bucket" {
  description = "S3 bucket holding all ClickHouse table data (per-tenant prefixes)"
  type        = string
}

variable "s3_data_prefix" {
  description = "Key prefix for ClickHouse data within the data bucket"
  type        = string
  default     = "data"
}

variable "s3_backup_bucket" {
  description = "S3 bucket for clickhouse-backup"
  type        = string
}

variable "s3_backup_prefix" {
  description = "Key prefix for backups"
  type        = string
  default     = "backups"
}

variable "bootstrap_bucket" {
  description = "S3 bucket for bootstrap scripts/configs"
  type        = string
}

variable "bootstrap_prefix" {
  description = "Key prefix for bootstrap artifacts"
  type        = string
  default     = "bootstrap"
}

# Secrets
variable "default_password_secret_name" {
  description = "Secrets Manager secret name for the default (read-only) user password"
  type        = string
}

variable "admin_password_secret_name" {
  description = "Secrets Manager secret name for the admin user password"
  type        = string
}

variable "writer_password_secret_name" {
  description = "Secrets Manager secret name for the writer user password"
  type        = string
}

variable "tls_certificate_secret_name" {
  description = "Secrets Manager secret name for the TLS certificate chain (null = acm mode)"
  type        = string
  default     = null
}

variable "tls_private_key_secret_name" {
  description = "Secrets Manager secret name for the TLS private key (null = acm mode)"
  type        = string
  default     = null
}

variable "tls_ca_certificate_secret_name" {
  description = "Secrets Manager secret name for the issuer CA (null = acm mode)"
  type        = string
  default     = null
}

# Network restrictions (per-role ClickHouse user network limits)
variable "admin_allowed_networks" {
  description = "Comma-separated CIDRs the admin user may connect from (empty = subnet only)"
  type        = string
  default     = ""
}

variable "writer_allowed_networks" {
  description = "Comma-separated CIDRs the writer user may connect from (empty = subnet only)"
  type        = string
  default     = ""
}

variable "default_user_allowed_networks" {
  description = "Comma-separated CIDRs the default user may connect from (empty = subnet only)"
  type        = string
  default     = ""
}

variable "subnet_cidr" {
  description = "Subnet CIDR for default user network restrictions"
  type        = string
}

variable "subnet_cidrs_multi_az" {
  description = "EXTRA per-AZ nodes subnet CIDRs the multi-AZ opt-in creates (peers in other AZs arrive from these; threaded into the chinit specs' network section so every render allowlists them beside the node's own subnet — a dropped CIDR Code 516s every cross-AZ peer). Empty in the Single-AZ posture."
  type        = list(string)
  default     = []
}

variable "multi_az_enabled" {
  description = "The multi-AZ compute placement opt-in (mirrors multi_az_config.enabled). Rendered into the chinit specs' multi_az section: false renders nothing (the Single-AZ posture, the pre-multi-AZ bundle shape); true renders the placement + the cold_tier it rides on (chinit validates the pairing at spec load)."
  type        = bool
  default     = false
}

variable "multi_az_cold_tier" {
  description = "The shared-blob cold tier the multi-AZ opt-in relies on ('cas' — the Antalya content-addressed pool). Threaded verbatim into the spec; chinit Fatals when the placement is enabled without a tier. Constant 'cas' when multi_az_enabled."
  type        = string
  default     = ""
}

# Reader ordinal claimer
variable "claimer_table_name" {
  description = "DynamoDB table name for reader ordinal claims"
  type        = string
}

# Telemetry — remote log shipping (inert when remote_endpoint is empty)
variable "remote_agent" {
  description = "Remote log agent: \"otel\" (OpenTelemetry Collector) or \"fluentbit\" (Fluent Bit)"
  type        = string
  default     = "otel"
}

variable "remote_endpoint" {
  description = "Remote log destination: OTLP endpoint for otel (e.g. https://collector:4318/v1/logs) or the base URL for fluentbit output"
  type        = string
  default     = ""
}

variable "remote_headers" {
  description = "Comma-separated k=v header pairs sent with each remote export (auth tokens)"
  type        = string
  default     = ""
}

variable "logs_endpoint" {
  description = "Optional remote logs push URL override (e.g. VictoriaLogs 'https://<vl>/insert/opentelemetry/v1/logs')"
  type        = string
  default     = ""
}

variable "metrics_endpoint" {
  description = "Optional remote metrics push URL (VictoriaMetrics '/api/v1/write' Prometheus Remote Write or '/opentelemetry/v1/metrics' OTLP HTTP)"
  type        = string
  default     = ""
}

variable "traces_endpoint" {
  description = "Optional remote traces push URL (VictoriaTraces 'https://<vt>/insert/opentelemetry/v1/traces' or OTLP '/v1/traces')"
  type        = string
  default     = ""
}

variable "metrics_headers" {
  description = "Optional comma-separated k=v header pairs overriding remote_headers for metrics push"
  type        = string
  default     = ""
}

variable "traces_headers" {
  description = "Optional comma-separated k=v header pairs overriding remote_headers for traces push"
  type        = string
  default     = ""
}

variable "scrape_interval" {
  description = "Prometheus scrape interval for otelcol remote metrics push"
  type        = string
  default     = "15s"
}

variable "otel_trace_probability" {
  description = "ClickHouse opentelemetry_start_trace_probability (0.0-1.0) on default and ingest profiles"
  type        = number
  default     = 0
}

variable "otel_keeper_trace_probability" {
  description = "ClickHouse opentelemetry_start_keeper_trace_probability ('auto' or '0'..'1')"
  type        = string
  default     = "auto"
}

variable "otel_trace_processors" {
  description = "Enable ClickHouse per-processor spans (opentelemetry_trace_processors)"
  type        = bool
  default     = false
}

variable "otel_trace_cpu_scheduling" {
  description = "Enable ClickHouse CPU-scheduler spans (opentelemetry_trace_cpu_scheduling)"
  type        = bool
  default     = false
}

variable "otel_span_log_ttl_days" {
  description = "TTL days for system.opentelemetry_span_log"
  type        = number
  default     = 3
}

variable "trace_log_ttl_days" {
  description = "TTL days for trace_log, processors_profile_log, s3queue_log, blob_storage_log, and background_schedule_pool_log"
  type        = number
  default     = 7
}

variable "global_profiler_period_ns" {
  description = "Optional server-level global_profiler_real_time_period_ns and global_profiler_cpu_time_period_ns override (0 = ClickHouse default)"
  type        = number
  default     = 0
}

variable "log_query_threads" {
  description = "Enable log_query_threads on default and ingest profiles"
  type        = bool
  default     = false
}

variable "fluentbit_output" {
  description = "Fluent Bit output flavor (remote_agent = \"fluentbit\"): \"otlp\" = opentelemetry output; \"http\" = generic HTTP POST (e.g. ClickHouse INSERT endpoint)"
  type        = string
  default     = "otlp"
}




# Backup
variable "backup_retention_days" {
  description = "Remote backup retention"
  type        = number
  default     = 7
}

variable "backup_compression_format" {
  description = "clickhouse-backup compression format"
  type        = string
  default     = "tar"
}

variable "backup_schedule" {
  description = "Cron expression (e.g. '0 2 * * *') or systemd OnCalendar expression ('*-*-* 02:00:00') for clickhouse-backup and the on-demand backup replica wake timer."
  type        = string
  default     = "0 2 * * *"
}

variable "backup_on_demand" {
  description = "Spin up the dedicated backup replica on backup_schedule (via chinit's ordinal-0 writer coordinator) and stop it automatically once the backup pass completes."
  type        = bool
  default     = false
}


variable "tags" {
  description = "Tags for module resources"
  type        = map(string)
  default     = {}
}

variable "cache_disk_size_gb" {
  description = "Provisioned S3 cache disk size (GB) — storage.xml cache max_size derives from it and must not exceed the volume capacity"
  type        = number
  default     = 500
}

variable "nlb_subnet_cidr" {
  description = "CIDR of the subnet where the NLBs live. NLBs do NOT preserve client source IP - forwarded requests arrive from this subnet, so every ClickHouse user's <networks> must include it (mirrors the SG rules)."
  type        = string
}


# Content-addressed storage (Altinity Antalya CAS) for the cold tier.
# Enabling renders the `object_storage` CAS disks + `tiered_cas` policy in
# storage.xml, plus per-tenant `cas_tenant_<tenant>` policies. The ClickHouse
# build must be an Altinity Antalya build (metadata_type=cas does not exist
# upstream); the AMI must come from packer with build_flavor = "antalya".
variable "cas_enabled" {
  description = "Enable the CAS (content-addressed storage) cold tier (Altinity Antalya builds only). Renders `cas`/`cas_cache` disks, a `tiered_cas` policy and per-tenant `cas_tenant_<tenant>` cold tiers into storage.xml. Requires the data bucket to have versioning and object lock DISABLED (CAS refuses to mount otherwise). See the CAS runbook."
  type        = bool
  default     = false
}

variable "cas_prefix" {
  description = "Key prefix (inside the data bucket) for the shared CAS pools. One pool per tenant (`<cas_prefix>/<tenant>/`) plus the shared `tiered_cas` pool (`<cas_prefix>/tiered/`). Must NOT overlap the per-replica prefixes under the data prefix; parts written there are content-addressed and GC'd by CAS."
  type        = string
  default     = "cas"

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9/_-]{0,126}[a-z0-9]$", var.cas_prefix)) && !can(regex("(^|/)\\.\\.?(/|$)", var.cas_prefix))
    error_message = "cas_prefix must be a relative S3 key prefix of [a-z0-9/_-], with no leading/trailing slash and no dot segments."
  }
}

variable "cas_cache_max_size_bytes" {
  description = "max_size for each CAS cache wrapper disk (raw bytes). 0 = derive from cache_disk_size_gb: floor(0.9 * cache_disk_size_gb / (tenant_count + 1)) GiB, split across the tiered pool and one wrapper per tenant. The CAS pool itself is never cached wholesale; this bounds the local mirror of hot CAS reads."
  type        = number
  default     = 0

  validation {
    condition     = var.cas_cache_max_size_bytes >= 0
    error_message = "cas_cache_max_size_bytes must be >= 0 (0 = derive from cache_disk_size_gb)."
  }
}

variable "cold_move_ttl_days" {
  description = "Days hot parts stay on the hot volume before TTL-moving to VOLUME 'cold' on default.events (1d default when CAS is enabled; 7d non-CAS)."
  type        = number
  default     = 7

  validation {
    condition     = var.cold_move_ttl_days >= 1 && var.cold_move_ttl_days <= 30
    error_message = "cold_move_ttl_days must be between 1 and 30."
  }
}

variable "nvme_hot_enabled" {
  description = "Emit the local nvme_hot disk (/var/lib/clickhouse/disks/hot/, 10 TiB default keep_free_space_bytes lowered by chinit on >=400 GiB NVMe writers) and render CAS hot volumes with both nvme_hot and default under least_used load balancing."
  type        = bool
  default     = false
}

variable "backup_replica_enabled" {
  description = "Whether the cluster provisions the dedicated backup replica. Drives the backup's CR slot: replica_count gains the +1 and every role spec carries the backup hostname (inventory membership — remote_servers on ALL nodes list the backup; a backup omitted from the specs is a node nobody's spec knows exists)."
  type        = bool
  default     = false
}


# ----------------------------------------------------------------------------
# Vertical autoscaling (the chinit autoscale coordinator's spec fields)
# ----------------------------------------------------------------------------
variable "autoscaling_enabled" {
  description = "Render the autoscaling ladders + fleet identity into the chinit role specs (the `autoscale` timer's contract). false = the timers install but the subcommand skips with a logged reason."
  type        = bool
  default     = false
}

variable "keeper_type_ladder" {
  description = "Ascending keeper instance types for the vertical ladder."
  type        = list(string)
  default     = []
}

variable "keeper_min_type" {
  description = "Keeper ladder floor."
  type        = string
  default     = ""
}

variable "keeper_max_type" {
  description = "Keeper ladder ceiling (the runaway cap)."
  type        = string
  default     = ""
}

variable "writer_type_ladder" {
  description = "Ascending writer instance types for the vertical ladder (native ASG horizontal applies regardless)."
  type        = list(string)
  default     = []
}

variable "writer_min_type" {
  description = "Writer ladder floor."
  type        = string
  default     = ""
}

variable "writer_max_type" {
  description = "Writer ladder ceiling."
  type        = string
  default     = ""
}

variable "writers_asg_name" {
  description = "The writers ASG name the ordinal-0 holder rolls on a vertical step."
  type        = string
  default     = ""
}

variable "writers_launch_template_id" {
  description = "The writers launch-template id the ordinal-0 holder versions on a vertical step."
  type        = string
  default     = ""
}
