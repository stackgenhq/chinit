# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# chinit cluster module (AWS) — the ecosystem-composed all-in-one
# ============================================================================
# One small facade that composes the production-hardened ecosystem modules
# (network, storage, secrets, clickhouse, compute, dns_tls, monitoring,
# security) around the chinit contract. Every input has a secure default;
# private-DNS-only by default (no load balancers, no public exposure) —
# flip enable_nlbs-style paths later or compose the ecosystem modules
# directly when the facade runs out of room. The facade is the on-ramp,
# not the ceiling.
# ============================================================================

module "network" {
  source = "../modules/network"

  aws_region   = var.region
  cluster_name = var.cluster_name

  # Fresh VPC + private subnet + free S3/DynamoDB gateway endpoints; the
  # bring-your-own path lives on the network module directly (facade keeps
  # the simple create/attach choice).
  vpc_cidr                         = var.subnet_cidr
  subnet_cidr                      = var.subnet_cidr
  enable_nat_gateway               = false
  create_s3_gateway_endpoint       = true
  create_dynamodb_gateway_endpoint = true

  # Client ingress reaches the per-role SGs from exactly these CIDRs.
  allowed_source_ips = var.client_cidrs

  tags = local.common_tags
}

module "storage" {
  source = "../modules/storage"

  cluster_name = var.cluster_name

  data_bucket_name      = local.data_bucket_name
  backup_bucket_name    = local.backup_bucket_name
  tenants               = var.tenants
  data_prefix           = var.s3_data_prefix
  backup_prefix         = "backups"
  bootstrap_prefix      = "bootstrap"
  lifecycle_days_to_ia  = var.lifecycle_days_to_ia
  cas_prefix            = var.cas_enabled ? var.cas_prefix : ""
  backup_retention_days = var.backup_retention_days

  # SSE-S3 (free): at ClickHouse part-merge rates, SSE-KMS per-part API
  # charges dominate the storage bill. Public access blocked + TLS-only
  # transport are enforced inside the module.
  vpc_id              = module.network.vpc_id
  subnet_id           = module.network.subnet_id
  flow_log_bucket_arn = module.network.flow_log_bucket_arn

  tags = local.common_tags
}

module "secrets" {
  source = "../modules/secrets"

  cluster_name = var.cluster_name

  allowed_role_arns = [
    module.storage.writer_role_arn,
    module.storage.reader_role_arn,
    module.storage.keeper_role_arn,
    module.storage.backup_role_arn,
  ]

  rotation = {
    enabled       = local.rotation_enabled
    rotation_days = var.rotation_days
  }

  tags = local.common_tags
}

module "clickhouse" {
  source = "../modules/clickhouse"

  cluster_name  = var.cluster_name
  timezone      = "UTC"
  aws_region    = var.region
  dns_zone_name = var.dns_zone_name

  writer_hostnames       = local.writer_hostnames
  keeper_hostnames       = local.keeper_hostnames
  reader_count           = var.reader_max
  backup_replica_enabled = var.enable_backup

  tenants = var.tenants

  s3_data_bucket     = module.storage.data_bucket_name
  clickhouse_version = var.cas_enabled ? "26.8.34.20001.altinityantalya" : "26.8.36.3"
  s3_data_prefix     = var.s3_data_prefix
  s3_backup_bucket   = module.storage.backup_bucket_name
  s3_backup_prefix   = "backups"

  bootstrap_bucket = module.storage.data_bucket_name
  bootstrap_prefix = "bootstrap"

  default_password_secret_name = module.secrets.default_password_secret_name
  admin_password_secret_name   = module.secrets.admin_password_secret_name
  writer_password_secret_name  = module.secrets.writer_password_secret_name

  tls_certificate_secret_name    = ""
  tls_private_key_secret_name    = ""
  tls_ca_certificate_secret_name = ""

  subnet_cidr        = module.network.subnet_cidr_block
  nlb_subnet_cidr    = module.network.subnet_cidr_block
  cache_disk_size_gb = var.cache_disk_size_gb

  admin_allowed_networks        = join(",", distinct(concat(var.client_cidrs, [module.network.subnet_cidr_block])))
  writer_allowed_networks       = join(",", distinct(concat(var.client_cidrs, [module.network.subnet_cidr_block])))
  default_user_allowed_networks = join(",", distinct(concat(var.client_cidrs, [module.network.subnet_cidr_block])))

  claimer_table_name = aws_dynamodb_table.node_claims.name

  remote_agent     = coalesce(var.log_shipping.agent, "otel")
  remote_endpoint  = coalesce(var.log_shipping.endpoint, "")
  logs_endpoint    = coalesce(var.log_shipping.logs_endpoint, "")
  metrics_endpoint = coalesce(var.log_shipping.metrics_endpoint, "")
  traces_endpoint  = coalesce(var.log_shipping.traces_endpoint, "")
  remote_headers   = coalesce(var.log_shipping.headers, "")
  metrics_headers  = coalesce(var.log_shipping.metrics_headers, "")
  traces_headers   = coalesce(var.log_shipping.traces_headers, "")
  scrape_interval  = coalesce(var.log_shipping.scrape_interval, "15s")
  fluentbit_output = coalesce(var.log_shipping.fluentbit_output, "otlp")

  otel_trace_probability        = coalesce(var.tracing_config.otel_trace_probability, 0)
  otel_keeper_trace_probability = coalesce(var.tracing_config.otel_keeper_trace_probability, "auto")
  otel_trace_processors         = coalesce(var.tracing_config.otel_trace_processors, false)
  otel_trace_cpu_scheduling     = coalesce(var.tracing_config.otel_trace_cpu_scheduling, false)
  otel_span_log_ttl_days        = coalesce(var.tracing_config.otel_span_log_ttl_days, 3)
  trace_log_ttl_days            = coalesce(var.tracing_config.trace_log_ttl_days, 7)
  global_profiler_period_ns     = coalesce(var.tracing_config.global_profiler_period_ns, 0)
  log_query_threads             = coalesce(var.tracing_config.log_query_threads, false)

  backup_retention_days     = var.backup_retention_days
  backup_compression_format = "tar"
  backup_schedule           = "0 2 * * *"
  backup_on_demand          = false


  cas_enabled              = var.cas_enabled
  cas_prefix               = var.cas_prefix
  cas_cache_max_size_bytes = 0
  cold_move_ttl_days       = var.cas_enabled ? 1 : 7
  nvme_hot_enabled         = false

  autoscaling_enabled = false
  writers_asg_name    = "${var.cluster_name}-writers"

  tags = local.common_tags

  depends_on = [module.storage, module.secrets]
}

data "aws_ami" "public_golden" {
  count            = var.golden_ami_id == null ? 1 : 0
  most_recent      = true
  executable_users = ["all"]

  filter {
    name   = "name"
    values = [var.cas_enabled ? "clickhouse-golden-*altinityantalya-*" : "clickhouse-golden-*"]
  }

  filter {
    name   = "description"
    values = [var.cas_enabled ? "*(antalya)*https://github.com/stackgenhq/chinit*" : "*(upstream)*https://github.com/stackgenhq/chinit*"]
  }

  filter {
    name   = "architecture"
    values = ["arm64"]
  }

  filter {
    name   = "is-public"
    values = ["true"]
  }

  filter {
    name   = "state"
    values = ["available"]
  }
}

locals {
  golden_ami_id = var.golden_ami_id != null ? var.golden_ami_id : data.aws_ami.public_golden[0].id
}

# ----------------------------------------------------------------------------
# Node ordinal claims table (the lease identity store)
# ----------------------------------------------------------------------------
resource "aws_dynamodb_table" "node_claims" {
  name         = "${var.cluster_name}-node-claims"
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

  tags = merge(local.common_tags, { Name = "${var.cluster_name}-node-claims" })
}
