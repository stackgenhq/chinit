# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# chinit contract module (AWS) — the ONLY renderer of the chinit interface
# ============================================================================
# What this module IS: the open-source home of the chinit↔Terraform
# contract. It renders the role specs (chinit-<role>.yaml), computes the
# SHA-gated bundle digest (byte-identical to what chinit recomputes at
# boot), uploads the bundle to S3, and renders the per-role user-data
# wrappers. Any consumer — the all-in-one modules in this repo, or a
# stack extending the ecosystem — calls THIS and adds
# its own resources on top.
#
# What this module is NOT: a cluster. No compute, no IAM, no DNS, no
# secrets — the caller owns the cloud resources and passes their ids in.
# ============================================================================

# ----------------------------------------------------------------------------
# Inputs: cluster identity (rendered into every spec)
# ----------------------------------------------------------------------------

variable "cluster_name" {
  description = "ClickHouse cluster name (rendered into the chinit specs, the CHI spec, DNS names)."
  type        = string
}

variable "region" {
  description = "AWS region (pins chinit's SDK clients and the S3 endpoint env)."
  type        = string
}

variable "dns_zone_name" {
  description = "Private DNS zone name for node discovery (keeper FQDNs render from it)."
  type        = string
}

variable "timezone" {
  description = "Node timezone (written into the server's EnvironmentFile as CLICKHOUSE_TZ)."
  type        = string
  default     = "UTC"
}

# ----------------------------------------------------------------------------
# Inputs: identity ceilings (ASG maxima — never live counts)
# ----------------------------------------------------------------------------

variable "writer_max" {
  description = "Writer ordinal claim ceiling = the writers ASG max_size. Writers claim writer-N in [0, writer_max)."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_max >= 1
    error_message = "writer_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

# ----------------------------------------------------------------------------
# Vertical autoscaling (the chinit `autoscale` coordinator's inputs)
# ----------------------------------------------------------------------------
# The ladders and the writers' fleet identity render into the role
# specs — the hash-pinned config bundle every node re-fetches at boot
# and the boot --refresh path converges live nodes to.
variable "autoscaling_enabled" {
  description = "Render the autoscaling section (ladders + fleet identity) into the role specs. false = the section renders empty and the chinit autoscale timer no-ops (native ASG policies own horizontal regardless)."
  type        = bool
  default     = false
}

variable "keeper_type_ladder" {
  description = "Ascending keeper instance types for the vertical ladder. Empty = keepers do not vertically scale."
  type        = list(string)
  default     = []
}

variable "keeper_min_type" {
  description = "Keeper ladder floor (a step below is refused)."
  type        = string
  default     = ""

  validation {
    condition     = var.keeper_min_type == "" || length(var.keeper_type_ladder) == 0 || contains(var.keeper_type_ladder, var.keeper_min_type)
    error_message = "keeper_min_type must appear in keeper_type_ladder when both are set."
  }
}

variable "keeper_max_type" {
  description = "Keeper ladder ceiling (a step past is refused — the runaway cap)."
  type        = string
  default     = ""

  validation {
    condition = (
      var.keeper_max_type == "" ||
      length(var.keeper_type_ladder) == 0 ||
      (
        contains(var.keeper_type_ladder, var.keeper_max_type) &&
        (var.keeper_min_type == "" || try(index(var.keeper_type_ladder, var.keeper_min_type) <= index(var.keeper_type_ladder, var.keeper_max_type), false))
      )
    )
    error_message = "keeper_max_type must appear in keeper_type_ladder at or after keeper_min_type."
  }
}

variable "writer_type_ladder" {
  description = "Ascending writer instance types for the vertical ladder. Empty = writers do not vertically scale (native ASG horizontal still applies)."
  type        = list(string)
  default     = []

  validation {
    condition = alltrue([
      for t in var.writer_type_ladder :
      !can(regex("(\\.(nano|micro|small|medium)$|^c[0-9]+[a-z]*\\.large$)", t))
    ])
    error_message = "writer_type_ladder entries must provide at least 8 GiB RAM (no *.nano/micro/small/medium or c*.large)."
  }
}

variable "writer_min_type" {
  description = "Writer ladder floor."
  type        = string
  default     = ""

  validation {
    condition     = var.writer_min_type == "" || length(var.writer_type_ladder) == 0 || contains(var.writer_type_ladder, var.writer_min_type)
    error_message = "writer_min_type must appear in writer_type_ladder when both are set."
  }
}

variable "writer_max_type" {
  description = "Writer ladder ceiling."
  type        = string
  default     = ""

  validation {
    condition = (
      var.writer_max_type == "" ||
      length(var.writer_type_ladder) == 0 ||
      (
        contains(var.writer_type_ladder, var.writer_max_type) &&
        (var.writer_min_type == "" || try(index(var.writer_type_ladder, var.writer_min_type) <= index(var.writer_type_ladder, var.writer_max_type), false))
      )
    )
    error_message = "writer_max_type must appear in writer_type_ladder at or after writer_min_type."
  }
}

variable "writers_asg_name" {
  description = "The writers ASG name the ordinal-0 holder rolls on a vertical step (the chinit autoscale coordinator's fleet identity)."
  type        = string
  default     = ""
}

variable "writer_hostnames" {
  description = "Reserved writer ordinal slots ([writer-0..writer-{writer_max-1}]) — the static-path compat field the spec renders as writer_nodes (length must equal writer_max; live membership is lease-driven)."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.writer_hostnames) == 0 || length(var.writer_hostnames) == var.writer_max
    error_message = "writer_hostnames must be empty or exactly writer_max entries (it is the reserved slot ceiling, not a live count)."
  }
}

variable "reader_max" {
  description = "Reader ordinal claim ceiling = the readers ASG max_size."
  type        = number
  default     = 6

  validation {
    condition     = var.reader_max >= 1
    error_message = "reader_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

variable "reader_count" {
  description = "Reader baseline (render-time inventory; the claim ceiling is reader_max)."
  type        = number
  default     = 2
}

variable "keeper_hostnames" {
  description = "The keeper (Raft) inventory — keeper-N names, in order. Every role's spec carries the list."
  type        = list(string)
}

variable "enable_backup" {
  description = "The cluster has a dedicated backup replica (backup-0): every spec carries its identity (host inventories list it), and the backup's own spec renders."
  type        = bool
  default     = true
}

# ----------------------------------------------------------------------------
# Inputs: cloud resources the specs point at (ids — never values)
# ----------------------------------------------------------------------------

variable "bootstrap_bucket" {
  description = "S3 bucket that stores the config bundle (this module uploads it)."
  type        = string
}

variable "bootstrap_prefix" {
  description = "Bundle key prefix inside the bucket."
  type        = string
  default     = "bootstrap"
}

variable "data_bucket" {
  description = "S3 bucket holding ClickHouse table data (the cold tier; storage.xml's endpoint env root)."
  type        = string
}

variable "data_prefix" {
  description = "Key prefix inside the data bucket for table data."
  type        = string
  default     = "data"
}

variable "backup_bucket" {
  description = "S3 bucket for clickhouse-backup (empty = backups disabled)."
  type        = string
  default     = ""
}

variable "backup_prefix" {
  description = "Key prefix for backups inside the backup bucket."
  type        = string
  default     = "backups"
}

variable "backup_retention_days" {
  description = "clickhouse-backup remote retention (days)."
  type        = number
  default     = 7
}

variable "backup_compression_format" {
  description = "clickhouse-backup archive format."
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

variable "claimer_table_name" {
  description = "DynamoDB ordinal-lease table name (writer-N/reader-N claims; the caller creates the table and passes its name)."
  type        = string
  default     = ""
}

variable "subnet_cidr" {
  description = "The nodes subnet CIDR (renders into the spec's network section — the ClickHouse <networks> entries)."
  type        = string
}

variable "subnet_cidrs_multi_az" {
  description = "EXTRA per-AZ nodes subnet CIDRs the multi-AZ opt-in creates (peers in other AZs arrive from these; each render allowlists them beside the node's own subnet_cidr). Empty in the Single-AZ posture (multi_az_enabled = false)."
  type        = list(string)
  default     = []
}

variable "nlb_subnet_cidr" {
  description = "Load-balancer subnet CIDR (NLBs do not preserve client source IPs: forwarded traffic arrives from here, so every user's <networks> carries it alongside the SG rules)."
  type        = string
  default     = ""
}

variable "admin_allowed_networks" {
  description = "Comma-separated CIDRs the admin user may connect from (empty = subnet only)."
  type        = string
  default     = ""
}

variable "writer_allowed_networks" {
  description = "Comma-separated CIDRs the writer user may connect from (empty = subnet only)."
  type        = string
  default     = ""
}

variable "default_user_allowed_networks" {
  description = "Comma-separated CIDRs the default user may connect from (empty = subnet only)."
  type        = string
  default     = ""
}

# ----------------------------------------------------------------------------
# Inputs: secret NAMES (never values)
# ----------------------------------------------------------------------------

variable "default_password_secret_name" {
  description = "Secrets Manager secret name for the default (read-only) user password."
  type        = string
}

variable "admin_password_secret_name" {
  description = "Secrets Manager secret name for the admin user password."
  type        = string
}

variable "writer_password_secret_name" {
  description = "Secrets Manager secret name for the writer user password."
  type        = string
}

variable "tls_certificate_secret_name" {
  description = "Secrets Manager secret name for the TLS certificate chain (empty = self-signed interserver mode)."
  type        = string
  default     = ""
}

variable "tls_private_key_secret_name" {
  description = "Secrets Manager secret name for the TLS private key (empty = self-signed interserver mode)."
  type        = string
  default     = ""
}

variable "tls_ca_certificate_secret_name" {
  description = "Secrets Manager secret name for the issuer CA (empty = self-signed interserver mode)."
  type        = string
  default     = ""
}

# ----------------------------------------------------------------------------
# Inputs: disks (labels — mount by LABEL, never device path)
# ----------------------------------------------------------------------------

variable "metadata_disk_label" {
  description = "Filesystem label of the metadata (hot-tier) disk."
  type        = string
  default     = "ch-metadata"
}

variable "cache_disk_label" {
  description = "Filesystem label of the S3 read-cache disk."
  type        = string
  default     = "ch-cache"
}

variable "keeper_disk_label" {
  description = "Filesystem label of the keeper raft-log disk."
  type        = string
  default     = "ch-keeper"
}

variable "cache_disk_size_gb" {
  description = "Provisioned cache-disk size (GB). storage.xml's cache <max_size> derives from it at 0.9x and must not exceed the volume (ClickHouse refuses to start otherwise — Code 36; EBS sizes are decimal GB, max_size takes raw bytes)."
  type        = number
  default     = 128
}

# ----------------------------------------------------------------------------
# Inputs: log shipping (inert when the endpoint is empty)
# ----------------------------------------------------------------------------

variable "log_shipping_agent" {
  description = "Remote log agent: otel | fluentbit."
  type        = string
  default     = "otel"
}

# ----------------------------------------------------------------------------
# Inputs: ClickHouse logging configuration
# ----------------------------------------------------------------------------

variable "ch_log_level" {
  description = "External sink <level> in server-logger.xml (rotating file shipped by the log agent). Default `warning`: keeps remote log ingestion focused on warnings and errors while verbose detail is preserved in system.text_log via ch_text_log_*. Poco levels apply: trace|debug|information|warning|error|critical."
  type        = string
  default     = "warning"
}

variable "ch_text_log_level" {
  description = "system.text_log <level> in server-text-log.xml — the in-cluster verbose log (ZSTD-compressed, TTL-dropped). Sits at or above ch_log_level (the root logger rises to the max of both, Loggers.cpp). Set `warning` to keep only warnings in the table too."
  type        = string
  default     = "information"
}

variable "ch_text_log_ttl_days" {
  description = "system.text_log TTL days (engine `TTL event_date + INTERVAL N DAY DELETE`). 0 = forever (never recommended — a log table without a TTL is unbounded cost)."
  type        = number
  default     = 7

  validation {
    condition     = var.ch_text_log_ttl_days >= 0
    error_message = "ch_text_log_ttl_days must be >= 0 (0 keeps rows forever)."
  }
}

variable "log_shipping_endpoint" {
  description = "OTLP/HTTP endpoint (empty = remote shipping disabled)."
  type        = string
  default     = ""
}

variable "log_shipping_headers" {
  description = "Comma-separated k=v header pairs sent with each remote export."
  type        = string
  default     = ""
}

variable "fluentbit_output" {
  description = "Fluent Bit output flavor when agent=fluentbit: otlp | http."
  type        = string
  default     = "otlp"
}

# ----------------------------------------------------------------------------
# Inputs: the fleet policy (disruption budget + lifecycle hooks)
# ----------------------------------------------------------------------------

variable "min_healthy_writers" {
  description = "The voluntary-disruption floor for writers (the PDB equivalent): a refresh/rollout waits when fewer healthy writers would remain. 0 = the safe default (1)."
  type        = number
  default     = 1
}

variable "min_healthy_readers" {
  description = "The voluntary-disruption floor for readers. 0 = readers are disposable (the ASG owns their horizontal scaling)."
  type        = number
  default     = 0
}

variable "lifecycle_hooks" {
  description = "Lifecycle hook commands at the boot/drain boundaries (the operator's spec.reconcile.hooks parity). List of {event, name, command, timeout_seconds, critical}."
  type = list(object({
    event           = string
    name            = string
    command         = string
    timeout_seconds = optional(number, 0)
    critical        = optional(bool, false)
  }))
  default = []
}

# ----------------------------------------------------------------------------
# EXTENSION HOOKS — how consuming stacks extend the bundle
# ----------------------------------------------------------------------------

variable "config_files" {
  description = "EXTRA bundle members beyond the module's static XMLs (extension hook). Map of bundle key → content: each entry uploads at <prefix>/<key> and joins the SHA gate digest. KEYS MUST BE AMONG chinit's hashed bundle members (configs/network.xml, configs/ssl.xml, configs/storage.xml, configs/bootstrap-ddl.sql) — the gate digest chinit recomputes covers EXACTLY that closed list plus chi.yaml, so an unknown key would upload but silently desync the digest and Fatal every boot. Override the module's defaults by reusing their keys (e.g. configs/storage.xml for a caller-rendered storage layout)."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([
      for k in keys(var.config_files) :
      contains(["configs/network.xml", "configs/ssl.xml", "configs/storage.xml", "configs/bootstrap-ddl.sql"], k)
    ])
    error_message = "config_files keys must be chinit's hashed bundle members: configs/network.xml, configs/ssl.xml, configs/storage.xml, configs/bootstrap-ddl.sql (chi.yaml is pinned separately via chi_spec)."
  }
}

variable "chi_spec" {
  description = "Caller-rendered CHI (cluster) spec — the extension hook for the consumer's own settings/profiles/files. When set it REPLACES the module's default cluster topology render; it must keep the load-bearing invariants documented in this module's README (the named shard, replica_count ceiling arithmetic, the keeper ensemble). Omit it to use the default render."
  type        = string
  default     = null
}

variable "keeper_spec" {
  description = "Caller-rendered CHK (keeper) spec — same contract as chi_spec for the keeper fleet."
  type        = string
  default     = null
}

variable "wrapper_extra" {
  description = "Extra shell appended to every role's user-data wrapper BEFORE `chinit boot` runs (extension hook: stage artifacts, run stack-specific pre-flight). Must not exec a replacement boot; keep it small — the 16 KiB EC2 user-data cap."
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags applied to the bundle objects."
  type        = map(string)
  default     = {}
}

# ----------------------------------------------------------------------------
# Inputs: compute placement (the cross-AZ part-traffic gate)
# ----------------------------------------------------------------------------

variable "multi_az_enabled" {
  description = "Opt-in multi-AZ compute placement: writers/readers distributed across AZs instead of pinned to one AZ per shard. Default FALSE — Single-AZ is the cost posture (sub-ms peer RTT, $0 cross-AZ part traffic; AZ failure is an ASG re-launch, data is already 3-AZ durable in S3). Enabling on stock ReplicatedMergeTree bills AWS's $0.02/GB cross-AZ transfer on every peer part fetch, so the opt-in requires multi_az_cold_tier and is only sane under a shared-blob tier (Antalya CAS)."
  type        = bool
  default     = false
}

variable "multi_az_cold_tier" {
  description = "The shared-blob cold tier the multi-AZ opt-in rides on ('cas' = Altinity Antalya CAS: part blobs are shared through S3 via content addressing, so cross-AZ re-reads cost $0 while a $0.02/GB peer-copy tax would apply to plain ReplicatedMergeTree). Required (non-empty) when multi_az_enabled = true."
  type        = string
  default     = ""

  validation {
    condition     = var.multi_az_cold_tier == "" || contains(["cas"], var.multi_az_cold_tier)
    error_message = "multi_az_cold_tier must be \"cas\" (the Altinity Antalya shared-blob tier) or empty."
  }
}

# ----------------------------------------------------------------------------
# The bundle digest — the contract with chinit's FetchBundle
# ----------------------------------------------------------------------------

locals {
  # The static XMLs every install requires (chinit's fetch validates +
  # installs exactly these three; missing = Fatal at boot).
  static_config_files = {
    "configs/network.xml" = file("${path.module}/templates/network.xml")
    "configs/ssl.xml"     = file("${path.module}/templates/ssl.xml")
    "configs/storage.xml" = templatefile("${path.module}/templates/storage.xml", {
      s3_data_prefix       = var.data_prefix
      cache_max_size_bytes = floor(var.cache_disk_size_gb * 0.9) * 1024 * 1024 * 1024
    })
    "configs/bootstrap-ddl.sql" = file("${path.module}/templates/bootstrap-ddl.sql")
  }

  # The bundle the module ships: statics + the caller's extra files.
  # The digest key set MUST equal chinit's hashed bundleFiles members
  # (configs/*.xml + bootstrap-ddl + chi.yaml): chinit recomputes the
  # digest over EXACTLY those keys at boot.
  config_files_all = merge(local.static_config_files, var.config_files)

  default_chi_spec = templatefile("${path.module}/templates/chi.yaml.tftpl", {
    cluster_name                 = var.cluster_name
    replica_count                = var.writer_max + var.reader_max + (var.enable_backup ? 1 : 0)
    keepers                      = [for h in var.keeper_hostnames : "${h}.${var.dns_zone_name}"]
    default_password_secret_name = var.default_password_secret_name
    admin_password_secret_name   = var.admin_password_secret_name
    writer_password_secret_name  = var.writer_password_secret_name
    log_level                    = var.ch_log_level
    text_log_level               = var.ch_text_log_level
    text_log_ttl_line            = var.ch_text_log_ttl_days > 0 ? "event_date + INTERVAL ${var.ch_text_log_ttl_days} DAY DELETE" : ""
  })

  chi_spec = var.chi_spec != null ? var.chi_spec : local.default_chi_spec

  default_keeper_spec = templatefile("${path.module}/templates/keeper.yaml.tftpl", {
    cluster_name = var.cluster_name
    keeper_count = length(var.keeper_hostnames)
  })

  keeper_spec = var.keeper_spec != null ? var.keeper_spec : local.default_keeper_spec

  # The SHA256 gate digest: sha256(jsonencode(merge(config_files_all,
  # {"chi.yaml": spec}))) — the EXACT shape chinit's FetchBundle
  # recomputes (terraformPairsDigest). The specs embed this hash; they
  # are therefore excluded from the digest (self-reference cycle).
  config_files_hash = sha256(jsonencode(merge(local.config_files_all, {
    "chi.yaml" = local.chi_spec
  })))

  # --------------------------------------------------------------------------
  # chinit role specs
  # --------------------------------------------------------------------------
  # hooks_yaml: the lifecycle hook list rendered as the spec's
  # hooks.items YAML (the template interpolates the block verbatim; an
  # empty list renders the empty section).
  hooks_yaml = join("\n", [for h in var.lifecycle_hooks : format(
    "    - event: %s\n      name: %s\n      command: %s\n      timeout_seconds: %d\n      critical: %t",
    h.event, h.name, h.command, h.timeout_seconds, h.critical
  )])

  chinit_spec_common = {
    cluster_name  = var.cluster_name
    dns_zone_name = var.dns_zone_name
    region        = var.region
    # The fleet policy: the disruption floors + the lifecycle hook
    # list, rendered into every role's spec (the boundaries run on
    # every node).
    min_healthy_writers = var.min_healthy_writers
    min_healthy_readers = var.min_healthy_readers
    hooks_yaml          = local.hooks_yaml
    bundle_bucket       = var.bootstrap_bucket
    bundle_prefix       = var.bootstrap_prefix
    config_hash         = local.config_files_hash
    s3_data_bucket      = var.data_bucket
    timezone            = var.timezone
    keeper_hostnames    = var.keeper_hostnames
    # Static-writer compat fields (writer_nodes/writer_count): lease
    # binaries ignore them in slot arithmetic; pre-lease binaries on
    # the static path still read them.
    writer_hostnames               = length(var.writer_hostnames) > 0 ? var.writer_hostnames : [for i in range(var.writer_max) : "writer-${i}"]
    writer_count                   = var.writer_max
    writer_nodes_list              = join(", ", [for n in(length(var.writer_hostnames) > 0 ? var.writer_hostnames : [for i in range(var.writer_max) : "writer-${i}"]) : "\"${n}\""])
    keeper_nodes_list              = join(", ", [for n in var.keeper_hostnames : "\"${n}\""])
    metadata_disk_label            = var.metadata_disk_label
    cache_disk_label               = var.cache_disk_label
    keeper_disk_label              = var.keeper_disk_label
    claimer_table_name             = var.claimer_table_name
    writer_max                     = var.writer_max
    reader_max                     = var.reader_max
    reader_count                   = var.reader_count
    default_password_secret_name   = var.default_password_secret_name
    admin_password_secret_name     = var.admin_password_secret_name
    writer_password_secret_name    = var.writer_password_secret_name
    tls_certificate_secret_name    = var.tls_certificate_secret_name
    tls_private_key_secret_name    = var.tls_private_key_secret_name
    tls_ca_certificate_secret_name = var.tls_ca_certificate_secret_name
    subnet_cidr                    = var.subnet_cidr
    subnet_cidrs_multi_az          = var.multi_az_enabled ? join(", ", [for c in var.subnet_cidrs_multi_az : "\"${c}\""]) : ""
    nlb_subnet_cidr                = var.nlb_subnet_cidr
    admin_networks_list            = join(", ", [for c in compact([for c in split(",", var.admin_allowed_networks) : trimspace(c)]) : "\"${c}\""])
    writer_networks_list           = join(", ", [for c in compact([for c in split(",", var.writer_allowed_networks) : trimspace(c)]) : "\"${c}\""])
    default_networks_list          = join(", ", [for c in compact([for c in split(",", var.default_user_allowed_networks) : trimspace(c)]) : "\"${c}\""])
    backup_bucket                  = var.backup_bucket
    backup_prefix                  = var.backup_prefix
    backup_retention_days          = var.backup_retention_days
    backup_compression_format      = var.backup_compression_format
    backup_schedule                = var.backup_schedule
    backup_on_demand               = var.enable_backup && var.backup_on_demand ? "true" : "false"
    is_backup_replica              = "false"
    backup_replica_hostname        = var.enable_backup ? "backup-0" : ""
    log_shipping_agent             = var.log_shipping_agent
    log_shipping_endpoint          = var.log_shipping_endpoint
    log_shipping_headers           = var.log_shipping_headers
    fluentbit_output               = var.fluentbit_output
    # Vertical autoscaling (the `autoscale` coordinator's spec
    # projection): empty ladders render as empty lists and the
    # subcommand skips with a logged reason (autoscaling_enabled is
    # the module-level gate; the ladders themselves are the config).
    keeper_ladder_list = var.autoscaling_enabled ? join(", ", [for t in var.keeper_type_ladder : "\"${t}\""]) : ""
    keeper_min_type    = var.autoscaling_enabled ? var.keeper_min_type : ""
    keeper_max_type    = var.autoscaling_enabled ? var.keeper_max_type : ""
    writer_ladder_list = var.autoscaling_enabled ? join(", ", [for t in var.writer_type_ladder : "\"${t}\""]) : ""
    writer_min_type    = var.autoscaling_enabled ? var.writer_min_type : ""
    writer_max_type    = var.autoscaling_enabled ? var.writer_max_type : ""
    writers_asg_name   = var.autoscaling_enabled ? var.writers_asg_name : ""
    # Multi-AZ placement (the cross-AZ part-traffic contract): the
    # section renders EMPTY when the opt-in is off — every spec keeps
    # the pre-placement shape, and chinit's zero-value MultiAZ parses
    # as the single-AZ posture. Enabled renders the tier name the
    # placement rides on (chinit validates the pairing at load).
    multi_az_enabled   = var.multi_az_enabled ? "true" : ""
    multi_az_cold_tier = var.multi_az_enabled ? var.multi_az_cold_tier : ""
  }

  chinit_specs = {
    writer = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role          = "writer"
      identity_mode = "lease"
    }))
    reader = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role          = "reader"
      identity_mode = "lease"
    }))
    keeper = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role          = "keeper"
      identity_mode = "static"
    }))
    backup = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role              = "backup"
      identity_mode     = "static"
      is_backup_replica = "true"
    }))
  }

  uploaded_specs = var.enable_backup ? local.chinit_specs : {
    for role, spec in local.chinit_specs : role => spec if role != "backup"
  }

  # The per-role user-data wrappers (stage-0). The caller injects them
  # into its own launch templates / EC2 user_data.
  bootstrap_script = {
    for role, _spec in local.chinit_specs : role => templatefile("${path.module}/templates/user-data.sh.tftpl", {
      chinit_spec_key = "chinit-${role}.yaml"
      bundle_bucket   = var.bootstrap_bucket
      bundle_prefix   = var.bootstrap_prefix
      wrapper_extra   = var.wrapper_extra
    })
  }

  common_tags = merge(var.tags, { Cluster = var.cluster_name })
}

# ----------------------------------------------------------------------------
# Bundle uploads — the objects chinit's FetchBundle downloads
# ----------------------------------------------------------------------------

resource "aws_s3_object" "bundle_configs" {
  for_each = local.config_files_all

  bucket       = var.bootstrap_bucket
  key          = "${var.bootstrap_prefix}/${each.key}"
  content      = each.value
  content_type = can(regex("\\.xml$", each.key)) ? "application/xml" : (can(regex("\\.sql$", each.key)) ? "application/sql" : "text/plain")
  etag         = md5(each.value)

  tags = local.common_tags
}

resource "aws_s3_object" "chi_spec" {
  bucket       = var.bootstrap_bucket
  key          = "${var.bootstrap_prefix}/chi.yaml"
  content      = local.chi_spec
  content_type = "application/yaml"
  etag         = md5(local.chi_spec)

  tags = local.common_tags
}

# keeper.yaml: fetched by keepers, EXCLUDED from the digest (Terraform's
# hash does not cover it, so the digest stays role-independent — the Go
# fetcher's hashed:false member).
resource "aws_s3_object" "keeper_spec" {
  bucket       = var.bootstrap_bucket
  key          = "${var.bootstrap_prefix}/keeper.yaml"
  content      = local.keeper_spec
  content_type = "application/yaml"
  etag         = md5(local.keeper_spec)

  tags = local.common_tags
}

resource "aws_s3_object" "chinit_specs" {
  for_each = local.uploaded_specs

  bucket       = var.bootstrap_bucket
  key          = "${var.bootstrap_prefix}/chinit-${each.key}.yaml"
  content      = each.value
  content_type = "application/yaml"
  etag         = md5(each.value)

  tags = local.common_tags
}

# ----------------------------------------------------------------------------
# Outputs — what the caller wires into compute
# ----------------------------------------------------------------------------

output "bootstrap_script" {
  description = "Role → rendered user-data wrapper (seed the role's spec, exec chinit boot). Inject into each role's launch template / instance user_data."
  value       = local.bootstrap_script
}

output "config_files_hash" {
  description = "The SHA256 gate digest. chinit recomputes it at boot; a mismatch is a Fatal error pointing at stale tfvars or an overwritten bucket."
  value       = local.config_files_hash
}

output "chinit_specs" {
  description = "Role → rendered chinit node spec (chinit-<role>.yaml; uploaded to the bundle)."
  value       = local.chinit_specs
  sensitive   = true
}

output "bundle_keys" {
  description = "Every bundle key the module uploaded (the caller's IAM read grants scope to <prefix>/* already; useful for audits)."
  value       = [for k, _ in local.config_files_all : "${var.bootstrap_prefix}/${k}"]
}
