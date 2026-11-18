# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# The chinit contract — consumed from the contract module
# ============================================================================
# The all-in-one module does NOT render specs/bundles itself: it calls
# terraform/modules/chinit/aws (the open-source home of the chinit
# interface) and adds the cloud resources around it. An external
# stack consumes the SAME contract module — one renderer, one digest,
# one wrapper; the ecosystem extends instead of forking.
# ============================================================================

module "chinit" {
  source = "../modules/chinit/aws"

  cluster_name  = var.cluster_name
  region        = var.region
  dns_zone_name = var.dns_zone_name

  writer_max       = var.writer_max
  reader_max       = var.reader_max
  reader_count     = var.reader_desired
  keeper_hostnames = local.keeper_hostnames
  enable_backup    = var.enable_backup

  # Cloud resources this module owns and the specs point at.
  bootstrap_bucket      = aws_s3_bucket.bootstrap.id
  bootstrap_prefix      = "bootstrap"
  data_bucket           = aws_s3_bucket.data.id
  data_prefix           = var.s3_data_prefix
  cas_enabled           = var.cas_enabled
  cas_prefix            = var.cas_prefix
  backup_bucket         = aws_s3_bucket.backup.id
  backup_prefix         = "backups"
  backup_retention_days = var.backup_retention_days
  claimer_table_name    = aws_dynamodb_table.node_claims.name

  default_password_secret_name = aws_secretsmanager_secret.default_password.name
  admin_password_secret_name   = aws_secretsmanager_secret.admin_password.name
  writer_password_secret_name  = aws_secretsmanager_secret.writer_password.name

  subnet_cidr = var.subnet_cidr

  cache_disk_size_gb = var.cache_disk_size_gb

  log_shipping_agent    = coalesce(var.log_shipping.agent, "otel")
  log_shipping_endpoint = coalesce(var.log_shipping.endpoint, "")
  log_shipping_headers  = coalesce(var.log_shipping.headers, "")

  tags = local.common_tags
}
