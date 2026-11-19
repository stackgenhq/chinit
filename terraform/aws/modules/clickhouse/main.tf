# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse Module — extends the open-source chinit contract module
# ============================================================================
# Why: Renders the tenant/CAS tiered storage.xml and the tuned CHI/CHK
#      specs into the SHA-gated S3 bootstrap bundle (the role specs,
#      user-data wrappers and default DDL come from the contract module).
# Without it: EC2 nodes have no bootstrap configuration and cannot form a cluster.
# ============================================================================

module "chinit" {
  # Co-located in this repo — local path, mirroring the all-in-one
  # modules in terraform/aws + terraform/azure. The ?ref=main pin is
  # gone: this module ships in the same commit as the contract module.
  source = "../chinit"

  cluster_name  = var.cluster_name
  region        = var.aws_region
  dns_zone_name = var.dns_zone_name
  timezone      = var.timezone

  writer_max       = length(var.writer_hostnames)
  writer_hostnames = var.writer_hostnames
  reader_max       = var.reader_count
  reader_count     = var.reader_count
  keeper_hostnames = var.keeper_hostnames
  enable_backup    = var.backup_replica_enabled

  bootstrap_bucket          = var.bootstrap_bucket
  bootstrap_prefix          = var.bootstrap_prefix
  data_bucket               = var.s3_data_bucket
  data_prefix               = var.s3_data_prefix
  backup_bucket             = var.s3_backup_bucket
  backup_prefix             = var.s3_backup_prefix
  backup_retention_days     = var.backup_retention_days
  backup_compression_format = var.backup_compression_format
  backup_schedule           = var.backup_schedule
  backup_on_demand          = var.backup_on_demand
  claimer_table_name        = var.claimer_table_name

  ch_log_level         = var.ch_log_level
  ch_text_log_level    = var.ch_text_log_level
  ch_text_log_ttl_days = var.ch_text_log_ttl_days
  subnet_cidr          = var.subnet_cidr
  nlb_subnet_cidr      = var.nlb_subnet_cidr
  cache_disk_size_gb   = var.cache_disk_size_gb

  default_password_secret_name   = var.default_password_secret_name
  admin_password_secret_name     = var.admin_password_secret_name
  writer_password_secret_name    = var.writer_password_secret_name
  tls_certificate_secret_name    = var.tls_certificate_secret_name
  tls_private_key_secret_name    = var.tls_private_key_secret_name
  tls_ca_certificate_secret_name = var.tls_ca_certificate_secret_name

  admin_allowed_networks        = var.admin_allowed_networks
  writer_allowed_networks       = var.writer_allowed_networks
  default_user_allowed_networks = var.default_user_allowed_networks

  log_shipping_agent    = var.remote_agent
  log_shipping_endpoint = local.eff_logs_endpoint != "" ? local.eff_logs_endpoint : var.remote_endpoint
  log_shipping_headers  = var.remote_headers
  fluentbit_output      = var.fluentbit_output
  lifecycle_hooks       = local.telemetry_lifecycle_hooks

  config_files = merge(
    {
      "configs/storage.xml" = local.config_files_all["configs/storage.xml"]
    },
    # Caller DDL (or any other bundle-member override) — e.g. a stack's own
    # S3Queue staging pipeline as configs/bootstrap-ddl.sql. The contract
    # module merges these OVER its static defaults, and the SHA gate digest
    # covers the closed bundle-member key list either way.
    var.extra_config_files,
  )
  chi_spec    = local.chi_spec
  keeper_spec = local.keeper_spec

  autoscaling_enabled = var.autoscaling_enabled
  keeper_type_ladder  = var.keeper_type_ladder
  keeper_min_type     = var.keeper_min_type
  keeper_max_type     = var.keeper_max_type
  writer_type_ladder  = var.writer_type_ladder
  writer_min_type     = var.writer_min_type
  writer_max_type     = var.writer_max_type
  writers_asg_name    = var.writers_asg_name

  multi_az_enabled      = var.multi_az_enabled
  multi_az_cold_tier    = var.multi_az_cold_tier
  subnet_cidrs_multi_az = var.subnet_cidrs_multi_az

  # Stage-0 node tuning for ASG replacements + Antalya CAS (the script
  # itself lives in templates/wrapper-extra.sh.tftpl; the leading comment
  # block is stripped so the user-data stays minimal):
  # 1. Bind CH_CAS_SERVER_ROOT_ID in /etc/default/clickhouse (loaded by stock
  #    clickhouse-server.service alongside /etc/default/clickhouse-server) to
  #    the EC2 instance ID on ephemeral-EBS ASG writers/readers (and stable
  #    "backup-0" on persistent-EBS backup-0), preventing Code 246 CAS owner
  #    UUID collisions across ASG replacements while preserving same-UUID
  #    fast remounts across restarts/reboots of the same instance.
  # 2. Set LimitNPROC=65536 + TasksMax=infinity so 8 GiB readers never emit
  #    the "Maximum number of threads is lower than 30000" Context warning.
  # Drop the SPDX/doc comment lines, then trim only LEADING blank lines:
  # the value must keep its trailing newline so the contract template's
  # "wrapper_extra + a blank line" render stays byte-identical to the
  # original heredoc (a spurious user-data diff would force an LT roll
  # on every adopter).
  wrapper_extra = "${join("\n", compact([
    for line in split("\n", templatefile("${path.module}/templates/wrapper-extra.tftpl.sh", {})) :
    line if !startswith(line, "#")
  ]))}\n"

  tags = var.tags
}

