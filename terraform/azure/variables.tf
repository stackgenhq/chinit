# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Inputs — chinit cluster module (Azure)
# ============================================================================
# Mirrors the AWS module's surface: BootSpec fields, role sizing knobs,
# and bring-your-own network hooks. The Azure-specific deltas: resource
# group + subscription pins (chinit's ARM scope env), and the gallery
# image reference for the golden VM image.
# ============================================================================

variable "cluster_name" {
  description = "ClickHouse cluster name. Renders into the chinit specs (cluster_name), the CHI spec, DNS names and every resource name."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,18}[a-z0-9]$", var.cluster_name))
    error_message = "cluster_name must be 3-20 chars of [a-z0-9-], no leading/trailing hyphen (it lands in DNS names, blob keys, and the storage-account name — 24-char global-unique limit)."
  }
}

variable "location" {
  description = "Azure region (e.g. westus2)."
  type        = string
}

variable "resource_group_name" {
  description = "Resource group for every resource the module creates. Also pins chinit's ARM scope (the wrapper exports CHINIT_AZURE_RESOURCE_GROUP from it)."
  type        = string
}

variable "subscription_id" {
  description = "The subscription id the resource group lives in. chinit's ARM scope env (CHINIT_AZURE_SUBSCRIPTION_ID) — Private DNS record management and the volume grower build resource ids from it."
  type        = string
}

variable "create_network" {
  description = "Create the network (VNet + subnet + Private DNS zone link). Set to false to plug into an existing VNet via vnet_id/subnet_id."
  type        = bool
  default     = true
}

variable "vnet_id" {
  description = "Existing VNet id (create_network = false). The Private DNS zone links to it."
  type        = string
  default     = null
}

variable "subnet_id" {
  description = "Existing subnet id (create_network = false). Every node launches here; keepers/backup take static IPs at the +20/+30 offsets inside it."
  type        = string
  default     = null
}

variable "subnet_cidr" {
  description = "The subnet's IPv4 CIDR (required in BOTH modes: renders into the chinit spec's network section and allocates the static keeper/backup IPs)."
  type        = string

  validation {
    condition     = can(cidrnetmask(var.subnet_cidr))
    error_message = "subnet_cidr must be a valid IPv4 CIDR."
  }
}

variable "client_cidrs" {
  description = "CIDRs allowed to reach ClickHouse client ports (8123/9000/8443/9440) on writers and readers. Empty = no client ingress."
  type        = list(string)
  default     = []
}

# ----------------------------------------------------------------------------
# Identity ceilings (VMSS maxima — never live counts)
# ----------------------------------------------------------------------------

variable "writer_max" {
  description = "Writers VMSS max = the writer ordinal claim ceiling. Writers claim writer-N in [0, writer_max) from the claims table at boot."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_max >= 1
    error_message = "writer_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

variable "writer_desired" {
  description = "Writers VMSS instance count at apply time."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_desired >= 1 && var.writer_desired <= var.writer_max
    error_message = "writer_desired must be within [1, writer_max]."
  }
}

variable "reader_max" {
  description = "Readers VMSS max = the reader ordinal claim ceiling (the same contract as writer_max)."
  type        = number
  default     = 6

  validation {
    condition     = var.reader_max >= 1
    error_message = "reader_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

variable "reader_desired" {
  description = "Readers VMSS instance count at apply time."
  type        = number
  default     = 2

  validation {
    condition     = var.reader_desired >= 0 && var.reader_desired <= var.reader_max
    error_message = "reader_desired must be within [0, reader_max]."
  }
}

variable "reader_spot" {
  description = "Readers VMSS uses Spot VMs with an eviction policy of Delete (chinit's scheduled-events watcher drains on the Preempt notice; the lease TTL releases the ordinal)."
  type        = bool
  default     = true
}

variable "keeper_count" {
  description = "Keeper (Raft) fleet size. Must be odd; keepers are fixed VMs with static IPs at the +20 octet offsets."
  type        = number
  default     = 3

  validation {
    condition     = var.keeper_count % 2 == 1 && var.keeper_count >= 1 && var.keeper_count <= 10
    error_message = "keeper_count must be odd (Raft quorum) and within [1, 10] (the +20..+29 octet band chinit's static map accepts)."
  }
}

variable "enable_backup" {
  description = "Provision the dedicated backup replica: a fixed VM at the +30 octet offset running the clickhouse-backup timer."
  type        = bool
  default     = true
}

# ----------------------------------------------------------------------------
# Sizing
# ----------------------------------------------------------------------------

variable "vm_size" {
  description = "VM size for writers, readers and the backup replica."
  type        = string
  default     = "Standard_D4s_v5"
}

variable "keeper_vm_size" {
  description = "VM size for keepers (raft fsync latency favors steady-CPU families)."
  type        = string
  default     = "Standard_D2s_v5"
}

variable "disk_storage_account_type" {
  description = "Managed disk SKU (Premium_LRS / StandardSSD_LRS)."
  type        = string
  default     = "Premium_LRS"
}

variable "os_disk_size_gb" {
  description = "OS disk size (GB) for every node."
  type        = number
  default     = 32
}

variable "metadata_disk_size_gb" {
  description = "Metadata (hot-tier) disk size (GB), LUN 0, label ch-metadata. The disk-autoscale job can grow it online."
  type        = number
  default     = 128
}

variable "cache_disk_size_gb" {
  description = "Blob read-cache disk size (GB), LUN 1, label ch-cache. Must fit the storage.xml <max_size> (the template derives it at 0.9x)."
  type        = number
  default     = 256
}

variable "keeper_disk_size_gb" {
  description = "Keeper raft-log/snapshot disk size (GB), LUN 2, label ch-keeper."
  type        = number
  default     = 32
}

# ----------------------------------------------------------------------------
# Image + admin access
# ----------------------------------------------------------------------------

variable "golden_image_id" {
  description = "The chinit golden Compute Gallery image version resource id (see images/azure). REQUIRED — the wrapper refuses to boot a node without the baked chinit binary."
  type        = string

  validation {
    condition     = can(regex("^/subscriptions/.+/galleries/.+/images/.+/versions/.+", var.golden_image_id))
    error_message = "golden_image_id must be a Compute Gallery image version resource id (build it from images/azure with packer)."
  }
}

variable "admin_username" {
  description = "The VM admin username (SSH key is generated by the module; access is otherwise SSM-equivalent via bastion/serial — no password auth)."
  type        = string
  default     = "azureuser"
}

variable "ssh_public_key" {
  description = "Optional SSH public key for the admin user. Generate one and pass it, or rely on Azure serial console."
  type        = string
  default     = null
}

# ----------------------------------------------------------------------------
# Object storage + DNS
# ----------------------------------------------------------------------------

variable "data_prefix" {
  description = "Blob prefix (inside the data container) for ClickHouse table data. Every replica writes its own path under it."
  type        = string
  default     = "data"
}

variable "backup_retention_days" {
  description = "clickhouse-backup remote retention (days) on the backup replica."
  type        = number
  default     = 7
}

variable "dns_zone_name" {
  description = "Private DNS zone name for node discovery (e.g. clickhouse.internal). Nodes self-register their A records through chinit."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]*[a-z0-9]$", var.dns_zone_name))
    error_message = "dns_zone_name must be a DNS zone name (e.g. clickhouse.internal)."
  }
}

# ----------------------------------------------------------------------------
# Telemetry (optional, inert when unset)
# ----------------------------------------------------------------------------

variable "log_shipping" {
  description = "Remote telemetry shipping (renders into log_shipping in every chinit spec; empty endpoints = disabled). Supports base OTLP endpoint or per-signal VictoriaLogs/VictoriaMetrics/VictoriaTraces URLs."
  type = object({
    agent            = optional(string, "otel")
    endpoint         = optional(string, "")
    logs_endpoint    = optional(string, "")
    metrics_endpoint = optional(string, "")
    traces_endpoint  = optional(string, "")
    headers          = optional(string, "")
    metrics_headers  = optional(string, "")
    traces_headers   = optional(string, "")
    scrape_interval  = optional(string, "15s")
  })
  default  = {}
  nullable = false
}

variable "tracing_config" {
  description = "ClickHouse OpenTelemetry span sampling and system profiler configuration."
  type = object({
    otel_trace_probability        = optional(number, 0)
    otel_keeper_trace_probability = optional(string, "auto")
    otel_trace_processors         = optional(bool, false)
    otel_span_log_ttl_days        = optional(number, 3)
    trace_log_ttl_days            = optional(number, 7)
    global_profiler_period_ns     = optional(number, 0)
    log_query_threads             = optional(bool, false)
  })
  default  = {}
  nullable = false
}

variable "tags" {
  description = "Tags applied to every resource the module creates."
  type        = map(string)
  default     = {}
}
