# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Compute + DNS — the fleet and its discovery surface
# ============================================================================
# The ecosystem compute module owns the nodes (writers/readers ASGs with
# lease identity, keepers/backup as fixed-identity EC2s). NLBs are OFF by
# default in this facade (private-DNS-only); dns_tls owns the private zone
# the nodes self-register into. The IAM glue grants each role exactly its
# claims + DNS write surface.
# ============================================================================

module "compute" {
  source = "./modules/compute"

  cluster_name = var.cluster_name
  aws_region   = var.region
  vpc_id       = module.network.vpc_id
  subnet_id    = module.network.subnet_id
  subnet_az    = module.network.subnet_availability_zone
  subnet_cidr  = module.network.subnet_cidr_block

  placement_strategy = "cluster"
  ami_id             = local.golden_ami_id

  writer_instance_type = var.instance_type
  reader_instance_type = var.instance_type
  keeper_instance_type = var.keeper_instance_type
  backup_instance_type = var.instance_type

  writer_count      = var.writer_desired
  writer_min        = 1
  writer_max        = var.writer_max
  writer_cpu_target = var.writer_cpu_target

  reader_count = var.reader_desired
  keeper_count = var.keeper_count

  reader_spot_ratio                   = var.reader_spot
  reader_spot_on_demand_base_capacity = var.reader_spot_on_demand_base_capacity
  enable_backup_replica               = var.enable_backup

  metadata_disk_size_gb         = var.metadata_disk_size_gb
  metadata_disk_iops            = var.metadata_disk_iops
  metadata_disk_throughput_mbps = var.metadata_disk_throughput_mbps
  cache_disk_size_gb            = var.cache_disk_size_gb
  keeper_disk_size_gb           = var.keeper_disk_size_gb
  root_disk_size_gb             = var.root_disk_size_gb
  cache_on_nvme_instance_store  = var.cache_on_nvme_instance_store

  # Private-DNS-only facade: no NLBs, no public client path. Enable via
  # the ecosystem modules directly when you outgrow the facade.
  enable_nlbs = false

  writers_sg_id = module.network.writers_sg_id
  readers_sg_id = module.network.readers_sg_id
  keepers_sg_id = module.network.keepers_sg_id
  backup_sg_id  = module.network.backup_sg_id

  writer_instance_profile_name = module.storage.writer_instance_profile_name
  reader_instance_profile_name = module.storage.reader_instance_profile_name
  keeper_instance_profile_name = module.storage.keeper_instance_profile_name
  backup_instance_profile_name = module.storage.backup_instance_profile_name

  writer_user_data = module.clickhouse.writer_bootstrap
  reader_user_data = module.clickhouse.reader_bootstrap
  keeper_user_data = module.clickhouse.keeper_bootstrap
  backup_user_data = module.clickhouse.backup_replica_bootstrap

  autoscaling_enabled             = var.autoscaling_enabled
  autoscaling_min                 = 1
  autoscaling_max                 = var.reader_max
  autoscaling_target_cpu          = 70
  scheduled_scaling               = []
  refresh_instance_warmup_seconds = var.refresh_instance_warmup_seconds
  refresh_min_healthy_percentage  = var.refresh_min_healthy_percentage
  max_unhealthy_percent           = 50

  tags = local.common_tags

  depends_on = [module.clickhouse, module.network, module.storage]
}

module "dns_tls" {
  source = "./modules/dns_tls"

  cluster_name      = var.cluster_name
  aws_region        = var.region
  vpc_id            = module.network.vpc_id
  private_zone_name = var.dns_zone_name

  keeper_hostnames = local.keeper_hostnames
  keeper_ips       = module.compute.keeper_ips

  backup_replica_enabled = var.enable_backup
  backup_hostname        = "backup-0"
  backup_ip              = coalesce(module.compute.backup_ip, "0.0.0.0")

  # No public records in the facade: nodes are reached on
  # <role>-N.<dns_zone_name> inside the VPC.
  create_public_records = false
  nlb_tls_mode          = "passthrough"

  tags = local.common_tags

  depends_on = [module.compute, module.network, module.storage]
}

# ----------------------------------------------------------------------------
# Per-role IAM glue: claims + private-DNS self-registration
# ----------------------------------------------------------------------------
# The storage module owns the roles; chinit's claims + DNS registration
# need these scoped grants on top. Exactly the PLOP each role uses at boot.

resource "aws_iam_role_policy" "writer_claims" {
  name = "${var.cluster_name}-writer-claims"
  role = module.storage.writer_role_name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "WriterOrdinalClaims"
      Effect   = "Allow"
      Action   = ["dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:GetItem", "dynamodb:Scan"]
      Resource = aws_dynamodb_table.node_claims.arn
    }]
  })
}

resource "aws_iam_role_policy" "backup_claims" {
  count = var.enable_backup ? 1 : 0

  name = "${var.cluster_name}-backup-claims"
  role = module.storage.backup_role_name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid      = "BackupPeerClaimsScan"
      Effect   = "Allow"
      Action   = ["dynamodb:Scan"]
      Resource = aws_dynamodb_table.node_claims.arn
    }]
  })
}

# Writers + readers claim an ordinal AND self-register their DNS record.
resource "aws_iam_role_policy" "writer_dns" {
  name = "${var.cluster_name}-writer-dns"
  role = module.storage.writer_role_name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "WriterPrivateDNSRegistration"
        Effect = "Allow"
        Action = ["route53:ChangeResourceRecordSets"]
        Resource = [
          "arn:aws:route53:::hostedzone/${module.dns_tls.private_zone_id}"
        ]
      },
      {
        Sid    = "WriterPrivateDNSRecordRead"
        Effect = "Allow"
        Action = ["route53:ListResourceRecordSets"]
        Resource = [
          "arn:aws:route53:::hostedzone/${module.dns_tls.private_zone_id}"
        ]
      },
      {
        Sid      = "WriterPrivateDNSZoneLookup"
        Effect   = "Allow"
        Action   = ["route53:ListHostedZonesByName", "route53:GetHostedZone"]
        Resource = "*"
      }
    ]
  })

  depends_on = [module.dns_tls]
}

resource "aws_iam_role_policy" "reader_dns" {
  name = "${var.cluster_name}-reader-dns"
  role = module.storage.reader_role_name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "ReaderPrivateDNSRegistration"
        Effect = "Allow"
        Action = ["route53:ChangeResourceRecordSets"]
        Resource = [
          "arn:aws:route53:::hostedzone/${module.dns_tls.private_zone_id}"
        ]
      },
      {
        Sid    = "ReaderPrivateDNSRecordRead"
        Effect = "Allow"
        Action = ["route53:ListResourceRecordSets"]
        Resource = [
          "arn:aws:route53:::hostedzone/${module.dns_tls.private_zone_id}"
        ]
      },
      {
        Sid      = "ReaderPrivateDNSZoneLookup"
        Effect   = "Allow"
        Action   = ["route53:ListHostedZonesByName", "route53:GetHostedZone"]
        Resource = "*"
      }
    ]
  })

  depends_on = [module.dns_tls]
}

# Keepers + backup hold static identities; they only register their A record.
resource "aws_iam_role_policy" "keeper_dns" {
  name = "${var.cluster_name}-keeper-dns"
  role = module.storage.keeper_role_name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "KeeperPrivateDNSRegistration"
        Effect = "Allow"
        Action = ["route53:ChangeResourceRecordSets"]
        Resource = [
          "arn:aws:route53:::hostedzone/${module.dns_tls.private_zone_id}"
        ]
      },
      {
        Sid      = "KeeperPrivateDNSZoneLookup"
        Effect   = "Allow"
        Action   = ["route53:ListHostedZonesByName", "route53:GetHostedZone"]
        Resource = "*"
      }
    ]
  })

  depends_on = [module.dns_tls]
}

resource "aws_iam_role_policy" "backup_dns" {
  count = var.enable_backup ? 1 : 0

  name = "${var.cluster_name}-backup-dns"
  role = module.storage.backup_role_name

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "BackupPrivateDNSRegistration"
        Effect = "Allow"
        Action = ["route53:ChangeResourceRecordSets"]
        Resource = [
          "arn:aws:route53:::hostedzone/${module.dns_tls.private_zone_id}"
        ]
      },
      {
        Sid      = "BackupPrivateDNSZoneLookup"
        Effect   = "Allow"
        Action   = ["route53:ListHostedZonesByName", "route53:GetHostedZone"]
        Resource = "*"
      }
    ]
  })

  depends_on = [module.dns_tls]
}
