# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# S3 Buckets, Lifecycle, Bucket Policies & Tenant Policy Documents
# ============================================================================
# Why: S3 is the persistence layer for ClickHouse parts:
#      it is the persistence layer for ClickHouse parts (data bucket) and
#      clickhouse-backup output (backup bucket). The data bucket is the
#      reason this architecture is "disk storage scalable like crazy" - a tenant
#      can add an arbitrary number of parts with no disk provisioning.
# Without it: ClickHouse has nowhere to write parts, and clickhouse-backup
#             has nowhere to write backups. The entire persistence layer is
#             missing.
# ============================================================================

# ----------------------------------------------------------------------------
# Locals — prefix normalization
# ----------------------------------------------------------------------------
# Why: Single source of truth for normalized key prefixes. IAM resource ARNs
#      and the tenant policy documents must reference identical prefix
#      strings, or a policy scoped to data/tenant-x would silently not match
#      the keys ClickHouse actually writes.
# Without it: Prefix strings would be re-derived (and could drift) at every
#             use site, producing IAM policies that grant nothing and S3
#             403s that look like auth bugs.
# ----------------------------------------------------------------------------

# ============================================================================
# Data Bucket — ClickHouse parts storage
# The data bucket is where ClickHouse writes every data part under data/<tenant>/.
# ============================================================================
resource "aws_s3_bucket" "data" {
  bucket = var.data_bucket_name
  tags   = merge(local.tags, { Name = local.data_name_tag })
}

# ----------------------------------------------------------------------------
# Data Bucket — Block Public Access (all four settings)
# Public access on the data bucket would expose every tenant's rows.
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_public_access_block" "data" {
  bucket = aws_s3_bucket.data.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ----------------------------------------------------------------------------
# Data Bucket — SSE-S3 encryption (deliberately NOT SSE-KMS)
# SSE-S3 is applied by S3 itself at zero marginal cost and zero extra API calls.
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_server_side_encryption_configuration" "data" {
  bucket = aws_s3_bucket.data.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# ----------------------------------------------------------------------------
# Data Bucket — Versioning (NEVER enabled by default; required for CAS)
# ClickHouse parts are immutable, and Altinity Antalya's CasProbe refuses to
# mount on any bucket where versioning is Enabled OR Suspended (calling
# PutBucketVersioning(Status="Suspended") on a new bucket permanently causes
# S3 to mint delete markers on every DELETE). Only create this resource when
# enable_versioning = true.
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_versioning" "data" {
  count  = var.enable_versioning ? 1 : 0
  bucket = aws_s3_bucket.data.id

  versioning_configuration {
    status = "Enabled"
  }
}

# ----------------------------------------------------------------------------
# Data Bucket — Lifecycle: transition cold parts to S3 Standard-IA
# ClickHouse's move_factor offloads cold parts to object storage;
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_lifecycle_configuration" "data" {
  bucket = aws_s3_bucket.data.id

  rule {
    id     = "tier-cold-parts-to-ia"
    status = "Enabled"

    # Scope the rule to the data prefix so the bootstrap prefix (owned by
    # the clickhouse module) is never re-tired or expired by this policy.
    filter {
      prefix = local.data_prefix_normalized
    }

    # Transition only - no expiration. Part deletion is ClickHouse's job
    # (merges/drops/TTL); S3 must never delete a part ClickHouse still
    # references, or every query touching it errors.
    # Omitted when lifecycle_days_to_ia = 0 (<=47d retention: S3 Standard-IA
    # bills a 30-day minimum storage duration per object, so transitioning at
    # day 30 for a 31-33d TTL triggers ~27 days of early-deletion fees).
    dynamic "transition" {
      for_each = var.lifecycle_days_to_ia > 0 ? [1] : []
      content {
        days          = var.lifecycle_days_to_ia
        storage_class = "STANDARD_IA"
      }
    }

    # Noncurrent versions (only exist when enable_versioning=true) are
    # cleaned on the same schedule so opt-in versioning does not leave
    # hidden storage behind after part merges/drops.
    noncurrent_version_expiration {
      noncurrent_days = var.lifecycle_days_to_ia > 0 ? var.lifecycle_days_to_ia : 30
    }

    # Abort incomplete multipart uploads: ClickHouse's S3 disk writes large
    # parts as multipart uploads, and a node replaced mid-upload (spot
    # reclaim, ASG scale-in, crash) leaves billable fragments that no
    # GetObject/ListObjects call can see. The 7-day abort reclaims them.
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  # When Altinity Antalya CAS is enabled (cas_prefix != ""), cold blobs live
  # under <cas_prefix>/tiered/ and <cas_prefix>/<tenant>/ instead of data/.
  # Apply the same 7-day multipart-abort (a hard CAS prerequisite per
  # the CAS runbook) and Standard-IA transition to the CAS pool prefix.
  dynamic "rule" {
    for_each = local.cas_prefix_normalized != "" ? [1] : []
    content {
      id     = "tier-cas-parts-to-ia"
      status = "Enabled"

      filter {
        prefix = local.cas_prefix_normalized
      }

      dynamic "transition" {
        for_each = var.lifecycle_days_to_ia > 0 ? [1] : []
        content {
          days          = var.lifecycle_days_to_ia
          storage_class = "STANDARD_IA"
        }
      }

      noncurrent_version_expiration {
        noncurrent_days = var.lifecycle_days_to_ia > 0 ? var.lifecycle_days_to_ia : 30
      }

      abort_incomplete_multipart_upload {
        days_after_initiation = 7
      }
    }
  }
}

# ============================================================================
# Dedicated CAS Data Bucket (unversioned by construction)
# ============================================================================
# Legacy data buckets that ever had PutBucketVersioning(Status="Suspended")
# applied permanently mint S3 delete markers on DELETE, which Altinity
# Antalya's CasProbe refuses with Code 48. Provisioning a dedicated
# "${var.data_bucket_name}-cas" bucket when CAS is enabled guarantees a
# never-versioned bucket without replacing or emptying aws_s3_bucket.data,
# and allows toggling cas_config.enabled back and forth with a single flag.
resource "aws_s3_bucket" "cas" {
  count         = 1
  bucket        = "${var.data_bucket_name}-cas"
  force_destroy = true
  tags          = merge(local.tags, { Name = "${local.data_name_tag}-cas" })
}

resource "aws_s3_bucket_public_access_block" "cas" {
  count  = 1
  bucket = aws_s3_bucket.cas[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "cas" {
  count  = 1
  bucket = aws_s3_bucket.cas[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "cas" {
  count  = 1
  bucket = aws_s3_bucket.cas[0].id

  rule {
    id     = "tier-cold-parts-to-ia"
    status = "Enabled"

    filter {
      prefix = local.data_prefix_normalized
    }

    dynamic "transition" {
      for_each = var.lifecycle_days_to_ia > 0 ? [1] : []
      content {
        days          = var.lifecycle_days_to_ia
        storage_class = "STANDARD_IA"
      }
    }

    noncurrent_version_expiration {
      noncurrent_days = var.lifecycle_days_to_ia > 0 ? var.lifecycle_days_to_ia : 30
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  dynamic "rule" {
    for_each = local.cas_prefix_normalized != "" ? [1] : []
    content {
      id     = "tier-cas-parts-to-ia"
      status = "Enabled"

      filter {
        prefix = local.cas_prefix_normalized
      }

      dynamic "transition" {
        for_each = var.lifecycle_days_to_ia > 0 ? [1] : []
        content {
          days          = var.lifecycle_days_to_ia
          storage_class = "STANDARD_IA"
        }
      }

      noncurrent_version_expiration {
        noncurrent_days = var.lifecycle_days_to_ia > 0 ? var.lifecycle_days_to_ia : 30
      }

      abort_incomplete_multipart_upload {
        days_after_initiation = 7
      }
    }
  }
}

resource "aws_s3_bucket_policy" "cas" {
  count  = 1
  bucket = aws_s3_bucket.cas[0].id
  policy = data.aws_iam_policy_document.cas_bucket_policy[0].json

  depends_on = [aws_s3_bucket_public_access_block.cas]
}

data "aws_iam_policy_document" "cas_bucket_policy" {
  count = 1

  statement {
    sid     = "DenyInsecureTransport"
    effect  = "Deny"
    actions = ["s3:*"]
    resources = [
      local.cas_arn,
      "${local.cas_arn}/*",
    ]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

# ============================================================================
# Backup Bucket — clickhouse-backup output
# Backups live in a SEPARATE bucket (not a prefix of the data bucket) so retention, versioning, and Object Lock are independently tunable: a…
# ============================================================================
resource "aws_s3_bucket" "backup" {
  bucket = var.backup_bucket_name
  tags   = merge(local.tags, { Name = local.backup_name_tag })

  # SECURITY: Object Lock (governance mode) makes recent backups undeletable
  # by compromised credentials. It is a create-only bucket property, so it
  # is set at creation time; flipping backup_enable_object_lock on an
  # existing bucket forces replacement.
  object_lock_enabled = var.backup_enable_object_lock
}

# ----------------------------------------------------------------------------
# Backup Bucket — Block Public Access
# Backups contain a full copy of every tenant's data;
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_public_access_block" "backup" {
  bucket = aws_s3_bucket.backup.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ----------------------------------------------------------------------------
# Backup Bucket — Metrics configuration (daily NumberOfObjects)
# The monitoring module's backup_missing alarm reads AWS/S3 NumberOfObjects for this bucket.
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_metric" "backup" {
  bucket = aws_s3_bucket.backup.id

  name = "entire-bucket"

  # Empty-prefix filter = the entire bucket (the AWS "entire-bucket"
  # convention; a truly empty filter block fails provider validation).
  filter {
    prefix = ""
  }
}

# ----------------------------------------------------------------------------
# Backup Bucket — SSE-S3 encryption
# Same rationale as the data bucket (free, no per-object KMS API calls).
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_server_side_encryption_configuration" "backup" {
  bucket = aws_s3_bucket.backup.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# ----------------------------------------------------------------------------
# Backup Bucket — Versioning (on by default)
# A backup that gets deleted (or partially overwritten by a second backup run) should stay recoverable.
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_versioning" "backup" {
  bucket = aws_s3_bucket.backup.id

  versioning_configuration {
    status = local.backup_versioning_enabled ? "Enabled" : "Suspended"
  }
}

# ----------------------------------------------------------------------------
# Backup Bucket — Lifecycle: expire backups after retention window
# clickhouse-backup prunes its own manifests, but the S3-side expiration is the backstop that keeps the bucket from growing without bound when…
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_lifecycle_configuration" "backup" {
  bucket = aws_s3_bucket.backup.id

  rule {
    id     = "expire-old-backups"
    status = "Enabled"

    filter {
      prefix = var.backup_prefix == "" ? null : local.backup_prefix_normalized
    }

    expiration {
      days = var.backup_retention_days
    }

    # Noncurrent versions (from versioning) expire on the same schedule so
    # deleted backup versions do not accumulate hidden Standard storage.
    noncurrent_version_expiration {
      noncurrent_days = var.backup_retention_days
    }

    # Abort incomplete uploads: a backup replica crash mid-upload leaves
    # billable fragments nothing can see or delete via normal APIs.
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

# ----------------------------------------------------------------------------
# Backup Bucket — Default Object Lock retention (governance mode)
# Object Lock in GOVERNANCE mode makes recent backups un-deletable even by the backup role itself (special permissions are needed to bypass).
# ----------------------------------------------------------------------------
resource "aws_s3_bucket_object_lock_configuration" "backup" {
  count  = var.backup_enable_object_lock ? 1 : 0
  bucket = aws_s3_bucket.backup.id

  rule {
    default_retention {
      mode = "GOVERNANCE"
      days = var.backup_retention_days
    }
  }
}

# ============================================================================
# Staging / Raw-Logs Bucket — Lifecycle: cap upstream raw .log.gz retention
# ============================================================================
# Why: The upstream raw-logs bucket (staging_bucket_name) receives raw
#      .log.gz files under AWSLogs/ alongside the staged NDJSON prefixes
#      (processed-logs/ and the optional staging_key_prefix). S3 allows only
#      ONE lifecycle configuration per bucket, so managing
#      raw_log_retention_days must preserve the staging-prefix expiration
#      rules in the same resource.
# Without it: Raw .log.gz files sit under AWSLogs/ for the upstream default
#             (often 45d) duplicating data already normalized into
#             ClickHouse, or a partial lifecycle rule would clobber the
#             drain-outage backstop on the staged prefixes.
resource "aws_s3_bucket_lifecycle_configuration" "staging" {
  count  = local.manage_staging_lifecycle ? 1 : 0
  bucket = var.staging_bucket_name

  rule {
    id     = "expire-staged-logs"
    status = "Enabled"

    filter {
      prefix = "processed-logs"
    }

    expiration {
      days = var.staging_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }

  dynamic "rule" {
    for_each = local.extra_staging_prefixes
    content {
      id     = "expire-synthetic-staging"
      status = "Enabled"

      filter {
        prefix = rule.value
      }

      expiration {
        days = var.staging_retention_days
      }

      noncurrent_version_expiration {
        noncurrent_days = 1
      }
    }
  }

  rule {
    id     = "expire-raw-logs"
    status = "Enabled"

    filter {
      prefix = "AWSLogs/"
    }

    expiration {
      days = var.raw_log_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }
}

# ============================================================================
# Bucket Policies — Deny insecure transport (both buckets)
# TLS-only transport is the baseline control on any bucket holding tenant data.
# ============================================================================
data "aws_iam_policy_document" "deny_insecure_transport_data" {
  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    # Resource-policy Deny statements must name principals to be valid
    # (IAM role policies don't need this, but bucket policies do).
    principals {
      type        = "*"
      identifiers = ["*"]
    }

    actions = ["s3:*"]

    # NOTE: only THIS bucket's resources — an S3 bucket policy may not
    # reference another bucket's ARN (MalformedPolicy: invalid resource).
    resources = [
      local.data_arn,
      "${local.data_arn}/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid     = "DenyNonTlsObjectUploads"
    effect  = "Deny"
    actions = ["s3:PutObject"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    resources = [
      "${local.data_arn}/*",
    ]

    # Deny any upload negotiated below TLS 1.2 (the S3 analog of an account
    # account's min_tls_version = TLS1_2). NumericLessThan is the canonical
    # AWS pattern here: it only matches when a TlsVersion key exists and is
    # below 1.2, so it denies TLS 1.0/1.1 exactly - plain HTTP is already
    # denied by DenyInsecureTransport above (no TlsVersion key to match).
    condition {
      test     = "NumericLessThan"
      variable = "s3:TlsVersion"
      values   = ["1.2"]
    }
  }
}

# Same TLS-only control scoped to the BACKUP bucket only (bucket policies
# cannot reference another bucket's resources).
data "aws_iam_policy_document" "deny_insecure_transport_backup" {
  statement {
    sid    = "DenyInsecureTransport"
    effect = "Deny"

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    actions = ["s3:*"]

    resources = [
      local.backup_arn,
      "${local.backup_arn}/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  statement {
    sid     = "DenyNonTlsObjectUploads"
    effect  = "Deny"
    actions = ["s3:PutObject"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    resources = [
      "${local.backup_arn}/*",
    ]

    condition {
      test     = "NumericLessThan"
      variable = "s3:TlsVersion"
      values   = ["1.2"]
    }
  }
}

resource "aws_s3_bucket_policy" "data" {
  bucket = aws_s3_bucket.data.id
  policy = data.aws_iam_policy_document.deny_insecure_transport_data.json

  # Why: The two bucket-policy resources are separate to keep a future
  #      data-specific statement (e.g., VPC-endpoint-only access using the
  #      reserved vpc_id/subnet_id inputs) isolated from the backup bucket.
  # Without it: Any bucket-specific policy addition forces a re-apply on
  #             both buckets, and the shared document becomes a merge
  #             conflict magnet.
  depends_on = [aws_s3_bucket_public_access_block.data]
}

resource "aws_s3_bucket_policy" "backup" {
  bucket = aws_s3_bucket.backup.id
  policy = data.aws_iam_policy_document.deny_insecure_transport_backup.json

  # Block Public Access must exist before a bucket policy is attached, or
  # AWS rejects the policy with "Public policy" detection heuristics.
  depends_on = [aws_s3_bucket_public_access_block.backup]
}

# ============================================================================
# Per-Tenant Policy Documents (data sources, not resources)
# Multi-tenancy is the AWS port's headline capability: "disk storage scalable like crazy" means a tenant can have an arbitrary number of parts under…
# ============================================================================
data "aws_iam_policy_document" "tenant" {
  for_each = { for t in var.tenants : t => t }

  # ListBucket scoped to the tenant's prefix.
  # Why: s3:ListBucket is a BUCKET-level action - without the prefix
  #      condition it would list every tenant's keys. The two condition
  #      values are the canonical AWS pattern: the bare prefix (list the
  #      tenant root) and the wildcard (list any sub-prefix under it,
  #      which is how parts enumeration actually lists "data/<tenant>/...").
  #      The condition is what turns a shared bucket into per-tenant
  #      isolation: the List succeeds only for keys under data/<tenant>/.
  # Without it: A tenant could list a sibling tenant's object keys (names,
  #             sizes, timestamps) even though object reads stay denied -
  #             or, with only the bare form, could not list its own parts
  #             deeper than the prefix root.
  statement {
    sid       = "TenantListOwnPrefix"
    effect    = "Allow"
    actions   = ["s3:ListBucket"]
    resources = [local.data_arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["${local.data_prefix_normalized}${each.key}", "${local.data_prefix_normalized}${each.key}/*"]
    }
  }

  # Read/write/delete on the tenant's own objects.
  # Why: A tenant needs the full CRUD set on its own prefix: external
  #      shippers write raw logs, tenant jobs read them, and tenant-owned
  #      tooling may prune. The resource ARN ends in /* so the grant is
  #      object-scoped within data/<tenant>/ - never the bucket itself.
  # Without it: Tenants could read each other's parts; any tenant with a
  #             leaked key becomes a full-data-bucket read/write.
  statement {
    sid       = "TenantObjectsInOwnPrefix"
    effect    = "Allow"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
    resources = ["arn:aws:s3:::${var.data_bucket_name}/${local.data_prefix_normalized}${each.key}/*"]
  }
}
