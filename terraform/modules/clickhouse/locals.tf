# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — ClickHouse Module
# ============================================================================

locals {
  config_files = {}

  # Per-tenant S3 disks (data/<tenant>/{replica}/).
  tenant_disks = join("\n", [
    for tenant in var.tenants : trimspace(templatefile("${path.module}/templates/tenant-disk.xml", {
      tenant      = tenant
      data_prefix = var.s3_data_prefix
    }))
  ])

  # Split the 0.9x cache volume budget across all cache wrappers (s3_cache + tenants + CAS when enabled)
  # so total cache max_size plus the raw S3 metadata store never exceeds the volume capacity.
  cache_wrapper_count = 1 + length(var.tenants) + (var.cas_enabled ? length(var.tenants) + 1 : 0)
  tenant_caches = join("\n", [
    for tenant in var.tenants : trimspace(templatefile("${path.module}/templates/tenant-cache.xml", {
      tenant               = tenant
      cache_max_size_bytes = floor(var.cache_disk_size_gb * 0.9 / local.cache_wrapper_count / length(var.tenants)) * 1024 * 1024 * 1024
    }))
  ])

  # CAS (Altinity Antalya) shared content-addressed cold tier (opt-in).
  cas_cache_bytes = var.cas_cache_max_size_bytes > 0 ? var.cas_cache_max_size_bytes : floor(var.cache_disk_size_gb * 0.9 / local.cache_wrapper_count) * 1024 * 1024 * 1024
  cas_disks = var.cas_enabled ? join("\n", concat(
    [trimspace(templatefile("${path.module}/templates/cas-disk.xml", {
      cas_prefix               = var.cas_prefix
      cas_cache_max_size_bytes = local.cas_cache_bytes
    }))],
    [for tenant in var.tenants : trimspace(templatefile("${path.module}/templates/cas-tenant-disk.xml", {
      tenant                   = tenant
      cas_prefix               = var.cas_prefix
      cas_cache_max_size_bytes = local.cas_cache_bytes
    }))],
  )) : ""
  cas_hot_disks_xml = var.nvme_hot_enabled ? "<disk>nvme_hot</disk>\n                        <disk>default</disk>\n                        <load_balancing>least_used</load_balancing>" : "<disk>default</disk>"
  # CAS disabled: render the tiered_cas alias + per-tenant cas_tenant_<tenant>
  # policies through the same non-CAS storage path (cache wrappers), so tables
  # created under the CAS policy names still attach cleanly.
  cas_policies = var.cas_enabled ? join("\n", concat(
    [trimspace(templatefile("${path.module}/templates/cas-policy.xml", {
      nvme_hot_enabled = var.nvme_hot_enabled
    }))],
    [for tenant in var.tenants : trimspace(templatefile("${path.module}/templates/cas-tenant-policy.xml", {
      tenant           = tenant
      nvme_hot_enabled = var.nvme_hot_enabled
    }))],
    )) : join("\n", concat(
    [trimspace(templatefile("${path.module}/templates/cas-fallback-policy.xml", {
      cas_hot_disks_xml = local.cas_hot_disks_xml
    }))],
    [for tenant in var.tenants : trimspace(templatefile("${path.module}/templates/cas-fallback-tenant-policy.xml", {
      tenant            = tenant
      cas_hot_disks_xml = local.cas_hot_disks_xml
    }))],
  ))

  tenant_policies = join("\n", [
    for tenant in var.tenants : trimspace(templatefile("${path.module}/templates/tenant-policy.xml", {
      tenant = tenant
    }))
  ])

  # Config bundle overrides (ssl.xml, network.xml and the default
  # bootstrap-ddl.sql come from the chinit contract module; stacks with
  # their own DDL pass it via the config_files variable).
  config_files_all = merge(local.config_files, {
    "configs/storage.xml" = templatefile("${path.module}/templates/configs/storage.xml", {
      tenant_disks         = local.tenant_disks
      tenant_caches        = local.tenant_caches
      tenant_policies      = local.tenant_policies
      s3_data_prefix       = var.s3_data_prefix
      cas_disks            = local.cas_disks
      cas_policies         = local.cas_policies
      cache_max_size_bytes = floor(var.cache_disk_size_gb * 0.9 / local.cache_wrapper_count) * 1024 * 1024 * 1024
      nvme_hot_enabled     = var.nvme_hot_enabled
    })
  })

  # CHI spec for chinit's in-process loader (replica_count = fixed slot ceiling: writer_max + reader_max + backup).
  chi_spec = templatefile("${path.module}/templates/chi-spec.yaml", {
    cluster_name      = var.cluster_name
    insert_quorum     = var.insert_quorum
    replica_count     = length(var.writer_hostnames) + var.reader_count + (var.backup_replica_enabled ? 1 : 0)
    keepers           = [for h in var.keeper_hostnames : "${h}.${var.dns_zone_name}"]
    log_level         = var.ch_log_level
    text_log_level    = var.ch_text_log_level
    text_log_ttl_line = var.ch_text_log_ttl_days > 0 ? "<ttl>event_date + INTERVAL ${var.ch_text_log_ttl_days} DAY DELETE</ttl>" : ""

    default_password_secret_name = var.default_password_secret_name
    admin_password_secret_name   = var.admin_password_secret_name
    writer_password_secret_name  = var.writer_password_secret_name
    # Keep single_replica_merge_threshold = 0 on two-volume hot(gp3)->cold(cas_cache)
    # policies: active merges happen on the local default gp3 disk (not CAS), and setting
    # >0 assigns 57% of hot merges to 2-vCPU t4g.large readers while forcing 16-vCPU
    # writers to throw Code 234 NO_REPLICA_HAS_PART and fetch merged hot parts over :9010.
    # CAS cold parts deduplicate automatically via CasPartWriteTxn mandatory HEAD and
    # DataPartsExchange CA_RELINK_COOKIE regardless of this setting.
    single_replica_merge_threshold = "0"
    # s3_allow_server_credentials_in_user_queries was added in 26.8 LTS (both
    # upstream 26.8 and antalya-26.8 require it for S3Queue CREATE/runtime);
    # only Altinity Antalya 26.6.x lacks this setting and rejects it with Code 115.
    s3_allow_server_credentials_line = can(regex("^26\\.6\\.", var.clickhouse_version)) ? "" : "default/s3_allow_server_credentials_in_user_queries: \"1\""

    otel_trace_probability    = var.otel_trace_probability
    otel_keeper_trace_prob    = var.otel_keeper_trace_probability
    otel_trace_processors     = var.otel_trace_processors ? "1" : "0"
    otel_trace_cpu_scheduling = var.otel_trace_cpu_scheduling ? "1" : "0"
    otel_span_log_ttl_days    = var.otel_span_log_ttl_days
    trace_log_ttl_days        = var.trace_log_ttl_days
    global_profiler_period_ns = var.global_profiler_period_ns
    log_query_threads         = var.log_query_threads ? "1" : "0"
  })

  # Remote push endpoint resolution (mirrors model.LogShipping in chinit so
  # post_boot convergence configures otelcol + chinit.yaml identically even
  # before a Golden AMI roll).
  raw_base_endpoint    = trimsuffix(trimspace(var.remote_endpoint), "/")
  is_base_otlp         = local.raw_base_endpoint != "" && !endswith(local.raw_base_endpoint, "/v1/logs") && !endswith(local.raw_base_endpoint, "/v1/metrics") && !endswith(local.raw_base_endpoint, "/v1/traces") && !endswith(local.raw_base_endpoint, "/api/v1/write")
  raw_logs_endpoint    = trimsuffix(trimspace(var.logs_endpoint != "" ? var.logs_endpoint : (local.is_base_otlp || endswith(local.raw_base_endpoint, "/v1/logs") ? local.raw_base_endpoint : "")), "/")
  eff_logs_endpoint    = local.raw_logs_endpoint == "" ? "" : (var.remote_agent == "fluentbit" || endswith(local.raw_logs_endpoint, "/v1/logs") ? local.raw_logs_endpoint : "${local.raw_logs_endpoint}/v1/logs")
  raw_metrics_endpoint = trimsuffix(trimspace(var.metrics_endpoint != "" ? var.metrics_endpoint : (local.is_base_otlp || endswith(local.raw_base_endpoint, "/v1/metrics") || endswith(local.raw_base_endpoint, "/api/v1/write") ? local.raw_base_endpoint : "")), "/")
  is_prom_remote_write = local.raw_metrics_endpoint != "" && endswith(local.raw_metrics_endpoint, "/api/v1/write")
  eff_metrics_endpoint = local.raw_metrics_endpoint == "" ? "" : (local.is_prom_remote_write || endswith(local.raw_metrics_endpoint, "/v1/metrics") ? local.raw_metrics_endpoint : "${local.raw_metrics_endpoint}/v1/metrics")
  raw_traces_endpoint  = trimsuffix(trimspace(var.traces_endpoint != "" ? var.traces_endpoint : (local.is_base_otlp || endswith(local.raw_base_endpoint, "/v1/traces") ? local.raw_base_endpoint : "")), "/")
  eff_traces_endpoint  = local.raw_traces_endpoint == "" ? "" : (endswith(local.raw_traces_endpoint, "/v1/traces") ? local.raw_traces_endpoint : "${local.raw_traces_endpoint}/v1/traces")
  eff_metrics_headers  = trimspace(var.metrics_headers != "" ? var.metrics_headers : var.remote_headers)
  eff_traces_headers   = trimspace(var.traces_headers != "" ? var.traces_headers : var.remote_headers)
  has_remote_push      = var.remote_agent == "otel" && (local.eff_logs_endpoint != "" || local.eff_metrics_endpoint != "" || local.eff_traces_endpoint != "")

  logs_headers_yaml = join("\n", [
    for p in compact([for s in split(",", var.remote_headers) : trimspace(s)]) :
    "      ${trimspace(split("=", p)[0])}: ${trimspace(substr(p, length(split("=", p)[0]) + 1, length(p)))}"
    if length(split("=", p)) >= 2
  ])
  metrics_headers_yaml = join("\n", [
    for p in compact([for s in split(",", local.eff_metrics_headers) : trimspace(s)]) :
    "      ${trimspace(split("=", p)[0])}: ${trimspace(substr(p, length(split("=", p)[0]) + 1, length(p)))}"
    if length(split("=", p)) >= 2
  ])
  traces_headers_yaml = join("\n", [
    for p in compact([for s in split(",", local.eff_traces_headers) : trimspace(s)]) :
    "      ${trimspace(split("=", p)[0])}: ${trimspace(substr(p, length(split("=", p)[0]) + 1, length(p)))}"
    if length(split("=", p)) >= 2
  ])

  otelcol_remote_yaml = local.has_remote_push ? join("\n", compact([
    "receivers:",
    (local.eff_metrics_endpoint != "" || local.eff_traces_endpoint != "") ? "  otlp:\n    protocols:\n      http:\n        endpoint: 127.0.0.1:4318" : "",
    local.eff_logs_endpoint != "" ? "  filelog:\n    include: [/var/log/clickhouse-server/*.log, /var/log/clickhouse-server/*-*.log, /var/log/clickhouse-keeper/*.log]" : "",
    local.eff_metrics_endpoint != "" ? "  prometheus:\n    config:\n      scrape_configs:\n        - job_name: clickhouse\n          scrape_interval: ${var.scrape_interval}\n          static_configs:\n            - targets: [\"127.0.0.1:9363\"]\n        - job_name: chinit\n          scrape_interval: ${var.scrape_interval}\n          static_configs:\n            - targets: [\"127.0.0.1:9150\"]" : "",
    "processors:\n  batch: {}\n  resource:\n    attributes:\n      - key: host.name\n        value: $${env:HOSTNAME}\n        action: upsert\n      - key: chinit.cluster\n        value: ${var.cluster_name}\n        action: upsert",
    "exporters:",
    local.eff_logs_endpoint != "" ? join("\n", compact(["  otlphttp:\n    logs_endpoint: ${local.eff_logs_endpoint}", local.logs_headers_yaml != "" ? "    headers:\n${local.logs_headers_yaml}" : ""])) : "",
    local.eff_metrics_endpoint != "" ? (
      local.is_prom_remote_write ?
      join("\n", compact(["  prometheusremotewrite:\n    endpoint: ${local.eff_metrics_endpoint}\n    resource_to_telemetry_conversion:\n      enabled: true", local.metrics_headers_yaml != "" ? "    headers:\n${local.metrics_headers_yaml}" : ""])) :
      join("\n", compact(["  otlphttp/metrics:\n    metrics_endpoint: ${local.eff_metrics_endpoint}", local.metrics_headers_yaml != "" ? "    headers:\n${local.metrics_headers_yaml}" : ""]))
    ) : "",
    local.eff_traces_endpoint != "" ? join("\n", compact(["  otlphttp/traces:\n    traces_endpoint: ${local.eff_traces_endpoint}", local.traces_headers_yaml != "" ? "    headers:\n${local.traces_headers_yaml}" : ""])) : "",
    "service:\n  pipelines:",
    local.eff_logs_endpoint != "" ? "    logs:\n      receivers: [filelog]\n      processors: [resource, batch]\n      exporters: [otlphttp]" : "",
    local.eff_metrics_endpoint != "" ? "    metrics:\n      receivers: [otlp, prometheus]\n      processors: [resource, batch]\n      exporters: [${local.is_prom_remote_write ? "prometheusremotewrite" : "otlphttp/metrics"}]" : "",
    local.eff_traces_endpoint != "" ? "    traces:\n      receivers: [otlp]\n      processors: [resource, batch]\n      exporters: [otlphttp/traces]" : "",
  ])) : ""

  remote_push_post_boot_cmd = local.has_remote_push ? format(
    "bash -c 'mkdir -p /etc/otelcol && echo %s | base64 -d > /etc/otelcol/config.yaml; if [ -x /usr/local/bin/otelcol ]; then printf \"[Unit]\\nDescription=OpenTelemetry Collector\\nAfter=network-online.target\\n[Service]\\nType=simple\\nEnvironment=HOSTNAME=%%H\\nExecStart=/usr/local/bin/otelcol --config=/etc/otelcol/config.yaml\\nRestart=always\\nRestartSec=5\\n[Install]\\nWantedBy=multi-user.target\\n\" > /etc/systemd/system/otelcol.service && systemctl daemon-reload && systemctl enable otelcol.service && systemctl restart otelcol.service; fi; if [ -n %s ] && /usr/local/bin/chinit help 2>&1 | grep -q span-ship; then grep -q traces_endpoint /var/lib/clickhouse-bootstrap/chinit.yaml || printf \"  traces_endpoint: %%s\\n\" %s >> /var/lib/clickhouse-bootstrap/chinit.yaml; printf \"[Unit]\\nDescription=Ship ClickHouse spans\\nAfter=clickhouse-server.service\\n[Service]\\nType=oneshot\\nExecStart=/usr/local/bin/chinit span-ship\\n\" > /etc/systemd/system/span-ship.service && printf \"[Unit]\\nDescription=Forward ClickHouse spans every 30s\\n[Timer]\\nOnCalendar=*-*-* *:*:00/30\\nPersistent=true\\n[Install]\\nWantedBy=timers.target\\n\" > /etc/systemd/system/span-ship.timer && systemctl daemon-reload && systemctl enable --now span-ship.timer; fi'",
    base64encode("${local.otelcol_remote_yaml}\n"),
    jsonencode(local.eff_traces_endpoint),
    jsonencode(local.eff_traces_endpoint),
  ) : ""

  telemetry_lifecycle_hooks = local.has_remote_push ? [{
    event           = "post_boot"
    name            = "configure-remote-telemetry"
    command         = jsonencode(local.remote_push_post_boot_cmd)
    timeout_seconds = 60
    critical        = false
  }] : []

  # CHK spec for the keeper nodes - rendered with the keeper fleet size,
  # uploaded next to chi.yaml; keeper cloud-init renders keeper config from
  # it (raft entries get cloud DNS at render time).
  keeper_spec = templatefile("${path.module}/templates/keeper-spec.yaml", {
    cluster_name = var.cluster_name
    keeper_count = length(var.keeper_hostnames)
  })

}
