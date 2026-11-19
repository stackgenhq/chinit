# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Storage Module Variables — AWS ClickHouse Cluster
# ============================================================================
# Why: The variable surface mirrors the root module's storage_config object
#      (flattened to scalars for module input simplicity) so the root can
#      pass its locals-derived values straight through. This keeps the
#      storage layer independently testable and reusable outside the
#      ClickHouse root.
# Without it: Callers would have to reconstruct the whole storage_config
#             object to use this module, and defaults would live in two
#             places (root + module), inviting drift.
# ============================================================================

# ----------------------------------------------------------------------------
# Core identity
# ----------------------------------------------------------------------------
variable "cluster_name" {
  description = "Name of the ClickHouse cluster. Used to derive IAM role, instance profile, and Name-tag values for every resource in this module."
  type        = string

  validation {
    condition     = can(regex("^[a-zA-Z][a-zA-Z0-9-]*$", var.cluster_name)) && length(var.cluster_name) <= 40
    error_message = "cluster_name must start with a letter, contain only [A-Za-z0-9-], and stay under 40 chars (it is embedded in IAM role names, which cap at 64 characters)."
  }
}

variable "data_bucket_name" {
  description = "Globally unique S3 bucket name for ClickHouse data (parts land under data/<tenant>/). Derived at the root via coalesce so callers never hardcode it twice."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.data_bucket_name))
    error_message = "data_bucket_name must be a valid S3 bucket name: 3-63 lowercase alphanumeric chars, dots or hyphens, no dots adjacent to start/end."
  }
}

variable "backup_bucket_name" {
  description = "Globally unique S3 bucket name for clickhouse-backup output. Separate from the data bucket so retention, versioning, and Object Lock can be tuned independently of the data lifecycle."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.backup_bucket_name)) && var.backup_bucket_name != var.data_bucket_name
    error_message = "backup_bucket_name must be a valid S3 bucket name and DIFFERENT from data_bucket_name (backups must survive a fat-fingered DeleteObject on data)."
  }
}

variable "staging_bucket_name" {
  description = "S3 bucket holding the NDJSON staging prefix the S3Queue engine drains (producers write staged files here, ClickHouse's S3Queue table polls the prefix). Empty string = no staging drain grant is emitted (staging lives elsewhere or is disabled)."
  type        = string
  default     = ""

  validation {
    condition     = var.staging_bucket_name == "" || can(regex("^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.staging_bucket_name))
    error_message = "staging_bucket_name must be a valid S3 bucket name (3-63 lowercase chars) or empty (grant disabled)."
  }
}

variable "staging_key_prefix" {
  description = "Key prefix inside staging_bucket_name that the S3Queue engine polls (e.g. \"processed-logs\"). Writers/readers get Get+Delete on <prefix>/* and a prefix-conditioned ListBucket — exactly the after_processing='delete' surface (delete-on-ingest needs s3:DeleteObject or files pile up until the lifecycle rule eventually removes them). Used only when staging_bucket_name is set."
  type        = string
  default     = ""

  validation {
    # The guard fires ONLY when a staging bucket is configured: with no
    # staging bucket (the default
    # has none), the empty prefix is correct and must not trip the regex.
    # The old condition validated the prefix unconditionally, failing
    # every plan/destroy for stacks with staging disabled.
    condition     = var.staging_bucket_name == "" || can(regex("^[a-zA-Z0-9][a-zA-Z0-9_/.-]*$", var.staging_key_prefix))
    error_message = "staging_key_prefix must be non-empty path characters with no leading/trailing slash when staging_bucket_name is set."
  }
}

variable "raw_log_retention_days" {
  description = "Days before upstream raw .log.gz objects under AWSLogs/ in staging_bucket_name expire (a short replay-backstop window; a 45d default keeps ~45x the raw data alive, duplicating data already normalized into ClickHouse). 0 leaves the staging bucket's lifecycle configuration unmanaged by this module."
  type        = number
  default     = 0

  validation {
    condition     = var.raw_log_retention_days >= 0
    error_message = "raw_log_retention_days must be >= 0 (0 disables staging-bucket lifecycle management; >= 1 sets S3 Lifecycle expiration in days)."
  }
}

variable "staging_retention_days" {
  description = "Days before staged NDJSON files under processed-logs/ (and staging_key_prefix when distinct) expire in staging_bucket_name (drain-outage backstop). Used when raw_log_retention_days > 0."
  type        = number
  default     = 7

  validation {
    condition     = var.staging_retention_days >= 1
    error_message = "staging_retention_days must be >= 1 (S3 Lifecycle expiration age in days)."
  }
}

variable "tenants" {
  description = "Tenant identifiers provisioned into the S3 layout. Each tenant gets an isolated data/<tenant>/ prefix and a per-tenant IAM policy document (see tenant_policy_documents output). ClickHouse creates the keys itself - no per-tenant objects are provisioned here."
  type        = list(string)
  default     = ["default"]

  validation {
    condition     = length(var.tenants) > 0 && alltrue([for t in var.tenants : can(regex("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$", t))])
    error_message = "tenants must be non-empty; each tenant must be 1-63 chars of [A-Za-z0-9_-] starting with an alphanumeric (tenants are embedded in S3 key prefixes and IAM resource ARNs)."
  }

  # No tenant name may be a prefix of another tenant name.
  # Why: S3 prefix isolation cannot tell "data/alpha" apart from
  #      "data/alpha-x" for ListBucket requests (prefix "data/alpha" lists
  #      both trees), so a prefix-colliding pair would leak cross-tenant key
  #      names through the per-tenant ListBucket grant.
  # Without it: Tenant A could enumerate tenant B's object keys (names,
  #             sizes, timestamps) even though object reads stay denied.
  validation {
    condition = alltrue([
      for t in var.tenants : alltrue([
        for other in var.tenants : other == t || !startswith("${other}/", "${t}/")
      ])
    ])
    error_message = "tenant names must not be prefixes of one another (e.g. \"alpha\" vs \"alpha-x\"): S3 ListBucket prefix isolation cannot separate data/alpha from data/alpha-x, which would leak cross-tenant key listings."
  }

  validation {
    condition     = length(var.tenants) == length(distinct(var.tenants))
    error_message = "tenants must not contain duplicates (duplicate tenants would silently collapse in the per-tenant policy map while double-listing in tenant_prefixes)."
  }
}

# ----------------------------------------------------------------------------
# Prefixes
# ----------------------------------------------------------------------------
variable "data_prefix" {
  description = "S3 key prefix holding all ClickHouse parts (data/<tenant>/...). Scoped by the lifecycle rule and every node-role policy; must stay disjoint from bootstrap_prefix."
  type        = string
  default     = "data"

  # Non-empty on purpose.
  # Why: The all-tenant wildcard grant (writer/reader roles), the per-tenant
  #      policy documents, and the bootstrap-prefix isolation all derive
  #      from "everything under data/". With an empty prefix (bucket root)
  #      those scopes collapse into each other and the writer role would
  #      gain delete rights over the bootstrap payload.
  validation {
    condition     = can(regex("^[a-zA-Z0-9]([a-zA-Z0-9/_-]*[a-zA-Z0-9])?$", var.data_prefix)) && var.data_prefix != ""
    error_message = "data_prefix must be a non-empty string with no leading or trailing slash (it scopes tenant isolation, lifecycle rules, and IAM resource ARNs)."
  }
}

variable "backup_prefix" {
  description = "S3 key prefix inside the backup bucket where clickhouse-backup writes. Empty string means the bucket root (safe here: the whole backup bucket is backup-only)."
  type        = string
  default     = "backups"

  validation {
    condition     = var.backup_prefix == "" || can(regex("^[a-zA-Z0-9]([a-zA-Z0-9/_-]*[a-zA-Z0-9])?$", var.backup_prefix))
    error_message = "backup_prefix must not start or end with a slash (it is joined into S3 keys and IAM resource ARNs)."
  }
}

variable "bootstrap_prefix" {
  description = "S3 key prefix (inside the data bucket) where the clickhouse module uploads stage-2 cloud-init. Node roles get read-only access to this prefix at boot."
  type        = string
  default     = "bootstrap"

  validation {
    condition     = can(regex("^[a-zA-Z0-9]([a-zA-Z0-9/_-]*[a-zA-Z0-9])?$", var.bootstrap_prefix)) && var.bootstrap_prefix != ""
    error_message = "bootstrap_prefix must be a non-empty string with no leading or trailing slash (keepers get read access to exactly this prefix)."
  }

  # bootstrap must stay OUTSIDE the data prefix tree.
  # Why: writer/reader roles hold Get/Put/Delete over everything under
  #      data/<tenant>/*. If bootstrap nested inside data/, a compromised
  #      writer could tamper with or delete the stage-2 cloud-init every
  #      other node boots from.
  # Without it: One bad writer credential could rewrite the bootstrap
  #             payload and pivot to every node that boots after it.
  validation {
    condition     = !startswith("${var.bootstrap_prefix}/", "${var.data_prefix}/") && !startswith("${var.data_prefix}/", "${var.bootstrap_prefix}/")
    error_message = "bootstrap_prefix and data_prefix must not overlap: node roles get write access to all of data/*, so nesting bootstrap there would let them delete the stage-2 cloud-init other nodes boot from."
  }
}

# ----------------------------------------------------------------------------
# Lifecycle & data protection
# ----------------------------------------------------------------------------
variable "lifecycle_days_to_ia" {
  description = "Days after which objects under the data/cas prefixes transition from S3 Standard to Standard-IA. Pairs with ClickHouse's move_factor offloading while staying instantly readable (never Glacier). Also reused as the noncurrent-version cleanup age when data versioning is on. Defaults to 0 (IA transition DISABLED) because S3 Standard-IA enforces a 30-day minimum storage charge per object: transitioning at day 30 when ClickHouse TTL drops parts at day 31-33 triggers ~27 days of early-deletion fees (~5x more expensive than S3 Standard). Only set >= 30 when table TTL retention is >= ~48-60+ days."
  type        = number
  default     = 0

  validation {
    condition     = var.lifecycle_days_to_ia >= 0
    error_message = "lifecycle_days_to_ia must be >= 0 (0 disables the IA transition; S3 Lifecycle transition age in days otherwise)."
  }
}

variable "cas_prefix" {
  description = "Key prefix of the CAS (content-addressed) pool inside the data bucket when CAS is enabled on the ClickHouse nodes. CAS GC deletes unreferenced blobs, so the node roles need DeleteObject on <cas_prefix>/*; set to empty string to leave CAS IAM grants out entirely (CAS disabled)."
  type        = string
  default     = ""

  validation {
    condition     = var.cas_prefix == "" || can(regex("^[a-z0-9][a-z0-9/_-]{0,126}[a-z0-9]$", var.cas_prefix))
    error_message = "cas_prefix must be empty (CAS IAM grants off) or a relative S3 key prefix of [a-z0-9/_-]."
  }
}

variable "enable_versioning" {
  description = "Enable S3 versioning on the DATA bucket. Off by default: ClickHouse parts are immutable (write-once, deleted-on-merge), so versioning would only add cost. Flip on for compliance-driven overwrite/delete protection."
  type        = bool
  default     = false
}

variable "backup_enable_versioning" {
  description = "Enable S3 versioning on the BACKUP bucket. On by default: a deleted backup version stays recoverable, which is the point of having backups."
  type        = bool
  default     = true
}

variable "backup_enable_object_lock" {
  description = "Apply a default GOVERNANCE-mode Object Lock to every backup objects for backup_retention_days, so compromised credentials cannot delete recent backups. Create-only bucket property: flipping it later forces bucket replacement."
  type        = bool
  default     = false
}

variable "backup_retention_days" {
  description = "Days a backup is kept: drives the backup-bucket lifecycle expiration AND the Object Lock window (they align, so lifecycle never deletes a retention-locked object)."
  type        = number
  default     = 7

  validation {
    condition     = var.backup_retention_days >= 1
    error_message = "backup_retention_days must be >= 1 (S3 Lifecycle expiration age in days)."
  }
}

# ----------------------------------------------------------------------------
# Tags
# ----------------------------------------------------------------------------
variable "tags" {
  description = "Tags applied to every taggable resource (buckets, IAM roles). Always merged with a Name tag identifying the resource."
  type        = map(string)
  default     = {}
}

# ----------------------------------------------------------------------------
# Reserved inputs (kept for interface stability)
# ----------------------------------------------------------------------------
variable "vpc_id" {
  description = "VPC id the cluster runs in. Reserved: not used by this module yet - kept in the interface so a future S3 gateway-endpoint bucket policy (Deny non-endpoint traffic) can consume it without breaking existing callers."
  type        = string
  default     = null
}

variable "subnet_id" {
  description = "Subnet id the cluster runs in. Reserved: not used by this module yet - same future-endpoint-policy purpose as vpc_id."
  type        = string
  default     = null
}

variable "flow_log_bucket_arn" {
  description = "ARN of the VPC flow-log bucket owned by the network module. Reserved passthrough: the flow-log bucket is deliberately NOT created here (storage module stays single-purpose), but the interface keeps the slot so the root and future bucket-policy additions do not churn."
  type        = string
  default     = null
}
