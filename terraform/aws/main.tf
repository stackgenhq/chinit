# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# chinit cluster module (AWS) — one self-contained root module
# ============================================================================
# Provisions exactly what a chinit-bootstrapped ClickHouse fleet needs:
# the private DNS zone, the node-claims lease table, the secrets, the
# bundle + data + backup buckets, one instance role, the spec/bundle
# uploads, and the four node roles (writers/readers as lease-identity
# ASGs, keepers/backup as fixed-identity EC2s). Everything else — load
# balancers, autoscaling policies, rotation lambdas — is deliberately
# out of scope; the module is the on-ramp, not the kitchen sink.
# ============================================================================

# ----------------------------------------------------------------------------
# Network (optional — bring-your-own via vpc_id/subnet_id)
# ----------------------------------------------------------------------------
# A single private subnet, one NAT-less route (S3 and DynamoDB ride the
# free gateway endpoints; nodes need no other egress to boot: the golden
# AMI carries every binary, and chinit talks to AWS APIs over IMDS +
# regional endpoints).
resource "aws_vpc" "this" {
  count = var.create_network ? 1 : 0

  cidr_block           = var.subnet_cidr
  enable_dns_hostnames = true # required for private-zone discovery
  enable_dns_support   = true

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-vpc" })
}

resource "aws_subnet" "nodes" {
  count = var.create_network ? 1 : 0

  vpc_id                  = aws_vpc.this[0].id
  cidr_block              = var.subnet_cidr
  map_public_ip_on_launch = false
  availability_zone       = data.aws_availability_zones.available.names[0]

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-nodes" })
}

data "aws_availability_zones" "available" {
  state = "available"
}

resource "aws_route_table" "nodes" {
  count = var.create_network ? 1 : 0

  vpc_id = aws_vpc.this[0].id

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-rt" })
}

resource "aws_route_table_association" "nodes" {
  count = var.create_network ? 1 : 0

  subnet_id      = aws_subnet.nodes[0].id
  route_table_id = aws_route_table.nodes[0].id
}

# Gateway endpoints: free, and the only S3/DynamoDB paths nodes need.
resource "aws_vpc_endpoint" "s3" {
  count = var.create_network ? 1 : 0

  vpc_id            = aws_vpc.this[0].id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.nodes[0].id]

  tags = local.common_tags
}

resource "aws_vpc_endpoint" "dynamodb" {
  count = var.create_network ? 1 : 0

  vpc_id            = aws_vpc.this[0].id
  service_name      = "com.amazonaws.${var.region}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [aws_route_table.nodes[0].id]

  tags = local.common_tags
}

# ----------------------------------------------------------------------------
# Private DNS zone — node discovery
# ----------------------------------------------------------------------------
# chinit self-registers every node's A record (writer-N at claim time,
# keepers/backup at identity settle). The zone lookup + upsert need the
# grants in the instance policy below.
resource "aws_route53_zone" "private" {
  name = var.dns_zone_name

  vpc {
    vpc_id = local.vpc_id
  }

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-private-zone" })
}

# ----------------------------------------------------------------------------
# Node-claims lease table — ordinal identity for the ASG roles
# ----------------------------------------------------------------------------
# One table serves both roles (writer-N and reader-N hash keys never
# collide). lease_until carries the TTL: a dead node's claim expires and
# its ordinal is released for the next replacement — the identity source
# that makes spot churn safe.
resource "aws_dynamodb_table" "node_claims" {
  name         = "${local.name_prefix}-node-claims"
  billing_mode = "PAY_PER_REQUEST"

  hash_key = "hostname"

  attribute {
    name = "hostname"
    type = "S"
  }

  ttl {
    attribute_name = "lease_until"
    enabled        = true
  }

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-node-claims" })
}

# ----------------------------------------------------------------------------
# Password secrets — NAMES reach the nodes; values never do
# ----------------------------------------------------------------------------
# random_password + secret version: the plaintext exists only in
# Terraform state (mark it sensitive where you keep it) and Secrets
# Manager; the chinit spec carries the NAME and nodes resolve values over
# the instance role. Access is the resource policy below, so revoking it
# cuts fleet access in one edit.
resource "random_password" "default_password" {
  length  = 24
  special = false
}

resource "random_password" "admin_password" {
  length  = 24
  special = false
}

resource "random_password" "writer_password" {
  length  = 24
  special = false
}

resource "aws_secretsmanager_secret" "default_password" {
  name                    = "${local.name_prefix}-clickhouse-default-password"
  recovery_window_in_days = 7

  tags = local.common_tags
}

resource "aws_secretsmanager_secret" "admin_password" {
  name                    = "${local.name_prefix}-clickhouse-admin-password"
  recovery_window_in_days = 7

  tags = local.common_tags
}

resource "aws_secretsmanager_secret" "writer_password" {
  name                    = "${local.name_prefix}-clickhouse-writer-password"
  recovery_window_in_days = 7

  tags = local.common_tags
}

resource "aws_secretsmanager_secret_version" "default_password" {
  secret_id     = aws_secretsmanager_secret.default_password.id
  secret_string = random_password.default_password.result
}

resource "aws_secretsmanager_secret_version" "admin_password" {
  secret_id     = aws_secretsmanager_secret.admin_password.id
  secret_string = random_password.admin_password.result
}

resource "aws_secretsmanager_secret_version" "writer_password" {
  secret_id     = aws_secretsmanager_secret.writer_password.id
  secret_string = random_password.writer_password.result
}

# ----------------------------------------------------------------------------
# Buckets: the config bundle, the data (cold tier), backups
# ----------------------------------------------------------------------------
resource "aws_s3_bucket" "bootstrap" {
  bucket = "${local.name_prefix}-clickhouse-bootstrap"

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-clickhouse-bootstrap" })
}

resource "aws_s3_bucket" "data" {
  bucket = "${local.name_prefix}-clickhouse-data"

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-clickhouse-data" })
}

resource "aws_s3_bucket" "backup" {
  bucket = "${local.name_prefix}-clickhouse-backup"

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-clickhouse-backup" })
}

resource "aws_s3_bucket_public_access_block" "bootstrap" {
  bucket = aws_s3_bucket.bootstrap.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_public_access_block" "data" {
  bucket = aws_s3_bucket.data.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_public_access_block" "backup" {
  bucket = aws_s3_bucket.backup.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "data" {
  bucket = aws_s3_bucket.data.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "aws:kms"
    }
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backup" {
  bucket = aws_s3_bucket.backup.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "aws:kms"
    }
  }
}

# ----------------------------------------------------------------------------
# Security groups
# ----------------------------------------------------------------------------
# One SG per role: client ports to var.client_cidrs only (empty = no
# client ingress), interserver 9010 + keeper 9181/9234 between the fleet's
# SGs, egress open (S3 rides the gateway endpoint; no NAT is provisioned).
resource "aws_security_group" "nodes" {
  for_each = toset(["writers", "readers", "keepers", "backup"])

  name        = "${local.name_prefix}-${each.key}-sg"
  description = "ClickHouse ${each.key} — client ports from var.client_cidrs, interserver/keeper from the fleet"
  vpc_id      = local.vpc_id

  egress {
    description = "All outbound (S3/DynamoDB ride the gateway endpoints)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-${each.key}-sg" })
}

# Client ports (HTTP 8123/8443, native 9000/9440) on writers + readers,
# from var.client_cidrs only (empty list = no client ingress).
# Client ports (HTTP 8123/8443, native 9000/9440) on writers + readers,
# from var.client_cidrs only. Rendered ONLY when the list is non-empty:
# an empty list means "no client ingress yet" — never an open rule.
resource "aws_security_group_rule" "writers_client" {
  for_each = length(var.client_cidrs) > 0 ? { for p in [8123, 8443, 9000, 9440] : tostring(p) => p } : {}

  type              = "ingress"
  security_group_id = aws_security_group.nodes["writers"].id
  from_port         = each.value
  to_port           = each.value
  protocol          = "tcp"
  cidr_blocks       = var.client_cidrs
  description       = "ClickHouse client port ${each.value}"
}

resource "aws_security_group_rule" "readers_client" {
  for_each = length(var.client_cidrs) > 0 ? { for p in [8123, 8443, 9000, 9440] : tostring(p) => p } : {}

  type              = "ingress"
  security_group_id = aws_security_group.nodes["readers"].id
  from_port         = each.value
  to_port           = each.value
  protocol          = "tcp"
  cidr_blocks       = var.client_cidrs
  description       = "ClickHouse client port ${each.value}"
}

# Interserver TLS (9010): every server role talks to every other.
resource "aws_security_group_rule" "interserver" {
  for_each = { for pair in [
    { from = "writers", to = "readers" },
    { from = "writers", to = "writers" },
    { from = "writers", to = "backup" },
    { from = "readers", to = "writers" },
    { from = "readers", to = "readers" },
    { from = "readers", to = "backup" },
    { from = "backup", to = "writers" },
    { from = "backup", to = "readers" },
  ] : "${pair.from}-to-${pair.to}" => pair }

  type                     = "ingress"
  security_group_id        = aws_security_group.nodes[each.value.to].id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.nodes[each.value.from].id
  description              = "Interserver TLS from ${each.value.from}"
}

# Keeper client port (9181) from every server role + keeper raft (9234)
# between keepers.
resource "aws_security_group_rule" "keeper_client" {
  for_each = { for from in ["writers", "readers", "backup", "keepers"] : from => from }

  type                     = "ingress"
  security_group_id        = aws_security_group.nodes["keepers"].id
  from_port                = 9181
  to_port                  = 9181
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.nodes[each.value].id
  description              = "Keeper coordination from ${each.value}"
}

resource "aws_security_group_rule" "keeper_raft" {
  type                     = "ingress"
  security_group_id        = aws_security_group.nodes["keepers"].id
  from_port                = 9234
  to_port                  = 9234
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.nodes["keepers"].id
  description              = "Keeper Raft consensus (keepers only)"
}
