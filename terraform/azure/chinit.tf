# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# The chinit contract — consumed from the contract module (Azure)
# ============================================================================
# Same pattern as ../aws: the all-in-one module does NOT render
# specs/bundles itself — it calls terraform/modules/chinit/azure and
# adds the cloud resources around it.
# ============================================================================

module "chinit" {
  source = "../modules/chinit/azure"

  cluster_name  = var.cluster_name
  region        = var.location
  dns_zone_name = var.dns_zone_name

  writer_max       = var.writer_max
  reader_max       = var.reader_max
  reader_count     = var.reader_desired
  keeper_hostnames = local.keeper_hostnames
  enable_backup    = var.enable_backup

  # Cloud resources this module owns and the specs point at.
  storage_account_name        = azurerm_storage_account.this.name
  scripts_container_name      = azurerm_storage_container.scripts.name
  data_container_name         = azurerm_storage_container.data.name
  backup_storage_account_name = azurerm_storage_account.this.name
  backup_container_name       = azurerm_storage_container.backup.name
  backup_retention_days       = var.backup_retention_days
  claimer_table_name          = azurerm_storage_table.node_claims.name

  default_password_secret_name = azurerm_key_vault_secret.default_password.name
  admin_password_secret_name   = azurerm_key_vault_secret.admin_password.name
  writer_password_secret_name  = azurerm_key_vault_secret.writer_password.name

  subnet_cidr = var.subnet_cidr

  cache_disk_size_gb = var.cache_disk_size_gb

  # chinit's Azure env contract.
  subscription_id            = var.subscription_id
  resource_group_name        = var.resource_group_name
  key_vault_name             = azurerm_key_vault.this.name
  managed_identity_client_id = azurerm_user_assigned_identity.nodes.client_id

  vm_names = {
    writers = "${local.name_prefix}-writers"
    readers = "${local.name_prefix}-readers"
    keepers = "clickhouse-keeper" # chinit's keeper VM names: clickhouse-keeper-N
    backup  = var.enable_backup ? "${local.name_prefix}-backup-0" : ""
  }

  log_shipping_agent            = coalesce(var.log_shipping.agent, "otel")
  log_shipping_endpoint         = coalesce(var.log_shipping.endpoint, "")
  log_shipping_logs_endpoint    = coalesce(var.log_shipping.logs_endpoint, "")
  log_shipping_metrics_endpoint = coalesce(var.log_shipping.metrics_endpoint, "")
  log_shipping_traces_endpoint  = coalesce(var.log_shipping.traces_endpoint, "")
  log_shipping_headers          = coalesce(var.log_shipping.headers, "")
  log_shipping_metrics_headers  = coalesce(var.log_shipping.metrics_headers, "")
  log_shipping_traces_headers   = coalesce(var.log_shipping.traces_headers, "")
  log_shipping_scrape_interval  = coalesce(var.log_shipping.scrape_interval, "15s")

  ch_otel_trace_probability        = coalesce(var.tracing_config.otel_trace_probability, 0)
  ch_otel_keeper_trace_probability = coalesce(var.tracing_config.otel_keeper_trace_probability, "auto")
  ch_otel_trace_processors         = coalesce(var.tracing_config.otel_trace_processors, false)
  ch_otel_span_log_ttl_days        = coalesce(var.tracing_config.otel_span_log_ttl_days, 3)
  ch_trace_log_ttl_days            = coalesce(var.tracing_config.trace_log_ttl_days, 7)
  ch_global_profiler_period_ns     = coalesce(var.tracing_config.global_profiler_period_ns, 0)
  ch_log_query_threads             = coalesce(var.tracing_config.log_query_threads, false)

  tags = local.common_tags
}
