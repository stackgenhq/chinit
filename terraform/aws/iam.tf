# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# IAM — the instance role chinit boots under (AWS)
# ============================================================================
# One role serves all four node roles (chinit adapts by role in-process);
# the policy grants exactly what chinit + ClickHouse call at boot and in
# recurring jobs — nothing more. This is the module's PLOP surface: every
# statement names its essential resource.
# ============================================================================

# EC2 assume-role trust: the instance profile hands these credentials to
# the OS via the metadata service (IMDSv2 required in the templates).
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

resource "aws_iam_role" "node" {
  name               = "${local.name_prefix}-clickhouse-node"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume_role.json

  tags = local.common_tags
}

resource "aws_iam_instance_profile" "node" {
  name = "${local.name_prefix}-clickhouse-node"
  role = aws_iam_role.node.name

  tags = local.common_tags
}

# ----------------------------------------------------------------------------
# The chinit policy: bundle read, secrets read, claims table, DNS write,
# self-heal metrics, own-volume growth
# ----------------------------------------------------------------------------
data "aws_iam_policy_document" "node_chinit" {
  # The config bundle (specs + XMLs + chi.yaml + keeper.yaml): every
  # role fetches it at boot. Read-only — Terraform is the bundle's only
  # writer.
  statement {
    sid    = "ReadConfigBundle"
    effect = "Allow"

    actions = ["s3:GetObject"]

    resources = [
      "${aws_s3_bucket.bootstrap.arn}/bootstrap/*",
    ]
  }

  # Passwords (names only reach the nodes; this is the value fetch).
  statement {
    sid    = "ReadPasswordSecrets"
    effect = "Allow"

    actions = ["secretsmanager:GetSecretValue"]

    resources = [
      aws_secretsmanager_secret.default_password.arn,
      aws_secretsmanager_secret.admin_password.arn,
      aws_secretsmanager_secret.writer_password.arn,
    ]
  }

  # Ordinal leases: claim (PutItem conditional), renew (UpdateItem with
  # the instance_id condition), release (DeleteItem guarded), and the
  # live-claims inventory (Scan — the table is PAY_PER_REQUEST and holds
  # only fleet rows).
  statement {
    sid    = "OrdinalLeases"
    effect = "Allow"

    actions = [
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:DeleteItem",
      "dynamodb:GetItem",
      "dynamodb:Scan",
    ]

    resources = [aws_dynamodb_table.node_claims.arn]
  }

  # Private DNS: resolve the zone (ListHostedZonesByName — Route53 has
  # no resource-level permissions for it) + write this fleet's records.
  statement {
    sid    = "ZoneLookup"
    effect = "Allow"

    actions = [
      "route53:ListHostedZonesByName",
      "route53:GetHostedZone",
    ]

    resources = ["*"]
  }

  statement {
    sid    = "RecordWrite"
    effect = "Allow"

    actions = [
      "route53:ChangeResourceRecordSets",
      "route53:ListResourceRecordSets",
    ]

    resources = [aws_route53_zone.private.arn]
  }

  # Self-heal metrics (keeper liveness, disk-autoscale outcomes).
  # CloudWatch has no resource-level permissions; the namespace
  # condition is the only boundary AWS offers — pinned to chinit's.
  statement {
    sid    = "PushOwnHealthMetrics"
    effect = "Allow"

    actions = ["cloudwatch:PutMetricData"]

    resources = ["*"]

    condition {
      test     = "StringEquals"
      variable = "cloudwatch:namespace"
      values = [
        "ClickHouse/Keeper",
        "ClickHouse/SelfHeal",
      ]
    }
  }

  # Own-volume growth (the disk-autoscale job): find the volume
  # (Describe* has no resource-level support — the call's own filter is
  # the scope: attachment by THIS instance id), grow it online, and
  # watch the modification converge.
  statement {
    sid    = "GrowOwnDataVolume"
    effect = "Allow"

    actions = [
      "ec2:DescribeVolumes",
      "ec2:DescribeVolumesModifications",
    ]

    resources = ["*"]
  }

  statement {
    sid    = "ModifyOwnTaggedVolume"
    effect = "Allow"

    actions = ["ec2:ModifyVolume"]

    # ASG-created volume ids are unknowable at policy time: the tag
    # condition (the launch templates stamp NodeType on every volume
    # they create) keeps the grant inert for anything untagged, and
    # chinit only ever passes the volume attached to its own instance.
    resources = ["arn:aws:ec2:${var.region}:*:volume/*"]

    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/NodeType"
      values   = ["clickhouse"]
    }
  }

  # The S3 cold tier + backups (ClickHouse's own disks, not chinit):
  # writers/readers write parts under the data prefix; the backup node
  # reads data and writes/deletes under the backup prefix. The
  # bootstrap prefix is NOT writable (the bundle's integrity is the
  # SHA gate — nodes must not be able to tamper with what they boot
  # from).
  statement {
    sid    = "ColdTierObjects"
    effect = "Allow"

    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:ListBucket",
    ]

    resources = concat([
      aws_s3_bucket.data.arn,
      "${aws_s3_bucket.data.arn}/${var.s3_data_prefix}/*",
      aws_s3_bucket.backup.arn,
      "${aws_s3_bucket.backup.arn}/*",
    ], var.cas_enabled ? ["${aws_s3_bucket.data.arn}/${var.cas_prefix}/*"] : [])
  }
}

resource "aws_iam_role_policy" "node_chinit" {
  name   = "chinit-node"
  role   = aws_iam_role.node.id
  policy = data.aws_iam_policy_document.node_chinit.json
}

# AWS-managed: SSM access (no bastion/SSH needed) — the standard agent
# policy every fleet node should carry anyway.
resource "aws_iam_role_policy_attachment" "ssm" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
