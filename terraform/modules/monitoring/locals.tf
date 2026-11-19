# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — Monitoring Module
# ============================================================================

locals {
  alarm_prefix = var.cluster_name

  # S3Queue watermark metric names (emitted via Prometheus EMF with ClusterName dimension).
  s3queue_seen_metric      = "ClickHouseDimensionalMetrics_object_storage_queue_newest_seen_object_timestamp_seconds"
  s3queue_committed_metric = "ClickHouseDimensionalMetrics_object_storage_queue_newest_committed_object_timestamp_seconds"

  alert_topic_arns = distinct(compact(concat([aws_sns_topic.alerts.arn], var.alert_topic_arns)))

  log_group_names = {
    server     = "/clickhouse/${var.cluster_name}/server"
    error      = "/clickhouse/${var.cluster_name}/error"
    keeper     = "/clickhouse/${var.cluster_name}/keeper"
    bootstrap  = "/clickhouse/${var.cluster_name}/bootstrap"
    prometheus = "/clickhouse/${var.cluster_name}/prometheus"
  }

  # Default CloudWatch Logs metric-filter alarm patterns when var.log_alarm_patterns is null.
  curated_log_alarm_patterns = [
    {
      name      = "readonly-mode"
      pattern   = "\"Read-only mode\""
      log_group = "server"
      threshold = 0
      guidance  = "Tables flipped read-only (quorum loss or disk pressure) — INSERTs are failing cluster-wide. Check keeper health (keeper-down alarm) and metadata/cache disk space."
    },
    {
      name      = "memory-limit-exceeded"
      pattern   = "\"Memory limit\" exceeded"
      log_group = "server"
      threshold = 5
      guidance  = "Queries are being OOM-killed. Check max_memory_usage settings and the writer memory alarm; sustained occurrences mean the per-node memory budget is too small — tune query memory settings or bump the writer launch-template instance type (rolls via ASG instance refresh)."
    },
    {
      name      = "too-many-parts"
      pattern   = "\"Too many parts\""
      log_group = "server"
      threshold = 0
      guidance  = "Merge backlog on a partition — inserts will throttle. Check writer CPU (merge pressure); the writers ASG step policy adds capacity, or consider lowering insert frequency / raising autoscaling_config.writer_max."
    },
    {
      name      = "no-space-left"
      pattern   = "\"No space left\""
      log_group = "error"
      threshold = 0
      guidance  = "Disk exhaustion on a node. gp3 metadata/cache volumes resize ONLINE — raise metadata_disk_size_gb / cache_disk_size_gb in tfvars and apply."
    },
    {
      name      = "tls-handshake-failure"
      pattern   = "\"Cannot initialize connection\""
      log_group = "error"
      threshold = 30 # >30/min ignores brief boot readiness probe bursts while catching 1/sec handshake storms
      guidance  = "TLS handshake failure storm (>=30/min). In acm mode the nodes serve self-signed certs BY DESIGN (client TLS terminates at the NLB) — a client failing 'unknown ca' is pointed at the NODE, not the NLB. On the node: sudo ss -tnp | grep 9440 to find the PID; point it at 127.0.0.1:9000 (plaintext loopback) or at the NLB DNS name (ACM cert). Do NOT disable TLS fleet-wide."
    },
    {
      name      = "high-cpu-internal"
      pattern   = "\"High CPU usage\""
      log_group = "server"
      threshold = 5
      guidance  = "ClickHouse's INTERNAL CPU monitor reports sustained saturation (>=90% for 10+ min) while the CW-average the ASG keys on lags. Check: (1) query_log for heavy queries; (2) background_pool_size vs vCPU (chi-spec renders 16 threads of merge/move pools on a 2-4 vCPU t4g node); (3) t4g credit exhaustion (CPUCreditBalance near 0 throttles real CPU while CW still shows ~100% — launch templates set unlimited credits to prevent this); (4) whether a TLS-handshake failure storm (see the tls-handshake-failure alarm) is burning CPU on the secure listeners."
    },
    {
      name      = "auth-failure"
      pattern   = "AUTHENTICATION_FAILED"
      log_group = "error"
      threshold = 10
      guidance  = "More than 10 authentication failures/min on a node. Distinguish LOCAL (127.0.0.1 peer — a chinit render/config drift or a stale secret after rotation: check /etc/clickhouse-server/users.d/ has ONE users file (z_chctl-users.xml) and its hashes match the Secrets Manager values; re-run chinit boot --refresh or the schema-sync unit) from REMOTE (the NLB — a client with a stale credential after rotation)."
    },
  ]

  effective_log_alarm_patterns = var.log_alarm_patterns != null ? var.log_alarm_patterns : local.curated_log_alarm_patterns
}
