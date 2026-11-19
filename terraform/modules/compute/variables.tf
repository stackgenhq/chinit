# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Compute Module Variables
# ============================================================================

variable "cluster_name" {
  description = "ClickHouse cluster name"
  type        = string
}

variable "aws_region" {
  description = "AWS region"
  type        = string
}

variable "vpc_id" {
  description = "VPC id"
  type        = string
}

variable "subnet_az" {
  description = "AZ of the nodes subnet (plain string from the network module; avoids data-read-during-apply forcing EBS volume replacement)"
  type        = string
}

variable "subnet_id" {
  description = "PRIVATE subnet id for all ClickHouse nodes (no IGW route; egress via NAT, S3 via gateway endpoint). The PRIMARY nodes subnet: static nodes (keepers, backup) and their EBS volumes are pinned to its AZ; the ASG roles' span is asg_subnet_ids."
  type        = string
}

variable "asg_subnet_ids" {
  description = "Subnet ids the writers/readers ASGs spread across (fallback when writers_asg_subnet_ids / readers_asg_subnet_ids are unset). Single-AZ (default, null): [subnet_id] — every node lands in the primary AZ. Multi-AZ: one subnet per AZ, so an AZ failure re-launches members into the survivors. Keepers/backup NEVER spread (raft/metadata EBS volumes are AZ-pinned to the primary subnet's AZ)."
  type        = list(string)
  default     = null

  validation {
    condition     = var.asg_subnet_ids == null || length(var.asg_subnet_ids) >= 1
    error_message = "asg_subnet_ids must carry at least the primary nodes subnet."
  }
}

variable "writers_asg_subnet_ids" {
  description = "Subnet ids the writers ASG spreads across. Overrides asg_subnet_ids for writers (e.g. [subnet_id] when multi_az_config.scope = 'readers_only', or all 3 private nodes subnets when scope = 'all')."
  type        = list(string)
  default     = null

  validation {
    condition     = var.writers_asg_subnet_ids == null || length(var.writers_asg_subnet_ids) >= 1
    error_message = "writers_asg_subnet_ids must carry at least one subnet when set."
  }
}

variable "readers_asg_subnet_ids" {
  description = "Subnet ids the readers ASG spreads across. Overrides asg_subnet_ids for readers."
  type        = list(string)
  default     = null

  validation {
    condition     = var.readers_asg_subnet_ids == null || length(var.readers_asg_subnet_ids) >= 1
    error_message = "readers_asg_subnet_ids must carry at least one subnet when set."
  }
}

variable "public_subnet_id" {
  description = "PUBLIC subnet id for the internet-facing NLBs (IGW-routed; NLBs are the only compute-module presence here)"
  type        = string
  default     = null
}

variable "nlb_scheme" {
  description = "NLB scheme: 'internet-facing' (default, current behavior) or 'internal' (VPC-internal NLB for private client access, e.g. from an EKS cluster). Internal NLBs are placed in nlb_subnet_ids."
  type        = string
  default     = "internet-facing"

  validation {
    condition     = contains(["internet-facing", "internal"], var.nlb_scheme)
    error_message = "nlb_scheme must be 'internet-facing' or 'internal'."
  }
}

variable "nlb_subnet_ids" {
  description = "Subnets for the NLBs. Required when nlb_scheme = 'internal'. Defaults to [public_subnet_id] when null (internet-facing single-subnet layout). For internal NLBs pass one or more subnets (e.g. the ClickHouse nodes subnet, or dedicated private subnets per AZ)."
  type        = list(string)
  default     = null
}

variable "subnet_cidr" {
  description = "Subnet CIDR (static private IPs derived from it)"
  type        = string
}

variable "placement_strategy" {
  description = "'cluster' = low-latency colocation (sub-ms peer RTT, like a proximity placement group; NOTE: cluster placement does NOT support burstable t4g types — prod profile only). 'spread' = HA-distributed; required for dev profile sizes and supports every type."
  type        = string
  default     = "spread"
}

variable "ami_id" {
  description = "Pre-baked AMI (packer/) with ClickHouse pre-installed. null = latest Canonical Ubuntu 24.04 arm64 (slower bootstrap)"
  type        = string
  default     = null
}

# Sizing
variable "writer_instance_type" {
  description = "Writer EC2 instance type (Graviton default)"
  type        = string
}

variable "reader_instance_type" {
  description = "Reader EC2 instance type"
  type        = string
}

variable "writer_instance_type_pool" {
  description = <<-EOT
    Optional mixed writer pool (the writers ASG's mixed_instances_policy
    override entries). Each entry is one instance type rendered verbatim into
    the override list; `weighted_capacity` stays null (1 ASG unit = 1 writer).
    When unset, the writers ASG launches only var.writer_instance_type
    (on-demand unless writer_spot_ratio > 0, which is only exercised via the
    pool). Members must be >= 8 GiB RAM and size-compatible with the vertical
    autoscaler ladder (writers_ami_roll / writer_type_ladder in chprovision).
  EOT
  type = list(object({
    instance_type     = string
    weighted_capacity = optional(number)
  }))
  default = null
}

variable "writer_spot_ratio" {
  description = "Fraction of writer slots above writer_spot_on_demand_base_capacity on Spot (0.0-1.0). 0 = fully on-demand (default, prod-safe). 1.0 = writer-1..N on Spot while writer-0 stays On-Demand."
  type        = number
  default     = 0.0

  validation {
    condition     = var.writer_spot_ratio >= 0 && var.writer_spot_ratio <= 1
    error_message = "writer_spot_ratio must be between 0.0 and 1.0"
  }
}

variable "writer_spot_on_demand_base_capacity" {
  description = "Absolute number of writer slots kept on-demand when writer_spot_ratio > 0 (minimum 1 so writer-0 stays On-Demand to coordinate backup-wake and cluster maintenance). Slots above this base follow writer_spot_ratio."
  type        = number
  default     = 1

  validation {
    condition     = var.writer_spot_on_demand_base_capacity >= 1
    error_message = "writer_spot_on_demand_base_capacity must be >= 1 so writer-0 (backup-wake & maintenance coordinator) stays On-Demand."
  }
}

variable "reader_instance_type_pool" {
  description = <<-EOT
    Optional mixed reader size pool (the ASG's mixed_instances_policy
    override entries). Each entry is one instance type rendered verbatim
    into the override list; `weighted_capacity` scales the Spot pool's
    launch probability across entries (null = unweighted, equal
    probability). When unset, the pool is the baseline reader type's
    size-sharing family list (the historical single-size behavior).
    The launch template still carries `reader_instance_type` — its EBS
    mappings, AMI, and user-data boot identically on every pool member,
    so pool members are size swaps of the baseline family.
  EOT
  type = list(object({
    instance_type     = string
    weighted_capacity = optional(number)
  }))
  default = null
}

variable "keeper_instance_type" {
  description = "Keeper EC2 instance type"
  type        = string
}

variable "backup_instance_type" {
  description = "Backup replica EC2 instance type (burstable)"
  type        = string
  default     = "t4g.xlarge"
}

variable "writer_count" {
  description = "Number of writer nodes"
  type        = number
}

variable "reader_count" {
  description = "Desired reader count (ASG)"
  type        = number
}

variable "keeper_count" {
  description = "Number of keeper nodes"
  type        = number
}

variable "reader_spot_ratio" {
  description = "Fraction of readers on Spot (0.0-1.0). Prod default 0.5; the on-demand remainder is the always-on floor."
  type        = number
  default     = 0.5
}

variable "reader_spot_on_demand_base_capacity" {
  description = "Absolute number of reader slots kept On-Demand when reader_spot_ratio > 0 (0 = pure ratio split; >= 1 pins an On-Demand reader anchor). Slots above this base follow reader_spot_ratio."
  type        = number
  default     = 0

  validation {
    condition     = var.reader_spot_on_demand_base_capacity >= 0
    error_message = "reader_spot_on_demand_base_capacity must be >= 0."
  }
}

variable "enable_backup_replica" {
  description = "Whether the dedicated backup replica exists (prod only)"
  type        = bool
  default     = true
}

# Disks
variable "root_disk_size_gb" {
  description = "Root (OS) disk size"
  type        = number
  default     = 50
}

variable "metadata_disk_size_gb" {
  description = "Metadata (parts catalog) gp3 disk size - persistent"
  type        = number
  default     = 512
}

variable "metadata_disk_iops" {
  description = "Provisioned gp3 IOPS for the metadata/hot-tier disk (/dev/sdb) on writers and readers (3000 baseline free; 4000 gives headroom for 500+ MB/s sequential merges)"
  type        = number
  default     = 4000
}

variable "metadata_disk_throughput_mbps" {
  description = "Provisioned gp3 throughput (MB/s) for the metadata/hot-tier disk (/dev/sdb). Raising from the 125 MB/s baseline to 500 MB/s eliminates the 65% iowait merge/ingest stall for ~$15/mo per node."
  type        = number
  default     = 500
}

variable "cache_disk_size_gb" {
  description = "S3 cache gp3 disk size (ignored when cache_on_nvme_instance_store = true)"
  type        = number
  default     = 500
}

variable "keeper_disk_size_gb" {
  description = "Keeper raft log/snapshot disk size"
  type        = number
  default     = 64
}

variable "backup_metadata_disk_size_gb" {
  description = "Backup replica metadata disk size"
  type        = number
  default     = 256
}

variable "backup_cache_disk_size_gb" {
  description = "Backup replica cache disk size"
  type        = number
  default     = 128
}

variable "cache_on_nvme_instance_store" {
  description = "Use the instance's included NVMe instance store for the writer/backup cache disks (free, ephemeral - re-warms from S3) instead of persistent gp3. Metadata always stays on persistent gp3. Writers/backup have no instance store today; readers are gated separately by reader_cache_on_nvme_instance_store."
  type        = bool
  default     = false
}

variable "reader_cache_on_nvme_instance_store" {
  description = "Reader-only variant of cache_on_nvme_instance_store: the readers launch template drops the gp3 cache volume and chinit adopts the instance-store NVMe (m*/r*/c*gd families) as the cache disk at boot. Root only sets this when every reader pool member carries local NVMe - otherwise readers boot with no dedicated cache and fall back to a metadata-disk subdirectory (a documented failure mode)."
  type        = bool
  default     = false
}

# Security groups
variable "writers_sg_id" {
  description = "Security group id for writers"
  type        = string
}

variable "readers_sg_id" {
  description = "Security group id for readers"
  type        = string
}

variable "keepers_sg_id" {
  description = "Security group id for keepers"
  type        = string
}

variable "backup_sg_id" {
  description = "Security group id for the backup replica"
  type        = string
}

# Instance profiles
variable "writer_instance_profile_name" {
  description = "IAM instance profile name for writers"
  type        = string
}

variable "reader_instance_profile_name" {
  description = "IAM instance profile name for readers"
  type        = string
}

variable "keeper_instance_profile_name" {
  description = "IAM instance profile name for keepers"
  type        = string
}

variable "backup_instance_profile_name" {
  description = "IAM instance profile name for the backup replica"
  type        = string
}

# User data (stage-1 bootstrap scripts from the clickhouse module)
variable "writer_user_data" {
  description = "Stage-1 bootstrap script for writers"
  type        = string
}

variable "reader_user_data" {
  description = "Stage-1 bootstrap script for readers"
  type        = string
}

variable "keeper_user_data" {
  description = "Stage-1 bootstrap script for keepers"
  type        = string
}

variable "backup_user_data" {
  description = "Stage-1 bootstrap script for the backup replica"
  type        = string
}

# NLB
variable "nlb_tls_mode" {
  description = "'passthrough' (ClickHouse-terminated TLS, TCP listeners) or 'acm' (TLS listeners with certificate_arn attached; targets stay plaintext)"
  type        = string
  default     = "passthrough"
}

variable "acm_certificate_arn" {
  description = "ACM certificate ARN attached to the TLS listeners (acm mode only). Must be ISSUED in the deployment region."
  type        = string
  default     = null
}

# Autoscaling

# Writers ASG bounds — horizontal ingest scaling (aws_autoscaling_group.writers).
variable "writer_min" {
  description = "Writers ASG min_size — the ingest quorum floor. insert_quorum=auto is majority over ALL shard replicas, so the prod floor is 2 (tolerates single-writer churn); a dev floor of 1 is safe only because readers hold copies too. Root passes profile-derived values (prod 2, dev 1); this default exists so the module validates standalone."
  type        = number
  default     = 1
}

variable "writer_max" {
  description = "Writers ASG max_size — the writer ordinal claim ceiling: a node claims writer-N within [0, writer_max) at boot, and the CHI spec renders CR slots from this ceiling (reader CR offset = writer_max + i), so it MUST be >= writer_count. Root passes profile-derived values (prod 4, dev 2); this default exists so the module validates standalone."
  type        = number
  default     = 2

  # Plan-time guard (AWS only rejects desired > max at APPLY time, after the
  # static writer instances have already started churning; catching it at
  # plan is free).
  validation {
    condition     = var.writer_max >= var.writer_count
    error_message = "writer_max must be >= writer_count: it is the writer ordinal claim ceiling and the writers ASG max_size (desired_capacity = writer_count)."
  }
}

variable "writer_cpu_target" {
  description = "Target CPU utilization percentage for the writers ASG target-tracking policy (aws_autoscaling_policy.writers_cpu). Root passes autoscaling_config.writer_cpu_target (default 65)."
  type        = number
  default     = 65
}

variable "autoscaling_enabled" {
  description = "Enable target-tracking CPU autoscaling on the readers ASG"
  type        = bool
  default     = false
}

variable "autoscaling_min" {
  description = "Minimum readers when autoscaling is enabled"
  type        = number
  default     = 1
}

variable "autoscaling_max" {
  description = "Maximum readers when autoscaling is enabled"
  type        = number
  default     = 10
}

variable "autoscaling_target_cpu" {
  description = "Target CPU utilization percentage for the readers ASG"
  type        = number
  default     = 70
}

variable "max_unhealthy_percent" {
  description = "Max unhealthy percentage during instance refresh (rolling upgrades)"
  type        = number
  default     = 20
}

variable "refresh_instance_warmup_seconds" {
  description = "Instance warmup for ASG rolling refreshes: how long a replacement must stay healthy before the ASG touches the next node. The golden AMI boots a node in ~2-3 min (chinit + ClickHouse health gate); the 300s default pads every batch by 5 minutes — dev rolls 5-6 writers serially at MinHealthy=100 and pay ~25-30 min for it."
  type        = number
  default     = 300
}

variable "refresh_min_healthy_percentage" {
  description = "Min healthy percentage for ASG rolling refreshes. 100 = strictly one node at a time (a replacement never launches before its predecessor terminates — required at full capacity); below 100 lets the ASG overlap batches (dev tolerance: a writer fleet of 5 briefly at 4/5 is fine; prod should keep 100)."
  type        = number
  default     = 100

  validation {
    condition     = var.refresh_min_healthy_percentage >= 0 && var.refresh_min_healthy_percentage <= 100
    error_message = "refresh_min_healthy_percentage must be 0-100."
  }
}

variable "scheduled_scaling" {
  description = "Scheduled ASG actions (e.g. dev scale-in at night): list of {name, min_size, max_size, desired_capacity, recurrence, time_zone}"
  type = list(object({
    name             = string
    min_size         = number
    max_size         = number
    desired_capacity = number
    recurrence       = string
    time_zone        = optional(string, "UTC")
  }))
  default = []
}

variable "tags" {
  description = "Tags for all resources"
  type        = map(string)
  default     = {}
}
