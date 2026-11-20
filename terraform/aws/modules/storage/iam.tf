# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Node IAM Roles, Instance Profiles & Least-Privilege S3 Policies
# ============================================================================
# Why: ClickHouse's S3 disk authenticates with
#      <use_environment_credentials>, which picks up the EC2 instance role
#      from the instance metadata service - workload identity with no
#      static keys (the managed-identity pattern). No static keys ever exist, so there is nothing to leak in
#      cloud-init logs, AMI images, or backups.
# Without it: ClickHouse cannot authenticate to S3; every part write and
#             read fails. The only alternative is access keys embedded in
#      config, which are the classic leaked-credential incident.
# ============================================================================

# ----------------------------------------------------------------------------
# EC2 Assume-Role Policy (shared by all node roles)
# ----------------------------------------------------------------------------
# Why: The roles are assumed by EC2 instances, so the trust policy must
#      name the ec2.amazonaws.com service; the instance profile is what
#      hands the role credentials to the OS via the metadata service.
# Without it: The role can be attached to nothing - EC2 cannot assume it,
#             and no instance ever receives S3 credentials.
# ----------------------------------------------------------------------------
data "aws_iam_policy_document" "ec2_assume_role" {
  statement {
    sid     = "AllowEc2AssumeRole"
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

# ----------------------------------------------------------------------------
# Per-Role S3 Permission Matrix (see locals.tf)
# ----------------------------------------------------------------------------
# Why: All four roles talk to the same two buckets, so their policies are
#      permutations of a small set of (bucket-scope, action-set) choices.
#      Writing them as a matrix keeps the intent visible and makes the
#      least-privilege deltas between roles diffable at a glance.
# Without it: Four hand-rolled policies that drift apart over time - the
#             writer role gets the backup role's permissions (or vice
#             versa) and nobody notices until an audit.
# ----------------------------------------------------------------------------

# ----------------------------------------------------------------------------
# Per-Role S3 Policy Documents
# ----------------------------------------------------------------------------
# Why: One policy document per role, assembled from the matrix above. The
#      statements are emitted only when the role actually needs them
#      (keeper gets a single bootstrap-read statement, nothing else).
# Without it: Either every node shares one fat policy (keeper could read
#             tenant data; backup role could write parts) or four bespoke
#             policies that drift.
# ----------------------------------------------------------------------------
data "aws_iam_policy_document" "node_s3" {
  for_each = { for role in local.node_roles : role => role }

  # Tenant-data object access, scoped to this role's allowed prefixes.
  # Why: The resource ARN is bounded to the data prefix ("data/*"), NOT the
  #      bucket root: the bootstrap/ prefix lives in the same bucket, and a
  #      bucket-root grant would hand writer/reader delete rights over the
  #      stage-2 cloud-init objects. The "all" scope marker renders as a
  #      single "*" (every tenant prefix, i.e. data/*), never as the
  #      literal path segment "all".
  # Without it: A compromised writer could tamper with or delete the
  #             bootstrap payload every other node boots from.
  dynamic "statement" {
    for_each = length(local.role_permissions[each.key].data_object_actions) > 0 ? [1] : []
    content {
      sid     = "TenantDataObjects"
      effect  = "Allow"
      actions = local.role_permissions[each.key].data_object_actions
      resources = [for scope in local.role_permissions[each.key].data_prefix_scopes :
        "arn:aws:s3:::${var.data_bucket_name}/${scope == "all" ? "${local.data_prefix_normalized}*" : "${local.data_prefix_normalized}${scope}/*"}"
      ]
    }
  }

  # ClickHouse 26.8's S3 disk writes sentinel/marker objects at the bucket
  # ROOT (szl/<id>, kgg/<id>, ... — several undocumented prefixes, so a
  # per-prefix grant is whack-a-mole). The disk treats the whole bucket as
  # its namespace, so writer/reader/backup need root object access.
  # Security trade-off (deliberate): nodes can now write anywhere in the
  # data bucket — matching the managed-identity pattern where the node identity
  # full container access. The isolation that matters (tenant from tenant)
  # is enforced by per-tenant prefixes at the ClickHouse layer; the per-
  # tenant IAM policy documents (tenant_policy_documents output) remain the
  # hook for tenant-scoped grants to EXTERNAL principals. The bootstrap
  # prefix is still protected: keeper (bootstrap-read-only role) and any
  # future non-node roles never receive this grant.
  dynamic "statement" {
    for_each = length(local.role_permissions[each.key].data_object_actions) > 0 ? [1] : []
    content {
      sid     = "S3DiskBucketRootMarkers"
      effect  = "Allow"
      actions = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
      resources = [
        "${local.data_arn}/*",
        "${local.cas_arn}/*",
      ]
    }
  }

  # Staging-prefix drain (writers + readers, only when the staging bucket is
  # wired): the S3Queue engine's after_processing='delete' removes each
  # staged file the moment it is ingested, so the prefix holds only the
  # in-flight window instead of a lifecycle-day buffer.
  # Why: without s3:DeleteObject the setting silently degrades — files
  #      drain into ClickHouse but pile up in S3 until the lifecycle rule
  #      eventually deletes them (dead staging storage grows linearly with
  #      ingest volume; the grant collapses it to the in-flight window only).
  # Why Get+Delete ONLY on <prefix>/* (never bucket root): the staging
  #      bucket also holds the immutable AWSLogs/ landing zone and the
  #      bucket-root grant would let a compromised writer erase raw logs.
  # Without it: every drain leaves the staging bill growing linearly.
  dynamic "statement" {
    for_each = contains(local.staging_drain_roles, each.key) ? [1] : []
    content {
      sid     = "StagingDrainObjects"
      effect  = "Allow"
      actions = ["s3:GetObject", "s3:DeleteObject"]
      resources = [
        "${local.staging_arn}/${var.staging_key_prefix}/*",
      ]
    }
  }

  dynamic "statement" {
    for_each = contains(local.staging_drain_roles, each.key) ? [1] : []
    content {
      sid       = "StagingDrainListBucket"
      effect    = "Allow"
      actions   = ["s3:ListBucket"]
      resources = [local.staging_arn]
      condition {
        test     = "StringLike"
        variable = "s3:prefix"
        values   = ["${var.staging_key_prefix}/*"]
      }
    }
  }

  # CAS pool access (only when the CAS cold tier is enabled): CAS needs
  # full object CRUD on the pool prefix - GC deletes unreferenced blobs and
  # re-creations race with it by design. Readers get the same rights as
  # writers here: every replica mounts the pool read-write (fetch relinks a
  # part by publishing a ref, not by copying bytes). Keeper stays excluded.
  dynamic "statement" {
    for_each = var.cas_prefix != "" && contains(["writer", "reader", "backup"], each.key) ? [1] : []
    content {
      sid     = "CasPoolObjects"
      effect  = "Allow"
      actions = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"]
      resources = [
        "arn:aws:s3:::${var.data_bucket_name}/${local.cas_prefix_normalized}*",
        "${local.cas_arn}/${local.cas_prefix_normalized}*",
      ]
    }
  }

  # CAS pool listing: the GC folds the ref log by listing `<pool>/gc/...`
  # and `roots/`; mounts list the pool root to verify emptiness on
  # decommission. Scoped by prefix condition so CAS nodes still cannot
  # enumerate tenant keys outside the pool.
  dynamic "statement" {
    for_each = var.cas_prefix != "" && contains(["writer", "reader", "backup"], each.key) ? [1] : []
    content {
      sid       = "CasPoolList"
      effect    = "Allow"
      actions   = ["s3:ListBucket"]
      resources = [local.data_arn, local.cas_arn]

      condition {
        test     = "StringLike"
        variable = "s3:prefix"
        # both "cas/" (listing the pool root) and "cas/<...>" children
        values = [local.cas_prefix_normalized, "${local.cas_prefix_normalized}*"]
      }
    }
  }

  # Bucket-level ListBucket (needed for parts enumeration on boot).
  dynamic "statement" {
    for_each = length(local.role_permissions[each.key].data_bucket_actions) > 0 ? [1] : []
    content {
      sid       = "TenantDataListBucket"
      effect    = "Allow"
      actions   = local.role_permissions[each.key].data_bucket_actions
      resources = [local.data_arn, local.cas_arn]
    }
  }

  # Backup bucket object access (writer: put only; backup: full CRUD).
  dynamic "statement" {
    for_each = length(local.role_permissions[each.key].backup_object_actions) > 0 ? [1] : []
    content {
      sid       = "BackupBucketObjects"
      effect    = "Allow"
      actions   = local.role_permissions[each.key].backup_object_actions
      resources = ["${local.backup_arn}/*"]
    }
  }

  # Backup bucket LIST (backup role only): clickhouse-backup's retention
  # pruning (RemoveOldBackupsRemote) enumerates the bucket before deleting
  # expired backups, and `list remote` walks it for restore discovery.
  # Object-level CRUD alone makes every such call 403 (the documented
  # finding: every create_remote logged "BackupList bd.Walk return error:
  # ... AccessDenied s3:ListBucket" — backups uploaded fine but retention
  # silently never pruned and remote listings came back empty).
  dynamic "statement" {
    for_each = each.key == "backup" ? [1] : []
    content {
      sid       = "BackupBucketList"
      effect    = "Allow"
      actions   = ["s3:ListBucket"]
      resources = [local.backup_arn]
    }
  }

  # Bootstrap prefix read: stage-2 cloud-init is fetched at first boot.
  dynamic "statement" {
    for_each = local.role_permissions[each.key].bootstrap_read ? [1] : []
    content {
      sid       = "ReadBootstrapPrefix"
      effect    = "Allow"
      actions   = ["s3:GetObject"]
      resources = local.bootstrap_objects
    }
  }

  # Bootstrap prefix LIST, keeper-scoped: chinit's spec-sync/config-check
  # convergence enumerates the bootstrap prefix when it re-fetches the
  # bundle. Writers/readers hold bucket-wide ListBucket for
  # parts enumeration, but the keeper role is deliberately data-empty —
  # without the grant its convergence silently failed AccessDenied on
  # ListObjectsV2 (the live first-refresh finding; the boot-time GetObject
  # fetch never lists, so first boot worked while every later keeper
  # converge failed). Scoped by prefix condition so the keeper still cannot
  # enumerate tenant data or backups.
  dynamic "statement" {
    for_each = local.role_permissions[each.key].bootstrap_read ? [1] : []
    content {
      sid       = "ListBootstrapPrefix"
      effect    = "Allow"
      actions   = ["s3:ListBucket"]
      resources = [local.data_arn, local.cas_arn]

      condition {
        test     = "StringLike"
        variable = "s3:prefix"
        values   = ["${var.bootstrap_prefix}/*"]
      }
    }
  }
}

# ----------------------------------------------------------------------------
# IAM Roles (one per node role)
# ----------------------------------------------------------------------------
# Why: A dedicated role per node role is what makes the least-privilege
#      matrix above enforceable. If all nodes shared one role, a compromised
#      keeper (which holds cluster-coordination state, not tenant data)
#      would immediately hold read/write over every tenant's parts, and a
#      spot reader would hold backup-delete rights.
#      Names embed cluster_name so a multi-cluster account can distinguish
#      its roles.
# Without it: Either one shared role with union-of-all-needs permissions
#             (max blast radius) or manual per-node policy wrangling.
# ----------------------------------------------------------------------------
resource "aws_iam_role" "node" {
  for_each = { for role in local.node_roles : role => role }

  name               = "${var.cluster_name}-clickhouse-${each.key}"
  description        = "ClickHouse ${each.key} node role for cluster ${var.cluster_name} (S3 disk access via instance role credentials)"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json

  tags = merge(local.tags, {
    Name = "${var.cluster_name}-clickhouse-${each.key}"
  })
}

# ----------------------------------------------------------------------------
# IAM Instance Profiles (one per node role)
# ----------------------------------------------------------------------------
# Why: EC2 cannot use a role directly - the instance profile is the
#      container that hands the role to the instance metadata service,
#      which is where <use_environment_credentials> reads it from. One
#      profile per role so the compute module can hand each ASG exactly
#      the right profile by name.
# Without it: Instances boot without credentials; ClickHouse S3 disks fail
#             to mount and cloud-init S3 fetches get 403s.
# ----------------------------------------------------------------------------
resource "aws_iam_instance_profile" "node" {
  for_each = { for role in local.node_roles : role => role }

  name = "${var.cluster_name}-clickhouse-${each.key}"
  role = aws_iam_role.node[each.key].name

  tags = merge(local.tags, {
    Name = "${var.cluster_name}-clickhouse-${each.key}"
  })
}

# ----------------------------------------------------------------------------
# Role Policies — least-privilege S3 grants
# ----------------------------------------------------------------------------
# Why: The policy documents above are attached per-role so each node role
#      holds only its slice of the bucket permissions. The AWS-managed
#      SSM/CloudWatch policies are attached separately (below) since they
#      are account-standard and free.
# Without it: Nodes boot with either no S3 access (parts fail) or full
#             bucket access (tenant isolation meaningless).
# ----------------------------------------------------------------------------
resource "aws_iam_role_policy" "node_s3" {
  for_each = { for role in local.node_roles : role => role }

  name   = "s3-disk-access"
  role   = aws_iam_role.node[each.key].id
  policy = data.aws_iam_policy_document.node_s3[each.key].json
}

# ----------------------------------------------------------------------------
# Self-Healing Policy (metrics + online volume growth)
# ----------------------------------------------------------------------------
# Why: The chinit recurring jobs are the cluster's self-healing layer, and
#      two of them need permissions beyond S3/SSM/agent managed policies:
#        1. keeper-health pushes the keeper_up CloudWatch metric (the
#           chinit cutover lost the bash-era `aws cloudwatch put-metric-data`
#           cron — keeper_down went INSUFFICIENT_DATA blind on the
#           for exactly this reason). The agent-managed policy covers the
#           agent's own PutMetricData but NOT arbitrary CLI/SDK pushes.
#        2. disk-autoscale grows the node's own EBS data volume + ext4
#           online when effective free space (volume minus the 10 GiB
#           keep_free_space reservation) drops below the floor — the
#           follow-up incident where keep_free_space on a 32 GiB volume
#           left ~1 MiB effective: merges hit NOT_ENOUGH_SPACE, parts
#           climbed past 1800, inserts were minutes from the
#           parts_to_throw_insert hard-reject at 3000.
#
# LEAST-PRIVILEGE SHAPE (PLOP — every grant names its essential resource):
#   - cloudwatch:PutMetricData: CloudWatch has NO resource-level
#     permissions — "the namespace condition" is the ONLY boundary AWS
#     offers. Restrict to the exact two namespaces chinit pushes
#     (keeper liveness + self-heal outcomes); a node cannot use this
#     grant to pollute any other namespace's alarms or dashboards.
#   - ec2:DescribeVolumes: read-only discovery the grower needs to find
#     its own volume (attachment.instance-id filter). Describe* has no
#     resource-level support — "the resource list is "*" BY AWS DESIGN",
#     the filter is in the CALL, not the policy. DescribeInstances is
#     NOT granted: the grower derives identity from IMDS/state, never
#     from a DescribeInstances call.
#   - ec2:ModifyVolume: the ONLY mutation, scoped to the cluster's own
#     volume ARNs AND the NodeType tag condition. Volume IDs are
#     ASG-created (unknowable at policy time) — the account+volume
#     prefix scopes to this deployment's volumes; the tag condition
#     (launch-template tag_specifications set NodeType on every fleet
#     volume) keeps the grant inert for anything else in the account;
#     and the subcommand itself only ever passes the volume IT found
#     attached to its own instance id. Three independent gates.
#   - ec2:ModifyVolume is also bounded in CODE: the subcommand caps at
#     the 256 GiB ceiling and only grows (a shrink would be a different
#     action entirely).
# Without it: keeper liveness is invisible (keeper_down alarm can never
#             fire), and the only disk-pressure remedy is a human with a
#             console while the cluster degrades toward insert rejection.
# ----------------------------------------------------------------------------
# ----------------------------------------------------------------------------
# Vertical Autoscale Policy (the chinit `autoscale` coordinator's grants)
# ----------------------------------------------------------------------------
# Why: the chinit `autoscale` coordinator runs ON the nodes (a timer
#      unit), reading the fleet's own pushed metrics and stepping
#      instance types when a role's ladder is configured. The grants
#      ride the node roles so the coordinator's identity is its own
#      IMDS-settled instance — never a value an out-of-band component
#      could hold stale.
#        1. cloudwatch:GetMetricStatistics: the signals read (the same
#           EMF metrics the alarms evaluate — GetMetric* has no
#           resource-level support; the namespace condition is the
#           only boundary AWS offers, and it scopes to the exact
#           three namespaces the stack's agents push).
#        2. The keeper's self-resize: describe/stop/start/modify
#           scoped to the role's own NodeType tag (keeper-0 resizes
#           ITSELF; the writer's describe is the fleet-type read).
#        3. The writers' fleet roll (the ordinal-0 holder): the
#           launch-template version + the ASG's instance refresh —
#           the MinHealthyPercentage/AutoRollback discipline lives in
#           the coordinator's calls, not the IAM.
# Without it: the autoscale timer logs
#             "provider aws lacks the metric-read capability" (or the
#             resize equivalent) and the vertical path silently no-ops.
data "aws_iam_policy_document" "node_vertical_autoscale" {
  for_each = { for role in local.node_roles : role => role }

  # The signals read — the breach decision's input.
  statement {
    sid    = "ReadOwnFleetMetrics"
    effect = "Allow"

    actions   = ["cloudwatch:GetMetricStatistics", "cloudwatch:ListMetrics"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "cloudwatch:namespace"
      values = [
        "ClickHouse/Keeper", # the ensemble's own signals
        "ClickHouse/Node",   # the node agent's CPU series
        "ClickHouse/Server", # the writers' fleet signals
      ]
    }
  }

  # Read-only instance discovery: DescribeInstances has no resource-level
  # support in AWS IAM ("*" is the only legal resource and tag conditions
  # do not match DescribeInstances). Kept separate from the mutating
  # actions below so DescribeInstances never fails when filtered by tag.
  statement {
    sid       = "DescribeClusterInstances"
    effect    = "Allow"
    actions   = ["ec2:DescribeInstances"]
    resources = ["*"]
  }

  # The keeper's self-resize + the writer's fleet-type read: the trio
  # is scoped to the role's own NodeType tag — a keeper can only step
  # keeper-tagged instances (itself), a writer only writer-tagged.
  statement {
    sid    = "ResizeOwnRoleInstances"
    effect = "Allow"

    actions   = ["ec2:DescribeInstances", "ec2:StopInstances", "ec2:StartInstances", "ec2:ModifyInstanceAttribute"]
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/NodeType"
      values   = [each.key]
    }
  }

  # On-demand backup replica wake (writers only — the ordinal-0
  # maintenance holder runs `chinit backup-wake` on the backup cron
  # schedule and starts the stopped backup-0 EC2 instance).
  dynamic "statement" {
    for_each = contains(["writer"], each.key) ? [1] : []

    content {
      sid       = "WakeOnDemandBackupReplica"
      effect    = "Allow"
      actions   = ["ec2:StartInstances"]
      resources = ["*"]

      condition {
        test     = "StringEquals"
        variable = "ec2:ResourceTag/NodeType"
        values   = ["backup"]
      }
    }
  }

  # The writers' fleet roll (writers only): the launch-template
  # version (cluster-shaped) and the ASG's refresh. Both are
  # name-bound resources the condition cannot scope per-instance;
  # the ordinal-0 gate + cooldown pace the calls.
  dynamic "statement" {
    for_each = contains(["writer"], each.key) ? [1] : []

    content {
      sid    = "WriterFleetRoll"
      effect = "Allow"

      actions = [
        "ec2:CreateLaunchTemplateVersion",
        "autoscaling:StartInstanceRefresh",
      ]
      resources = ["*"]
    }
  }

  # The orphaned-resource sweep (writers only — the ordinal-0
  # maintenance holder runs the daily census): Describe* has no
  # resource-level support ("*" is the only legal value — the call's
  # filters are the scope); DeleteVolume is the sweep's single
  # mutation and the untagged+unattached+aged criteria live in the
  # job, not the IAM.
  dynamic "statement" {
    for_each = contains(["writer"], each.key) ? [1] : []

    content {
      sid    = "OrphanSweep"
      effect = "Allow"

      actions = [
        "ec2:DescribeVolumes",
        "ec2:DeleteVolume",
        "ec2:DescribeAddresses",
        "ec2:DescribeSnapshots",
      ]
      resources = ["*"]
    }
  }
}

resource "aws_iam_role_policy" "node_vertical_autoscale" {
  # Backup replicas run no autoscale (a durability node; the units
  # matrix has no autoscale.service for the backup role) — same shape
  # as node_self_heal: the backup simply gets no policy.
  for_each = { for role in local.node_roles : role => role if role != "backup" }

  name   = "vertical-autoscale"
  role   = aws_iam_role.node[each.key].id
  policy = data.aws_iam_policy_document.node_vertical_autoscale[each.key].json
}

data "aws_iam_policy_document" "node_self_heal" {
  # Per-role least privilege: each role gets ONLY the statements its
  # chinit units actually execute (the units matrix is the contract —
  # keepers run keeper-health; writers/readers run disk-autoscale;
  # backup replicas push BackupSuccess to ClickHouse/SelfHeal and
  # self-stop when backup_config.on_demand is enabled).
  for_each = { for role in local.node_roles : role => role }

  # keeper_up liveness push (keepers), disk-autoscale outcome gauge
  # (writers/readers), and BackupSuccess outcome gauge (backup).
  statement {
    sid    = "PushOwnHealthMetrics"
    effect = "Allow"

    actions = ["cloudwatch:PutMetricData"]

    # CloudWatch actions do not support resource-level permissions —
    # "*" is the only legal value; the namespace condition below is
    # the actual boundary (the only one AWS provides for this
    # action).
    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "cloudwatch:namespace"
      values = [
        "ClickHouse/Keeper",   # keeper-health's keeper_up liveness push
        "ClickHouse/SelfHeal", # disk-autoscale + backup-daily BackupSuccess outcome gauge
      ]
    }
  }

  # On-demand backup replica self-stop (backup role only): after
  # `chinit backup-daily` completes create/upload/delete-local and
  # publishes BackupSuccess, it stops its own EC2 instance (the
  # StatefulSet 0<->1 pattern).
  dynamic "statement" {
    for_each = contains(["backup"], each.key) ? [1] : []
    content {
      sid       = "DescribeBackupInstanceForSelfStop"
      effect    = "Allow"
      actions   = ["ec2:DescribeInstances"]
      resources = ["*"]
    }
  }

  dynamic "statement" {
    for_each = contains(["backup"], each.key) ? [1] : []
    content {
      sid       = "StopOwnBackupInstance"
      effect    = "Allow"
      actions   = ["ec2:StopInstances"]
      resources = ["*"]

      condition {
        test     = "StringEquals"
        variable = "ec2:ResourceTag/NodeType"
        values   = ["backup"]
      }
    }
  }

  # Online data-volume growth — disk-autoscale's unit (writers and
  # readers ONLY: keepers have no chinit-disk-autoscale unit, and the
  # keeper raft volume is a STANDALONE terraform-managed EBS volume
  # whose size is a deliberate static choice (vertical headroom is
  # the keeper scaling path), never an automatic growth target).
  dynamic "statement" {
    for_each = contains(["writer", "reader"], each.key) ? [1] : []
    content {
      sid    = "FindOwnDataVolume"
      effect = "Allow"

      # Read-only volume discovery: the grower filters by its OWN
      # instance-id attachment — the call's filter is the resource
      # scope. ec2:DescribeVolumes does not support resource-level
      # permissions ("*" is the only legal value); the action reads
      # volume metadata only and cannot mutate anything.
      actions   = ["ec2:DescribeVolumes"]
      resources = ["*"]
    }
  }

  dynamic "statement" {
    for_each = contains(["writer", "reader"], each.key) ? [1] : []
    content {
      sid    = "WatchOwnVolumeModification"
      effect = "Allow"

      # Read-only progress polling: the grower waits on its own
      # ModifyVolume operation after issuing it (the modification
      # reaches its new size asynchronously). DescribeVolumesModifications
      # is a read-only Describe* action with no resource-level support
      # ("*" is the only legal value). The finding: without
      # this grant the grower logged "modification wait ended early:
      # DescribeVolumesModifications ... UnauthorizedOperation" and
      # proceeded BLIND — the grow still succeeded, but the unit could
      # not verify progress and filesystem resize raced the volume.
      actions   = ["ec2:DescribeVolumesModifications"]
      resources = ["*"]
    }
  }

  dynamic "statement" {
    for_each = contains(["writer", "reader"], each.key) ? [1] : []
    content {
      sid    = "GrowOwnDataVolume"
      effect = "Allow"

      # The single mutation: grow the data volume online. No
      # ModifyInstance (identity is IMDS-derived), no DetachVolume
      # (growth is in-place), no DeleteVolume (growth never destroys
      # — a shrink would be a different action entirely).
      actions = ["ec2:ModifyVolume"]

      # This deployment's volumes only: ASG-created volume IDs are
      # unknowable at policy time, so scope to the region's volume
      # namespace and gate on the NodeType tag the launch templates
      # stamp on every fleet volume (tag_specifications
      # resource_type="volume"). The subcommand adds its own gate —
      # it only ever passes the volume ID it found attached to its
      # OWN instance id.
      resources = ["arn:aws:ec2:${data.aws_region.current.name}:*:volume/*"]

      condition {
        test     = "StringEquals"
        variable = "ec2:ResourceTag/NodeType"
        values   = [each.key]
      }
    }
  }
}

data "aws_region" "current" {}

# ----------------------------------------------------------------------------
# Volume Release Policy (the chinit volume-release shutdown path's grants)
# ----------------------------------------------------------------------------
# Why: the chinit-volume-release.service unit's ExecStop deletes the
#      dying node's OWN stateless volumes inside systemd's shutdown
#      transaction, so a volume whose delete_on_termination flag cannot
#      act (a wedged instance, a drifted flag) leaves no orphan behind.
#      The grant is writer+reader ONLY — keepers and the backup replica
#      carry standalone Terraform-managed volumes whose persistence IS
#      the design (raft logs, backup lineage), and the subcommand's
#      role gate double-enforces the same boundary in code.
#
# LEAST-PRIVILEGE SHAPE:
#   - ec2:DescribeVolumes: read-only discovery filtered by the node's
#     OWN instance-id attachment (the call's filter is the scope;
#     Describe* has no resource-level support).
#   - ec2:DetachVolume + ec2:DeleteVolume: the two mutations, scoped
#     to this deployment's volume ARNs AND the NodeType tag condition
#     the launch templates stamp on every fleet volume
#     (tag_specifications resource_type="volume"). The subcommand only
#     ever passes the volume IDs it found attached to its OWN
#     instance id — three independent gates.
# Without it: the release path logs "AccessDenied" at shutdown and the
#             daily orphan sweep (untagged+unattached census) remains
#             the only reclaim — with up to 24h of billing per leak.
data "aws_iam_policy_document" "node_volume_release" {
  for_each = { for role in local.node_roles : role => role if contains(["writer", "reader"], role) }

  statement {
    sid    = "FindOwnVolumes"
    effect = "Allow"

    actions   = ["ec2:DescribeVolumes"]
    resources = ["*"]
  }

  statement {
    sid    = "ReleaseOwnVolumes"
    effect = "Allow"

    actions = [
      "ec2:DetachVolume",
      "ec2:DeleteVolume",
    ]

    # This deployment's volumes only: ASG-created volume IDs are
    # unknowable at policy time, so scope to the region's volume
    # namespace and gate on the NodeType tag the launch templates
    # stamp on every fleet volume. The tag condition keeps the grant
    # inert for anything else in the account; the subcommand adds
    # its own gate (it only ever passes the volume IDs it found
    # attached to its OWN instance id).
    resources = ["arn:aws:ec2:${data.aws_region.current.name}:*:volume/*"]

    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/NodeType"
      values   = [each.key]
    }
  }
}

resource "aws_iam_role_policy" "node_volume_release" {
  for_each = { for role in local.node_roles : role => role if contains(["writer", "reader"], role) }

  name   = "volume-release"
  role   = aws_iam_role.node[each.key].id
  policy = data.aws_iam_policy_document.node_volume_release[each.key].json
}

resource "aws_iam_role_policy" "node_self_heal" {
  for_each = { for role in local.node_roles : role => role }

  name   = "self-healing"
  role   = aws_iam_role.node[each.key].id
  policy = data.aws_iam_policy_document.node_self_heal[each.key].json
}

# ----------------------------------------------------------------------------
# AWS-Managed Policy Attachments (SSM + CloudWatch agent)
# ----------------------------------------------------------------------------
# Why: The nodes are managed via SSM Session Manager (SSH is disabled by
#      default per the root's network_config) and report metrics via the
#      CloudWatch agent. Attaching the AWS-managed policies
#      AmazonSSMManagedInstanceCore (SSM core + GetParameters) and
#      AmazonCloudWatchAgentInstallerPolicy (agent install + logs:
#      CreateLogStream/PutLogEvents) is free and standard - there is no
#      cost to attach managed policies and no permission surface to
#      maintain by hand.
# ----------------------------------------------------------------------------
# Managed policy attachments — every node role
# ----------------------------------------------------------------------------
# Why: All node roles need SSM (Session Manager access, no SSH/bastion) and
#      the CloudWatch agent policy (metrics/logs shipping). Kept as one
#      attachment loop over the role list so a new role only extends
#      local.node_roles.
# ----------------------------------------------------------------------------

resource "aws_iam_role_policy_attachment" "managed" {
  for_each = { for pair in setproduct(local.node_roles, local.managed_policy_arns) : "${pair[0]}|${pair[1]}" => pair }

  role       = aws_iam_role.node[each.value[0]].name
  policy_arn = each.value[1]
}
