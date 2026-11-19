# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Network Module — AWS ClickHouse Cluster (chinit ecosystem)
# ============================================================================
# VPC -> aws-ia/vpc module (pinned), NSG -> per-role security groups,
# VNet Flow Logs -> aws_flow_log (kept local; hardened S3 destination).
# AWS-specific addition required by the S3-backed storage architecture:
#   an S3 *gateway* endpoint (free, keeps ClickHouse<->S3 traffic in-VPC).
#
# Ownership split with aws-ia/vpc (registry-module adoption policy):
#   module "vpc"  -> VPC, subnets, IGW, NAT+EIP, default routes, route tables
#   this module   -> S3 gateway endpoint, per-role SGs + rules, flow-log
#                    bucket + aws_flow_log, CIDR guard checks
# ============================================================================

# ----------------------------------------------------------------------------
# Locals: see locals.tf (all derived values in ONE locals block there)
# ----------------------------------------------------------------------------

# ============================================================================
# VPC, Subnets, NAT, Routes - aws-ia/vpc (registry module, pinned)
# The VPC/subnet/NAT/route-table boilerplate is standard AWS topology with no ClickHouse-specific logic, so it is delegated to the AWS Industries…
# ============================================================================
module "vpc" {
  count   = local.is_create_mode ? 1 : 0
  source  = "aws-ia/vpc/aws"
  version = "4.9.0"

  name       = var.cluster_name
  cidr_block = var.vpc_cidr
  # Single-AZ topology by default (the cost posture: sub-ms peer RTT,
  # $0 cross-AZ part traffic). The multi-AZ
  # opt-in spans 3 AZs so the writers/readers ASGs can fail an AZ; the
  # nodes subnet's CIDRs list grows one per AZ to match (aws-ia/vpc
  # requires cidrs count == az_count). Keepers/backup stay AZ-pinned
  # to the FIRST AZ (the primary subnet) regardless of the opt-in.
  az_count = local.multi_az_az_count

  # Why: DNS support + hostnames are required for the private hosted zone
  #      used for node discovery (keeper/writer/reader DNS records). Both
  #      flags must be true for instances to get resolvable private DNS
  #      names like writer-0.<cluster>.clickhouse.internal.
  vpc_enable_dns_support   = true
  vpc_enable_dns_hostnames = true

  subnets = {
    # PRIVATE nodes subnet - ALL ClickHouse compute. connect_to_public_natgw
    # = true only when NAT is enabled: without it the nodes route table gets
    # NO default route (air-gapped except for the S3 gateway endpoint).
    nodes = {
      name_prefix             = "${var.cluster_name}-nodes-subnet"
      cidrs                   = local.nodes_subnet_cidrs
      tags                    = { Tier = "private" }
      connect_to_public_natgw = var.enable_nat_gateway
    }
    # PUBLIC subnet - internet-facing NLBs + NAT gateway only; no compute
    # is ever placed here. nat gateway = single AZ by default; when multi_az
    # is true, nat_gateway_mode selects "single_az" vs "all_azs".
    public = {
      name_prefix               = "${var.cluster_name}-public-subnet"
      cidrs                     = local.public_subnet_cidrs
      tags                      = { Tier = "public" }
      map_public_ip_on_launch   = true
      nat_gateway_configuration = var.enable_nat_gateway ? (var.multi_az ? var.nat_gateway_mode : "single_az") : "none"
    }
  }

  tags = var.tags
}

# ============================================================================
# CIDR Overlap / Containment Guards
# ============================================================================
# Why: AWS reports overlapping subnet CIDRs only at apply time; these fail
#      fast at plan. Same checks the hand-rolled public subnet carried,
#      moved unchanged with the aws-ia/vpc adoption (module blocks accept no
#      lifecycle blocks, so they live in a check block here).
# Without it: An unusable VPC is provisioned and only surface as an AWS API
#             error mid-apply.
# ============================================================================
check "cidr_guards" {
  assert {
    condition     = !local.public_subnet_overlaps_nodes
    error_message = "public_subnet_cidr (${local.public_subnet_cidr}) overlaps the private nodes subnet (${var.subnet_cidr}); choose disjoint CIDRs."
  }
  assert {
    condition     = local.public_subnet_inside_vpc
    error_message = "public_subnet_cidr (${local.public_subnet_cidr}) must be a subnet of vpc_cidr (${var.vpc_cidr})."
  }
  assert {
    condition     = local.nodes_subnet_inside_vpc
    error_message = "subnet_cidr (${var.subnet_cidr}) must be a subnet of vpc_cidr (${var.vpc_cidr})."
  }
  # The multi-AZ carve: the /26s the opt-in carves for the extra-AZ
  # nodes and public subnets must stay disjoint from both the primary
  # nodes subnet and the primary public subnet.
  assert {
    condition = !var.multi_az || !anytrue([
      for extra in concat(
        slice(local.nodes_subnet_cidrs, 1, length(local.nodes_subnet_cidrs)),
        local.extra_public_cidrs,
      ) :
      cidrcontains(local.public_subnet_cidr, cidrhost(extra, 0)) ||
      cidrcontains(var.subnet_cidr, cidrhost(extra, 0))
    ])
    error_message = "multi-AZ extra /26 subnet CIDRs collide with the primary nodes subnet (${var.subnet_cidr}) or the primary public subnet (${local.public_subnet_cidr}); widen vpc_cidr or move subnet_cidr."
  }
  assert {
    condition     = !var.multi_az || local.vpc_prefix_bits <= 20
    error_message = "multi-AZ carves extra-AZ /26 subnets from the VPC tail and needs a /20 or larger vpc_cidr (${var.vpc_cidr})."
  }
}

# ============================================================================
# S3 Gateway Endpoint - Free In-VPC S3 Access (CRITICAL)
# A gateway endpoint keeps ClickHouse<->S3 traffic inside the AWS network instead of exiting via NAT/IGW.
# ============================================================================
resource "aws_vpc_endpoint" "s3" {
  count             = local.is_create_mode && var.create_s3_gateway_endpoint ? 1 : 0
  vpc_id            = local.vpc_id_effective
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"

  # Attaching the endpoint to the nodes route table injects the S3 prefix
  # list routes, which are more specific than the 0.0.0.0/0 NAT route, so
  # S3 traffic always rides the free endpoint even when NAT is enabled.
  route_table_ids = local.s3_gateway_route_tables

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-s3-endpoint"
  })
}

# ============================================================================
# Security Groups - One per Role, Least-Privilege (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per AWS security best practices, each node role gets its own security
#      group so traffic is restricted by role instead of opening Keeper and
#      interserver ports to the entire subnet. This implements
#      micro-segmentation and reduces the attack surface (parity with the
#      Azure ASG fix for BLINDSPOTS.md #13).
# Without it: Keeper and interserver ports are accessible from any host in
#             the subnet, increasing the blast radius if any single node is
#             compromised.
# ============================================================================

# ----------------------------------------------------------------------------
# Writers Security Group
# Writers accept client writes on the ClickHouse client ports and participate in interserver replication on 9010.
# ----------------------------------------------------------------------------
resource "aws_security_group" "writers" {
  name        = "${var.cluster_name}-writers-sg"
  description = "ClickHouse writers - client ports 8123/9000/8443/9440, interserver TLS 9010"
  vpc_id      = local.vpc_id_effective

  # Egress: allow all (traffic to S3 rides the free gateway endpoint route;
  # everything else exits via the NAT gateway when enabled).
  egress {
    description = "All outbound traffic (S3 rides the gateway endpoint route, the rest goes via NAT)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-writers-sg"
  })
}

# ----------------------------------------------------------------------------
# Readers Security Group
# Readers accept client reads on the same client ports as writers and pull replicated parts from writers over interserver TLS 9010.
# ----------------------------------------------------------------------------
resource "aws_security_group" "readers" {
  name        = "${var.cluster_name}-readers-sg"
  description = "ClickHouse readers - client ports 8123/9000/8443/9440, interserver TLS 9010"
  vpc_id      = local.vpc_id_effective

  egress {
    description = "All outbound traffic (S3 rides the gateway endpoint route, the rest goes via NAT)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-readers-sg"
  })
}

# ----------------------------------------------------------------------------
# Keepers Security Group
# Keepers talk to ClickHouse nodes (client port 9181 for coordination) and to each other (Raft consensus on 9234).
# ----------------------------------------------------------------------------
resource "aws_security_group" "keepers" {
  name        = "${var.cluster_name}-keepers-sg"
  description = "ClickHouse Keeper - client port 9181 (from cluster nodes), Raft 9234 (self only)"
  vpc_id      = local.vpc_id_effective

  egress {
    description = "All outbound traffic (S3 rides the gateway endpoint route, the rest goes via NAT)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-keepers-sg"
  })
}

# ----------------------------------------------------------------------------
# Backup Security Group
# The dedicated backup replica pulls replicated parts from writers and readers over interserver TLS 9010 so backups never touch client ports.
# ----------------------------------------------------------------------------
resource "aws_security_group" "backup" {
  name        = "${var.cluster_name}-backup-sg"
  description = "ClickHouse backup replica - interserver TLS 9010 replication pull only"
  vpc_id      = local.vpc_id_effective

  egress {
    description = "All outbound traffic (S3 rides the gateway endpoint route, the rest goes via NAT)"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-backup-sg"
  })
}

# ============================================================================
# Security Group Rules - Client Ports (Writers & Readers)
# Opens 8123/9000/8443/9440 to the caller-supplied client CIDRs plus the nodes subnet itself.
# ============================================================================
resource "aws_vpc_security_group_ingress_rule" "writers_client" {
  for_each = local.client_cidr_ingress_pairs

  security_group_id = aws_security_group.writers.id
  from_port         = each.value.port
  to_port           = each.value.port
  ip_protocol       = "tcp"
  # NLB health-check probes + forwarded client traffic arrive from the NLB's
  # subnets; the nodes subnet itself covers same-VPC clients (e.g. EKS pods).
  # Keyed per (port, cidr) so operator-IP or Multi-AZ CIDR changes never hit
  # InvalidPermission.Duplicate under upstream create_before_destroy.
  cidr_ipv4   = each.value.cidr
  description = "Client access (writers): ${each.value.description} + NLB subnet health checks"
}

resource "aws_vpc_security_group_ingress_rule" "readers_client" {
  for_each = local.client_cidr_ingress_pairs

  security_group_id = aws_security_group.readers.id
  from_port         = each.value.port
  to_port           = each.value.port
  ip_protocol       = "tcp"
  cidr_ipv4         = each.value.cidr
  description       = "Client access (readers): ${each.value.description} + NLB subnet health checks"
}

# SG-source client ingress (preferred on shared VPCs): pods/nodes in the EKS
# VPC reach ClickHouse without CIDR management. Fail-closed: only created
# when client_security_group_ids is non-empty.
resource "aws_security_group_rule" "writers_client_sg" {
  for_each = var.client_security_group_ids != [] ? {
    for pair in setproduct(keys(local.client_ports), var.client_security_group_ids) : "${pair[0]}-${pair[1]}" => {
      port = local.client_ports[pair[0]].port
      sg   = pair[1]
    }
  } : {}

  type                     = "ingress"
  security_group_id        = aws_security_group.writers.id
  from_port                = each.value.port
  to_port                  = each.value.port
  protocol                 = "tcp"
  source_security_group_id = each.value.sg
  description              = "Client access (writers) from allowed SG"
}

resource "aws_security_group_rule" "readers_client_sg" {
  for_each = var.client_security_group_ids != [] ? {
    for pair in setproduct(keys(local.client_ports), var.client_security_group_ids) : "${pair[0]}-${pair[1]}" => {
      port = local.client_ports[pair[0]].port
      sg   = pair[1]
    }
  } : {}

  type                     = "ingress"
  security_group_id        = aws_security_group.readers.id
  from_port                = each.value.port
  to_port                  = each.value.port
  protocol                 = "tcp"
  source_security_group_id = each.value.sg
  description              = "Client access (readers) from allowed SG"
}

# ============================================================================
# Security Group Rules - Interserver TLS (9010)
# Writers, readers and the backup replica exchange data parts over the TLS-encrypted interserver port 9010 (set by <interserver_https_port> in the…
# ============================================================================
resource "aws_security_group_rule" "writers_interserver_self" {
  type              = "ingress"
  security_group_id = aws_security_group.writers.id
  from_port         = 9010
  to_port           = 9010
  protocol          = "tcp"
  self              = true
  description       = "Interserver TLS 9010 from other writers"
}

resource "aws_security_group_rule" "writers_interserver_readers" {
  type                     = "ingress"
  security_group_id        = aws_security_group.writers.id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.readers.id
  description              = "Interserver TLS 9010 from readers"
}

resource "aws_security_group_rule" "readers_interserver_self" {
  type              = "ingress"
  security_group_id = aws_security_group.readers.id
  from_port         = 9010
  to_port           = 9010
  protocol          = "tcp"
  self              = true
  description       = "Interserver TLS 9010 from other readers"
}

resource "aws_security_group_rule" "readers_interserver_writers" {
  type                     = "ingress"
  security_group_id        = aws_security_group.readers.id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.writers.id
  description              = "Interserver TLS 9010 from writers"
}

# 9010;
resource "aws_security_group_rule" "writers_interserver_backup" {
  type                     = "ingress"
  security_group_id        = aws_security_group.writers.id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.backup.id
  description              = "Replication pull: interserver TLS 9010 from backup"
}

resource "aws_security_group_rule" "readers_interserver_backup" {
  type                     = "ingress"
  security_group_id        = aws_security_group.readers.id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.backup.id
  description              = "Replication pull: interserver TLS 9010 from backup"
}

resource "aws_security_group_rule" "backup_interserver_writers" {
  type                     = "ingress"
  security_group_id        = aws_security_group.backup.id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.writers.id
  description              = "Replication pull: interserver TLS 9010 from writers"
}

resource "aws_security_group_rule" "backup_interserver_readers" {
  type                     = "ingress"
  security_group_id        = aws_security_group.backup.id
  from_port                = 9010
  to_port                  = 9010
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.readers.id
  description              = "Replication pull: interserver TLS 9010 from readers"
}

# ----------------------------------------------------------------------------
# Backup client ports from the fleet SGs
# The backup replica is a CR member (its slot renders after the readers — chinit 0.
# ----------------------------------------------------------------------------
resource "aws_security_group_rule" "backup_client_http_writers" {
  for_each = {
    http   = 8123
    native = 9000
  }

  type                     = "ingress"
  security_group_id        = aws_security_group.backup.id
  from_port                = each.value
  to_port                  = each.value
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.writers.id
  description              = "CR-member cluster fan-out: ${each.key} from writers"
}

resource "aws_security_group_rule" "backup_client_http_readers" {
  for_each = {
    http   = 8123
    native = 9000
  }

  type                     = "ingress"
  security_group_id        = aws_security_group.backup.id
  from_port                = each.value
  to_port                  = each.value
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.readers.id
  description              = "CR-member cluster fan-out: ${each.key} from readers"
}

# ============================================================================
# Security Group Rules - Keeper Ports (9181 client, 9234 Raft)
# Keepers expose 9181 (recommended Keeper client port, NOT legacy ZooKeeper 2181) to ClickHouse nodes for coordination, and 9234 (Raft) only to…
# ============================================================================
resource "aws_security_group_rule" "keepers_client_writers" {
  type                     = "ingress"
  security_group_id        = aws_security_group.keepers.id
  from_port                = 9181
  to_port                  = 9181
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.writers.id
  description              = "Keeper client 9181 from writers"
}

resource "aws_security_group_rule" "keepers_client_readers" {
  type                     = "ingress"
  security_group_id        = aws_security_group.keepers.id
  from_port                = 9181
  to_port                  = 9181
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.readers.id
  description              = "Keeper client 9181 from readers"
}

resource "aws_security_group_rule" "keepers_client_backup" {
  type                     = "ingress"
  security_group_id        = aws_security_group.keepers.id
  from_port                = 9181
  to_port                  = 9181
  protocol                 = "tcp"
  source_security_group_id = aws_security_group.backup.id
  description              = "Keeper client 9181 from the backup replica"
}

resource "aws_security_group_rule" "keepers_raft_self" {
  type              = "ingress"
  security_group_id = aws_security_group.keepers.id
  from_port         = 9234
  to_port           = 9234
  protocol          = "tcp"
  self              = true
  description       = "Keeper Raft consensus 9234 from other keepers"
}

# ============================================================================
# Security Group Rules - SSH (22), Opt-In Only
# SSH is disabled by default in favor of SSM Session Manager (no bastion, no exposed port).
# ============================================================================
resource "aws_security_group_rule" "writers_ssh" {
  count             = length(var.allowed_ssh_ips) > 0 ? 1 : 0
  type              = "ingress"
  security_group_id = aws_security_group.writers.id
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  cidr_blocks       = var.allowed_ssh_ips
  description       = "SSH from allowed IPs (SSM Session Manager preferred)"
}

resource "aws_security_group_rule" "readers_ssh" {
  count             = length(var.allowed_ssh_ips) > 0 ? 1 : 0
  type              = "ingress"
  security_group_id = aws_security_group.readers.id
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  cidr_blocks       = var.allowed_ssh_ips
  description       = "SSH from allowed IPs (SSM Session Manager preferred)"
}

resource "aws_security_group_rule" "keepers_ssh" {
  count             = length(var.allowed_ssh_ips) > 0 ? 1 : 0
  type              = "ingress"
  security_group_id = aws_security_group.keepers.id
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  cidr_blocks       = var.allowed_ssh_ips
  description       = "SSH from allowed IPs (SSM Session Manager preferred)"
}

resource "aws_security_group_rule" "backup_ssh" {
  count             = length(var.allowed_ssh_ips) > 0 ? 1 : 0
  type              = "ingress"
  security_group_id = aws_security_group.backup.id
  from_port         = 22
  to_port           = 22
  protocol          = "tcp"
  cidr_blocks       = var.allowed_ssh_ips
  description       = "SSH from allowed IPs (SSM Session Manager preferred)"
}

# ============================================================================
# VPC Flow Logs (SECURITY AUDIT)
# ============================================================================
# Why: Per the AWS Security Baseline, flow logs are required for auditing
#      network traffic, detecting anomalies and investigating incidents
#      (compliance: PCI-DSS, HIPAA, SOC2). Opt-in because S3 storage is
#      metered (the Azure module defaulted this on; AWS parity keeps the
#      feature but flips the default to match the root module's contract).
# Without it: Cannot audit traffic to/from the cluster or investigate
#             security incidents; non-compliant with security frameworks.
# ============================================================================

# Account lookup for the delivery policy (count-gated so the module plans
# without AWS credentials when no module bucket is created).
data "aws_caller_identity" "current" {
  count = local.create_flow_log_bucket ? 1 : 0
}

# A small versioned bucket with lifecycle expiry keeps flow-log spend bounded and isolated from the cluster's data/backup buckets.
resource "aws_s3_bucket" "flow_logs" {
  count  = local.create_flow_log_bucket ? 1 : 0
  bucket = "${var.cluster_name}-vpc-flow-logs"

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-vpc-flow-logs"
  })
}

resource "aws_s3_bucket_versioning" "flow_logs" {
  count  = local.create_flow_log_bucket ? 1 : 0
  bucket = aws_s3_bucket.flow_logs[0].id

  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "flow_logs" {
  count  = local.create_flow_log_bucket ? 1 : 0
  bucket = aws_s3_bucket.flow_logs[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_public_access_block" "flow_logs" {
  count  = local.create_flow_log_bucket ? 1 : 0
  bucket = aws_s3_bucket.flow_logs[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "flow_logs" {
  count  = local.create_flow_log_bucket ? 1 : 0
  bucket = aws_s3_bucket.flow_logs[0].id

  rule {
    id     = "expire-flow-logs"
    status = "Enabled"

    filter {}

    expiration {
      days = var.flow_log_retention_days
    }

    # Versioning is on, so expired objects become noncurrent versions that
    # would accumulate forever without this rule.
    noncurrent_version_expiration {
      noncurrent_days = var.flow_log_retention_days
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  # The versioning resource must exist before the lifecycle configuration
  # references versioned-object behavior.
  depends_on = [aws_s3_bucket_versioning.flow_logs]
}

# Allows ONLY the log delivery service to write, scoped to this account and region via aws:SourceAccount / aws:SourceArn (confused-deputy protection).
resource "aws_s3_bucket_policy" "flow_logs" {
  count  = local.create_flow_log_bucket ? 1 : 0
  bucket = aws_s3_bucket.flow_logs[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "AWSLogDeliveryWrite"
        Effect    = "Allow"
        Principal = { Service = "delivery.logs.amazonaws.com" }
        Action    = "s3:PutObject"
        Resource  = "${aws_s3_bucket.flow_logs[0].arn}/*"
        Condition = {
          StringEquals = {
            "aws:SourceAccount" = data.aws_caller_identity.current[0].account_id
            "s3:x-amz-acl"      = "bucket-owner-full-control"
          }
          ArnLike = {
            "aws:SourceArn" = "arn:aws:logs:${var.aws_region}:${data.aws_caller_identity.current[0].account_id}:*"
          }
        }
      },
      {
        Sid       = "AWSLogDeliveryAclCheck"
        Effect    = "Allow"
        Principal = { Service = "delivery.logs.amazonaws.com" }
        Action    = "s3:GetBucketAcl"
        Resource  = aws_s3_bucket.flow_logs[0].arn
        Condition = {
          StringEquals = {
            "aws:SourceAccount" = data.aws_caller_identity.current[0].account_id
          }
          ArnLike = {
            "aws:SourceArn" = "arn:aws:logs:${var.aws_region}:${data.aws_caller_identity.current[0].account_id}:*"
          }
        }
      },
    ]
  })

  depends_on = [aws_s3_bucket_public_access_block.flow_logs]
}

resource "aws_flow_log" "main" {
  count = var.enable_flow_logs ? 1 : 0

  vpc_id               = local.vpc_id_effective
  traffic_type         = "ALL"
  log_destination_type = "s3"
  log_destination      = var.flow_log_bucket_arn != null ? var.flow_log_bucket_arn : aws_s3_bucket.flow_logs[0].arn

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-vpc-flow-log"
  })

  depends_on = [aws_s3_bucket_policy.flow_logs]
}

# ============================================================================
# Existing-VPC (BYO-VPC) resources — created ONLY when mode = "existing"
# ============================================================================
# Additive-only: no existing VPC resource is modified or deleted. The nodes
# subnet (optional) gets its own route table; the S3 gateway endpoint is
# attached only to that table or a caller-supplied route table.

# Reused subnet: read CIDR/AZ for placement and outputs.
data "aws_subnet" "existing_nodes" {
  count = local.is_existing && !local.create_nodes_subnet_eff ? 1 : 0
  id    = var.existing_nodes_subnet_id
}

# Subnets chosen for the NLBs (existing mode): look up CIDRs for SG rules.
data "aws_subnet" "nlb" {
  for_each = local.is_existing ? toset([for s in local.nlb_subnet_ids_raw : s if s != "__nodes_subnet__"]) : toset([])
  id       = each.value
}

# Route table of a REUSED nodes subnet: needed to attach the S3 gateway
# endpoint. Caller override via existing_nodes_route_table_id, else main RT.
data "aws_route_table" "nodes" {
  count     = local.is_existing && var.existing_nodes_route_table_id == null && !(local.create_nodes_subnet_eff && var.create_nodes_route_table) && var.nodes_route_table_id == null ? 1 : 0
  subnet_id = local.create_nodes_subnet_eff ? aws_subnet.nodes[0].id : var.existing_nodes_subnet_id
}

# Dedicated nodes subnet inside the existing VPC (recommended for shared VPCs).
resource "aws_subnet" "nodes" {
  count                   = local.create_nodes_subnet_eff ? 1 : 0
  vpc_id                  = var.existing_vpc_id
  cidr_block              = var.nodes_subnet_cidr
  availability_zone       = var.nodes_subnet_az
  map_public_ip_on_launch = false

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-nodes-subnet"
    Tier = "private"
  })
}

resource "aws_route_table" "nodes" {
  count  = local.create_nodes_subnet_eff && var.create_nodes_route_table ? 1 : 0
  vpc_id = var.existing_vpc_id

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-nodes-rt"
  })
}

resource "aws_route_table_association" "nodes" {
  count          = local.create_nodes_subnet_eff ? 1 : 0
  subnet_id      = aws_subnet.nodes[0].id
  route_table_id = local.nodes_route_table_id_eff
}

# Optional single NAT gateway in a caller-chosen public subnet. Nodes route
# 0.0.0.0/0 through it; without it the nodes subnet has no default route and
# reaches S3 exclusively via the gateway endpoint.
resource "aws_eip" "nat" {
  count  = local.is_existing && var.create_nat_gateway ? 1 : 0
  domain = "vpc"

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-nat-eip"
  })
}

resource "aws_nat_gateway" "nodes" {
  count         = local.is_existing && var.create_nat_gateway ? 1 : 0
  allocation_id = aws_eip.nat[0].id
  subnet_id     = var.nat_subnet_id

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-nat"
  })
}

resource "aws_route" "nodes_default" {
  count                  = local.is_existing && var.create_nat_gateway && var.create_nodes_route_table ? 1 : 0
  route_table_id         = aws_route_table.nodes[0].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.nodes[0].id
}

# Dedicated S3 gateway endpoint for the nodes route table when running in an
# existing VPC that may already have one attached elsewhere (create mode owns
# its own via aws-ia/vpc, unchanged).
resource "aws_vpc_endpoint" "s3_existing" {
  count             = local.is_existing && var.create_s3_gateway_endpoint ? 1 : 0
  vpc_id            = var.existing_vpc_id
  service_name      = "com.amazonaws.${var.aws_region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = [local.nodes_route_table_id_eff]

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-s3-endpoint"
  })
}

resource "aws_vpc_endpoint" "dynamodb" {
  count             = var.create_dynamodb_gateway_endpoint ? 1 : 0
  vpc_id            = local.is_create_mode ? local.vpc_id_effective : var.existing_vpc_id
  service_name      = "com.amazonaws.${var.aws_region}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = local.s3_gateway_route_tables

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-dynamodb-endpoint"
  })
}

# Interface endpoints for AWS API access without NAT (SSM, logs, secrets, ...).
# Only in existing mode: create mode relies on the NAT gateway.
resource "aws_vpc_endpoint" "interface" {
  for_each = local.is_existing ? toset(var.vpc_endpoint_services) : toset([])

  vpc_id              = var.existing_vpc_id
  service_name        = "com.amazonaws.${var.aws_region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = [local.nodes_subnet_id]
  security_group_ids  = concat([aws_security_group.endpoints[0].id], var.endpoint_security_group_ids)
  private_dns_enabled = true

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-${each.value}-endpoint"
  })
}

# Endpoint SG: ingress 443 from the ClickHouse node SGs only.
resource "aws_security_group" "endpoints" {
  count       = local.is_existing && length(var.vpc_endpoint_services) > 0 ? 1 : 0
  name        = "${var.cluster_name}-endpoints-sg"
  description = "HTTPS (443) from ClickHouse node security groups to interface VPC endpoints"
  vpc_id      = var.existing_vpc_id

  tags = merge(var.tags, {
    Name = "${var.cluster_name}-endpoints-sg"
  })
}

resource "aws_security_group_rule" "endpoints_from_nodes" {
  for_each = local.is_existing && length(var.vpc_endpoint_services) > 0 ? {
    writers = aws_security_group.writers.id
    readers = aws_security_group.readers.id
    keepers = aws_security_group.keepers.id
    backup  = aws_security_group.backup.id
  } : {}

  type                     = "ingress"
  security_group_id        = aws_security_group.endpoints[0].id
  from_port                = 443
  to_port                  = 443
  protocol                 = "tcp"
  source_security_group_id = each.value
  description              = "HTTPS from ${each.key} to interface endpoints"
}
