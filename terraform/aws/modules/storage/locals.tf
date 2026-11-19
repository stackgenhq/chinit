# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — Storage Module
# ============================================================================
# All derived values for this module live here, in ONE locals block (open
# source Terraform convention): resources below reference local.* only.
# Comments from the original main.tf / iam.tf blocks are preserved verbatim.
# ============================================================================

locals {
  # data_prefix is validated non-empty, so these are always well-formed
  # "prefix/" strings (trailing slash, no leading slash).
  data_prefix_normalized      = "${var.data_prefix}/"
  backup_prefix_normalized    = var.backup_prefix == "" ? "" : "${var.backup_prefix}/"
  bootstrap_prefix_normalized = "${var.bootstrap_prefix}/"

  # Staging-drain grant surface (empty when staging_bucket_name is unset):
  # the S3Queue engine drains <staging_key_prefix>/* on writer nodes ONLY
  # and deletes each file on successful ingest (after_processing='delete').
  # Readers and backup nodes receive rows via ClickHouse replication and
  # must never hold S3 permissions on the staging prefix.
  staging_drain_roles       = var.staging_bucket_name == "" ? [] : ["writer"]
  staging_arn               = "arn:aws:s3:::${var.staging_bucket_name}"
  staging_prefix_normalized = var.staging_key_prefix == "" ? "" : "${var.staging_key_prefix}/"

  # Staging-bucket lifecycle management: active when a staging bucket is
  # configured AND raw_log_retention_days > 0. S3 allows only ONE
  # lifecycle configuration per bucket (PutBucketLifecycleConfiguration
  # replaces the whole ruleset), so the resource renders all three
  # prefix-scoped rules together:
  #   1. expire-staged-logs       (processed-logs)
  #   2. expire-synthetic-staging (staging_key_prefix when != processed-logs)
  #   3. expire-raw-logs          (AWSLogs/ -> raw_log_retention_days)
  manage_staging_lifecycle = var.staging_bucket_name != "" && var.raw_log_retention_days > 0
  extra_staging_prefixes   = var.staging_key_prefix != "" && var.staging_key_prefix != "processed-logs" ? [var.staging_key_prefix] : []

  # Per-tenant key prefixes under the data prefix: data/<tenant>/
  # Why: ClickHouse creates these keys itself as it writes parts; the list
  #      here exists to drive per-tenant policy documents and outputs.
  tenant_prefixes = [for t in var.tenants : "${local.data_prefix_normalized}${t}"]

  # Bucket ARNs, derived statically from the (validated) bucket-name
  # variables. Equivalent to aws_s3_bucket.<x>.arn (S3 ARNs are exactly
  # "arn:aws:s3:::<name>"), but fully computable at plan time so the bucket
  # policy and per-tenant policy documents render reviewably in every plan
  # instead of "(known after apply)".
  data_arn   = "arn:aws:s3:::${var.data_bucket_name}"
  cas_arn    = "arn:aws:s3:::${var.data_bucket_name}-cas"
  backup_arn = "arn:aws:s3:::${var.backup_bucket_name}"

  # Name tags identifying every resource (matching the root's naming style).
  data_name_tag   = "${var.cluster_name}-clickhouse-data"
  backup_name_tag = "${var.cluster_name}-clickhouse-backups"

  # Backup bucket versioning: on by default; forced on when Object Lock is
  # enabled because S3 Object Lock requires a versioned bucket - creating a
  # lock-enabled bucket with versioning suspended fails at apply time.
  backup_versioning_enabled = var.backup_enable_versioning || var.backup_enable_object_lock

  # Roles that receive instance profiles in iam.tf. Kept here so the two
  # files stay consistent on naming while each remains readable.
  node_roles = ["writer", "reader", "keeper", "backup"]

  tags = var.tags
  # Bucket ARNs are defined once in main.tf (local.data_arn /
  # local.backup_arn) and reused by every policy document below.

  # Object-level action sets shared across roles.
  s3_rw   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
  s3_read = ["s3:GetObject"]
  s3_list = ["s3:ListBucket"]

  # Bootstrap prefix objects: the clickhouse module uploads stage-2
  # cloud-init there; every node role reads it at boot.
  bootstrap_objects = [
    "arn:aws:s3:::${var.data_bucket_name}/${local.bootstrap_prefix_normalized}*",
    "${local.cas_arn}/${local.bootstrap_prefix_normalized}*",
  ]

  # Which bucket-scopes and object-actions each node role gets.
  # - writer: full CRUD on ALL tenant prefixes under data/ (writers serve
  #   every tenant), plus PutObject on the backup bucket for any tooling
  #   that ships part files out, plus read on the bootstrap prefix.
  # - reader: same data-bucket CRUD as writer. S3-backed zero-copy readers
  #   still write parts on merge and evict on drop - a read-only reader
  #   would break merges, so it gets Get/Put/Delete on data. It does NOT
  #   get backup writes.
  # - keeper: bootstrap read ONLY. Keeper state is local (clickhouse-keeper
  #   keeps its log on instance storage); keepers never touch tenant data.
  # - backup: full CRUD on the backup bucket (clickhouse-backup reads data,
  #   writes/deletes backups) plus Get+ListBucket on the data bucket since
  #   a backup must be able to read every part it is archiving.
  #
  # data_prefix_scopes uses "all" as the marker for "every tenant prefix".
  # Why: The scope strings are joined into resource ARNs
  #      ("bucket/data/<scope>/*"); "*" as a marker value would render the
  #      double-wildcard "data/*/*", while the all-marker yields "data/*".
  # Without it: The rendered ARN grants nothing (S3 matching treats
  #             "data/*/*" as literal two-segment paths) and every ClickHouse
  #             write fails with 403 at runtime, which validate cannot catch.
  role_permissions = {
    writer = {
      data_bucket_actions   = local.s3_list
      data_object_actions   = local.s3_rw
      data_prefix_scopes    = ["all"] # all tenant prefixes - writers serve all tenants
      backup_object_actions = ["s3:PutObject"]
      bootstrap_read        = true
    }
    reader = {
      data_bucket_actions   = local.s3_list
      data_object_actions   = local.s3_rw
      data_prefix_scopes    = ["all"] # all tenant prefixes - readers serve all tenants
      backup_object_actions = []
      bootstrap_read        = true
    }
    keeper = {
      data_bucket_actions   = []
      data_object_actions   = []
      data_prefix_scopes    = []
      backup_object_actions = []
      bootstrap_read        = true
    }
    backup = {
      data_bucket_actions   = local.s3_list
      data_object_actions   = local.s3_read
      data_prefix_scopes    = ["all"] # read-all: a backup archives every tenant's parts
      backup_object_actions = local.s3_rw
      bootstrap_read        = true
    }
  }
  # CAS pool prefix (empty = CAS disabled; no extra IAM). Kept separate from
  # the tenant prefixes: the pool is shared across replicas by design.
  cas_prefix_normalized = var.cas_prefix == "" ? "" : "${trimsuffix(var.cas_prefix, "/")}/"

  # Managed policies every node role receives.
  # NOTE: CloudWatchAgentServerPolicy is the real AWS-managed policy for the
  # agent (AmazonCloudWatchAgentInstallerPolicy does not exist).
  managed_policy_arns = [
    "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
    "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy",
  ]
}
