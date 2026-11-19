# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# DNS + TLS Module — Route53 private zone (node discovery), public records
# (client endpoints), ACM cert (NLB termination) and ACME Let's Encrypt certs
# (DNS-01 via Route53) for interserver/client TLS.
# ============================================================================

# ----------------------------------------------------------------------------
# Private Hosted Zone — node discovery
# ClickHouse nodes must resolve each other's hostnames (writer-0, reader-0, keeper-0, backup-0) for cluster formation.
# ----------------------------------------------------------------------------
resource "aws_route53_zone" "private" {
  name = var.private_zone_name

  vpc {
    vpc_id = var.vpc_id
  }

  tags = merge(var.tags, {
    Name = var.private_zone_name
  })
}

# ----------------------------------------------------------------------------
# Static node A records — keepers, backup replica
# Keepers/backup are fixed-identity EC2 with static private IPs;
# ----------------------------------------------------------------------------
resource "aws_route53_record" "keeper" {
  count   = length(var.keeper_ips)
  zone_id = aws_route53_zone.private.zone_id
  name    = "${var.keeper_hostnames[count.index]}.${var.private_zone_name}"
  type    = "A"
  ttl     = 60
  records = [var.keeper_ips[count.index]]
}

resource "aws_route53_record" "backup" {
  count   = var.backup_replica_enabled ? 1 : 0
  zone_id = aws_route53_zone.private.zone_id
  name    = "${var.backup_hostname}.${var.private_zone_name}"
  type    = "A"
  ttl     = 60
  records = [var.backup_ip]
}

# ----------------------------------------------------------------------------
# ACM Certificate — supplied ARN (acm mode)
# ----------------------------------------------------------------------------
# Why: An existing ACM cert (e.g. *.example.io) is DNS-validated and
#      AWS-renews automatically — no Let's Encrypt/ACME needed.
#      NOTE: ACM certs with Export: DISABLED (AWS-managed) can ONLY be
#      attached to load balancers — exactly what acm mode does. The NLB
#      listener attachment is the validator: AWS rejects wrong-region,
#      non-existent, or non-ISSUED ARNs at apply time with a clear error.
#      (The aws_acm_certificate data source only searches by domain, which
#      cannot verify a specific ARN — so no data lookup here.)
# Without it: acm mode cannot terminate TLS; clients cannot get trusted certs.

# ----------------------------------------------------------------------------
# ACM Certificate (fallback: issue a new one when no ARN is provided)
# ----------------------------------------------------------------------------
# Why: When no pre-provisioned cert ARN is supplied, acm mode can still work
#      by issuing a new ACM cert — but the DNS validation record must land
#      in the same apply (Route53 zone + ACM validation), which couples the
#      stack to zone-management permissions. Prefer passing acm_certificate_arn.
resource "aws_acm_certificate" "nlb" {
  count = var.nlb_tls_mode == "acm" && var.acm_certificate_arn == null ? 1 : 0

  domain_name               = "*.${coalesce(var.acm_base_domain, var.public_zone_name)}"
  subject_alternative_names = [coalesce(var.acm_base_domain, var.public_zone_name)]
  validation_method         = "DNS"

  # NO create_before_destroy here, deliberately: OpenTofu's
  # ForcedCBDTransformer propagates CBD to every transitive dependency,
  # and this node's flag poisons the whole graph even when count=0
  # (pinned-ARN stacks keep the expand node, flag and all). That forced
  # create-before-destroy onto the static-private-IP writer/keeper
  # instances, whose replacements died on InvalidIPAddress.InUse —
  # the new node cannot bind the IP while the old one holds it. ACM
  # certs are ARN-addressed with no unique-name collision, so plain
  # destroy-then-create is safe here.

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Public zone A records for the load balancer endpoints
# writers.
# ----------------------------------------------------------------------------
# Zone stability: keying the LB
# records on this module's OWN data source made every plan with ANY
# pending change elsewhere mark zone_id "(known after apply)" and
# "force replacement" of both public records — a needless
# destroy+recreate of the live NLB endpoints (ch / ch-writers) on
# every bundle or spec edit, when the apply would only re-read the
# SAME zone id. The root module resolves the zone ONCE and passes
# public_zone_id in (the ACME path's existing input); the module data
# source stays as the fallback for stacks that don't pass the id
# (back-compat), but the record's zone_id is now a STABLE string.
data "aws_route53_zone" "public" {
  count = var.public_zone_name != null && var.public_zone_id == null ? 1 : 0
  name  = var.public_zone_name
}

locals {
  # The LB records' zone: the root-resolved id when provided (stable —
  # no per-plan data-source re-read), else this module's own lookup.
  public_lb_zone_id = var.public_zone_id != null ? var.public_zone_id : (length(data.aws_route53_zone.public) > 0 ? data.aws_route53_zone.public[0].zone_id : null)
}

resource "aws_route53_record" "writers_lb" {
  count   = local.public_lb_zone_id != null && var.create_public_records && var.writers_lb_dns_name != null ? 1 : 0
  zone_id = local.public_lb_zone_id
  # Configurable record label (must be a single label - the ACM wildcard
  # *.example.io covers exactly one level under the zone).
  name = var.writers_record_name
  type = "A"

  alias {
    name                   = var.writers_lb_dns_name
    zone_id                = var.writers_lb_zone_id
    evaluate_target_health = true
  }

}

resource "aws_route53_record" "readers_lb" {
  count   = local.public_lb_zone_id != null && var.create_public_records && var.readers_lb_dns_name != null ? 1 : 0
  zone_id = local.public_lb_zone_id
  name    = var.readers_record_name
  type    = "A"

  alias {
    name                   = var.readers_lb_dns_name
    zone_id                = var.readers_lb_zone_id
    evaluate_target_health = true
  }

}

# ============================================================================
# ACME (Let's Encrypt) — wildcard certs for ClickHouse-terminated TLS
# ============================================================================
# Why: Trusted certs terminate on ClickHouse itself
#      (ports 8443/9440) with the NLB as pure TCP passthrough. The ACME
#      provider is cloud-agnostic; the DNS challenge backend is
#      Route53 via an IAM user with change-record-set perms.
# Without it: passthrough mode has no trusted cert; interserver TLS still
#             works with self-signed but clients warn.
# ============================================================================

# Why: The ACME lego client needs static credentials to write the DNS-01 TXT
#      record into Route53. IAM instance roles are not usable from the
#      Terraform operator's machine. A dedicated least-privilege user scoped
#      to change-record-set on the single public zone is the dedicated
#      least-privilege ACME identity.
resource "aws_iam_user" "acme_dns" {
  count = local.acme_enabled ? 1 : 0
  name  = "${var.cluster_name}-acme-dns"

  tags = var.tags
}

resource "aws_iam_access_key" "acme_dns" {
  count      = local.acme_enabled ? 1 : 0
  user       = aws_iam_user.acme_dns[0].name
  depends_on = [aws_iam_user.acme_dns]
}

# only.
data "aws_iam_policy_document" "acme_dns" {
  count = local.acme_enabled ? 1 : 0

  statement {
    sid    = "AllowAcmeChallengeRecords"
    effect = "Allow"

    actions = [
      "route53:ChangeResourceRecordSets",
    ]

    resources = [
      "arn:aws:route53:::hostedzone/${var.public_zone_id}",
    ]
  }

  statement {
    sid    = "AllowListTargetZone"
    effect = "Allow"

    actions = [
      "route53:GetChange",
      "route53:GetHostedZone",
      "route53:ListResourceRecordSets",
    ]

    resources = ["*"]
  }
}

resource "aws_iam_user_policy" "acme_dns" {
  count  = local.acme_enabled ? 1 : 0
  name   = "${var.cluster_name}-acme-dns-challenge"
  user   = aws_iam_user.acme_dns[0].name
  policy = data.aws_iam_policy_document.acme_dns[0].json
}

resource "tls_private_key" "acme_account" {
  count     = local.acme_enabled ? 1 : 0
  algorithm = "RSA"
  rsa_bits  = 4096
}

resource "acme_registration" "main" {
  count           = local.acme_enabled ? 1 : 0
  account_key_pem = tls_private_key.acme_account[0].private_key_pem
  email_address   = var.acme_email
}

# Why: Wildcard *.cluster.domain covers every node hostname with one cert so
#      writers/readers/keepers share the same material.
resource "acme_certificate" "cluster" {
  count           = local.acme_enabled ? 1 : 0
  account_key_pem = acme_registration.main[0].account_key_pem

  common_name = "*.${var.cluster_name}.${var.public_zone_name}"

  subject_alternative_names = [
    "${var.cluster_name}.${var.public_zone_name}"
  ]

  # Why: Staging by default avoids Let's Encrypt production rate limits while
  #      the cluster is being rebuilt repeatedly during development.
  #      Set acme_environment = "production" in tfvars for trusted certs.
  #      The server URL is set on the provider in the root module.
  dns_challenge {
    provider = "route53"

    config = {
      AWS_ACCESS_KEY_ID     = aws_iam_access_key.acme_dns[0].id
      AWS_SECRET_ACCESS_KEY = aws_iam_access_key.acme_dns[0].secret
      AWS_REGION            = var.aws_region
      AWS_HOSTED_ZONE_ID    = var.public_zone_id
    }
  }

  depends_on = [aws_iam_user_policy.acme_dns]
}

# ----------------------------------------------------------------------------
# Store ACME certs in Secrets Manager
# ClickHouse nodes fetch cert/key/CA from Secrets Manager at boot via their instance role.
# ----------------------------------------------------------------------------
resource "aws_secretsmanager_secret" "tls_certificate" {
  count                   = local.acme_enabled ? 1 : 0
  name                    = "${var.tls_secret_prefix}-tls-certificate"
  description             = "Let's Encrypt wildcard certificate (chain) for ClickHouse ${var.cluster_name}"
  recovery_window_in_days = 7

  tags = var.tags
}

resource "aws_secretsmanager_secret_version" "tls_certificate" {
  count         = local.acme_enabled ? 1 : 0
  secret_id     = aws_secretsmanager_secret.tls_certificate[0].id
  secret_string = "${acme_certificate.cluster[0].certificate_pem}${acme_certificate.cluster[0].issuer_pem}"
}

resource "aws_secretsmanager_secret" "tls_private_key" {
  count                   = local.acme_enabled ? 1 : 0
  name                    = "${var.tls_secret_prefix}-tls-private-key"
  description             = "Let's Encrypt private key for ClickHouse ${var.cluster_name}"
  recovery_window_in_days = 7

  tags = var.tags
}

resource "aws_secretsmanager_secret_version" "tls_private_key" {
  count         = local.acme_enabled ? 1 : 0
  secret_id     = aws_secretsmanager_secret.tls_private_key[0].id
  secret_string = acme_certificate.cluster[0].private_key_pem
}

resource "aws_secretsmanager_secret" "tls_ca_certificate" {
  count                   = local.acme_enabled ? 1 : 0
  name                    = "${var.tls_secret_prefix}-tls-ca-certificate"
  description             = "Let's Encrypt issuer CA for ClickHouse ${var.cluster_name}"
  recovery_window_in_days = 7

  tags = var.tags
}

resource "aws_secretsmanager_secret_version" "tls_ca_certificate" {
  count         = local.acme_enabled ? 1 : 0
  secret_id     = aws_secretsmanager_secret.tls_ca_certificate[0].id
  secret_string = acme_certificate.cluster[0].issuer_pem
}

# Why: Instance roles must be able to pull the TLS secrets at bootstrap.
#      Scoped GetSecretValue to exactly the three TLS secret ARNs.
resource "aws_secretsmanager_secret_policy" "tls" {
  count = local.acme_enabled ? 1 : 0

  secret_arn = aws_secretsmanager_secret.tls_certificate[0].arn

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "AllowInstanceRolesRead"
      Effect    = "Allow"
      Principal = { AWS = var.allowed_role_arns }
      Action    = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
      Resource = [
        aws_secretsmanager_secret.tls_certificate[0].arn,
        aws_secretsmanager_secret.tls_private_key[0].arn,
        aws_secretsmanager_secret.tls_ca_certificate[0].arn,
      ]
    }]
  })
}

# ----------------------------------------------------------------------------
# Private client zone (optional) — Grafana/EKS reach ClickHouse over private DNS
# Mirrors the public writers/readers records, but in a PRIVATE zone associated
# with the target VPC so cluster clients resolve internal NLBs without DNS
# leaving the VPC.
# ----------------------------------------------------------------------------
resource "aws_route53_zone" "client_private" {
  count = var.client_private_zone_name != null && var.create_client_private_zone ? 1 : 0

  name    = var.client_private_zone_name
  comment = "Private client endpoints for ${var.private_zone_name} (managed by the clickhouse stack)"

  dynamic "vpc" {
    for_each = local.client_zone_vpc_ids
    content {
      vpc_id = vpc.value
    }
  }

  tags = merge(var.tags, {
    Name = var.client_private_zone_name
  })
}

# Note: all VPCs in client_zone_vpc_ids are attached directly in the zone
# resource above (the provider supports multiple vpc blocks); no separate
# association resource is needed.

resource "aws_route53_record" "client_writers" {
  count   = local.client_zone_enabled ? 1 : 0
  zone_id = local.client_zone_id
  name    = "${var.client_writers_record_name}.${var.client_private_zone_name}"
  type    = "A"

  alias {
    name                   = var.writers_lb_dns_name
    zone_id                = var.writers_lb_zone_id
    evaluate_target_health = true
  }
}

resource "aws_route53_record" "client_readers" {
  count   = local.client_zone_enabled ? 1 : 0
  zone_id = local.client_zone_id
  name    = "${var.client_readers_record_name}.${var.client_private_zone_name}"
  type    = "A"

  alias {
    name                   = var.readers_lb_dns_name
    zone_id                = var.readers_lb_zone_id
    evaluate_target_health = true
  }
}
