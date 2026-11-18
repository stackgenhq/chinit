# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Inputs — chinit cluster module (AWS)
# ============================================================================
# The module is intentionally small: every variable below is either a
# chinit BootSpec field rendered into the node specs, a sizing knob for
# the four node roles, or a bring-your-own hook (network) for callers
# that already run a VPC.
# ============================================================================

variable "cluster_name" {
  description = "ClickHouse cluster name. Renders into the chinit specs (cluster_name), the CHI spec, DNS names and every resource name."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$", var.cluster_name))
    error_message = "cluster_name must be 3-32 chars of [a-z0-9-], no leading/trailing hyphen (it lands in DNS names and S3 keys)."
  }
}

variable "region" {
  description = "AWS region. Pins every SDK client chinit builds (the spec's region field) and the S3 endpoint env."
  type        = string
}

# ----------------------------------------------------------------------------
# Identity ceilings (ASG maxima — never live counts)
# ----------------------------------------------------------------------------

variable "writer_max" {
  description = "Writers ASG max_size = the writer ordinal claim ceiling. Writers claim writer-N in [0, writer_max) from the claims table at boot; a scale-out must always be able to claim, so pass the MAXIMUM the fleet can reach, not today's count."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_max >= 1
    error_message = "writer_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

variable "writer_desired" {
  description = "Writers ASG desired capacity at apply time."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_desired >= 1 && var.writer_desired <= var.writer_max
    error_message = "writer_desired must be within [1, writer_max]."
  }
}

variable "reader_max" {
  description = "Readers ASG max_size = the reader ordinal claim ceiling (the same contract as writer_max)."
  type        = number
  default     = 6

  validation {
    condition     = var.reader_max >= 1
    error_message = "reader_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

variable "reader_desired" {
  description = "Readers ASG desired capacity at apply time."
  type        = number
  default     = 2

  validation {
    condition     = var.reader_desired >= 0 && var.reader_desired <= var.reader_max
    error_message = "reader_desired must be within [0, reader_max]."
  }
}

variable "reader_spot" {
  description = "Launch readers on Spot (mixed ASG, 100% spot with an on-demand fallback base of 0). chinit's spot-watch drains on the EC2 interruption notice."
  type        = bool
  default     = true
}

variable "keeper_count" {
  description = "Keeper (Raft) fleet size. Must be odd (quorum arithmetic); 3 tolerates one loss, 5 tolerates two. Keepers are fixed instances with static IPs at the +20 octet offsets."
  type        = number
  default     = 3

  validation {
    condition     = var.keeper_count % 2 == 1 && var.keeper_count >= 1 && var.keeper_count <= 10
    error_message = "keeper_count must be odd (Raft quorum) and within [1, 10] (the +20..+29 octet band chinit's static map accepts)."
  }
}

variable "enable_backup" {
  description = "Provision the dedicated backup replica: a fixed instance at the +30 octet offset running the clickhouse-backup timer. The backup is a CR member (its slot is the first after the readers' ceiling)."
  type        = bool
  default     = true
}

# ----------------------------------------------------------------------------
# Sizing
# ----------------------------------------------------------------------------

variable "instance_type" {
  description = "Instance type for writers, readers and the backup replica (arm64 recommended: Graviton). Keepers use keeper_instance_type."
  type        = string
  default     = "r8g.large"

  validation {
    condition     = !can(regex("(\\.(nano|micro|small|medium)$|^c[0-9]+[a-z]*\\.large$)", var.instance_type))
    error_message = "instance_type must provide at least 8 GiB RAM (e.g. t4g.large, m8g.large, r8g.large, c8g.xlarge or larger); <= 4 GiB instances saturate CPU and OOM during ReplicatedMergeTree merges."
  }
}

variable "keeper_instance_type" {
  description = "Instance type for keepers (raft fsync latency favors steady-CPU families)."
  type        = string
  default     = "m8g.large"
}

variable "root_disk_size_gb" {
  description = "Root volume size (GB) for every node."
  type        = number
  default     = 32
}

variable "metadata_disk_size_gb" {
  description = "Metadata (hot-tier) EBS volume size (GB) for writers/readers/backup. Mounted at /var/lib/clickhouse, label ch-metadata. This is where ReplicatedMergeTree hot parts live; the disk-autoscale job can grow it online up to 256 GB."
  type        = number
  default     = 64
}

variable "metadata_disk_iops" {
  description = "Provisioned gp3 IOPS for the metadata (hot-tier) volume (/dev/sdb) on writers and readers."
  type        = number
  default     = 4000

  validation {
    condition     = var.metadata_disk_iops >= 3000
    error_message = "metadata_disk_iops must be >= 3000."
  }
}

variable "metadata_disk_throughput_mbps" {
  description = "Provisioned gp3 throughput (MB/s) for the metadata (hot-tier) volume (/dev/sdb). Default 500 MB/s avoids the 125 MB/s gp3 baseline sequential merge iowait stall."
  type        = number
  default     = 500

  validation {
    condition     = var.metadata_disk_throughput_mbps >= 250
    error_message = "metadata_disk_throughput_mbps must be >= 250 MB/s to prevent sequential MergeTree merge iowait stalls."
  }
}

variable "cache_disk_size_gb" {
  description = "S3 read-cache EBS volume size (GB) for writers/readers. Mounted at /var/lib/clickhouse/disks, label ch-cache. Must fit the storage.xml <max_size> (the template derives it at 0.9x)."
  type        = number
  default     = 128
}

variable "keeper_disk_size_gb" {
  description = "Keeper raft-log/snapshot volume size (GB), label ch-keeper (LUN-equivalent /dev/sdf)."
  type        = number
  default     = 16
}

# ----------------------------------------------------------------------------
# Image + chinit
# ----------------------------------------------------------------------------

variable "golden_ami_id" {
  description = "The chinit golden AMI id (see images/aws; e.g. `ami-0f8511dd30c68ca21` for the Altinity Antalya build). REQUIRED — the wrapper refuses to boot a node without the baked chinit binary."
  type        = string

  validation {
    condition     = can(regex("^ami-[0-9a-f]{8,17}$", var.golden_ami_id))
    error_message = "golden_ami_id must look like ami-xxxxxxxx (build it from images/aws with packer)."
  }
}

# ----------------------------------------------------------------------------
# Network (bring-your-own or module-created)
# ----------------------------------------------------------------------------

variable "create_network" {
  description = "Create the network stack (VPC + private subnet + S3/DynamoDB gateway endpoints). Set to false to plug the cluster into an existing VPC via vpc_id/subnet_id."
  type        = bool
  default     = true
}

variable "vpc_id" {
  description = "Existing VPC id (create_network = false). The private DNS zone associates with it."
  type        = string
  default     = null
}

variable "subnet_id" {
  description = "Existing private subnet id (create_network = false). Every node launches here; keepers/backup take static IPs at the +20/+30 offsets inside it."
  type        = string
  default     = null
}

variable "subnet_cidr" {
  description = "The subnet's IPv4 CIDR (required in BOTH modes: it renders into the chinit spec's network section — the ClickHouse <networks> entries — and allocates the static keeper/backup IPs)."
  type        = string

  validation {
    condition     = can(cidrnetmask(var.subnet_cidr))
    error_message = "subnet_cidr must be a valid IPv4 CIDR."
  }
}

variable "client_cidrs" {
  description = "CIDRs allowed to reach ClickHouse client ports (8123/9000/8443/9440) on writers and readers. Empty = no client ingress (interserver + keeper traffic still flows)."
  type        = list(string)
  default     = []
}

# ----------------------------------------------------------------------------
# Object storage (the cold tier + the config bundle)
# ----------------------------------------------------------------------------

variable "s3_data_prefix" {
  description = "Key prefix (inside the data bucket) for ClickHouse table data. Every replica writes its own path under it (zero-copy replication is deprecated upstream)."
  type        = string
  default     = "data"
}

variable "cas_enabled" {
  description = "Enable the Altinity Antalya Content-Addressable Storage (CAS) shared S3 cold tier (`metadata_type = cas`, single copy across replicas). Requires an Antalya golden AMI (e.g. `ami-0f8511dd30c68ca21`)."
  type        = bool
  default     = false
}

variable "cas_prefix" {
  description = "Shared S3 key prefix (no `{replica}` segment) for the CAS cold-tier pool when `cas_enabled = true`."
  type        = string
  default     = "cas"
}

variable "backup_retention_days" {
  description = "clickhouse-backup remote retention (days) on the backup replica."
  type        = number
  default     = 7
}

# ----------------------------------------------------------------------------
# DNS
# ----------------------------------------------------------------------------

variable "dns_zone_name" {
  description = "Private DNS zone name for node discovery (e.g. clickhouse.internal). Nodes self-register writer-N/reader-N/keeper-N/backup-0 A records; peers resolve each other through it."
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
  description = "Remote log shipping (renders into log_shipping in every chinit spec; empty endpoint = disabled). agent: otel | fluentbit; endpoint: OTLP/HTTP target; headers: comma-separated k=v pairs."
  type = object({
    agent    = optional(string, "otel")
    endpoint = optional(string, "")
    headers  = optional(string, "")
  })
  default  = {}
  nullable = false
}

variable "tags" {
  description = "Tags applied to every resource the module creates."
  type        = map(string)
  default     = {}
}
