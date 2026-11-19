# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Monitoring Module — CloudWatch alarms + scaling guidance + budgets
# ============================================================================
# Why: CloudWatch-native cluster alerting. Emits three alarm classes:
#   1. Health alarms (CPU/mem/disk, NLB health, keeper up, backup freshness)
#   2. Horizontal scale alarms — readers + writers ASG capacity headroom;
#      the writer CPU/backpressure/idle alarms also INVOKE the writers ASG
#      step-scaling policies (action ARNs from the compute module)
#   3. Scale-guidance alarms — each message names the exact tfvars variable
#      to change, so acting on an alarm is a one-line edit.
# Plus idle-capacity shrink alerts so the cluster scales DOWN with load, not
# just up. Thresholds follow common ClickHouse operator practice.
# Without it: Outages go undetected until users report them; operators have
#             no signal for when to scale horizontally vs vertically.
# ============================================================================

# ----------------------------------------------------------------------------
# CloudWatch Logs groups + retention
# The CloudWatch agent creates these groups implicitly on first ship, with retention NEVER EXPIRE — a quiet, unbounded cost leak (ingest + storage…
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_log_group" "node_logs" {
  for_each = local.log_group_names

  name              = each.value
  retention_in_days = var.log_retention_days == 0 ? 0 : var.log_retention_days

  tags = merge(var.tags, {
    Name = each.value
  })
}

# ----------------------------------------------------------------------------
# Log-pattern alarms — content-level signals metrics cannot see
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_log_metric_filter" "patterns" {
  for_each = { for p in local.effective_log_alarm_patterns : p.name => p }

  name           = "${var.cluster_name}-${each.key}"
  log_group_name = aws_cloudwatch_log_group.node_logs[each.value.log_group].name
  pattern        = each.value.pattern

  metric_transformation {
    # PutMetricFilter rejects dimensions on term-based filter patterns.
    name      = "LogPattern_${each.key}"
    namespace = "ClickHouse/Node"
    value     = "1"
  }
}

resource "aws_cloudwatch_metric_alarm" "log_pattern" {
  for_each = { for p in local.effective_log_alarm_patterns : p.name => p }

  alarm_name          = "${local.alarm_prefix}-log-${each.key}"
  namespace           = "ClickHouse/Node"
  metric_name         = "LogPattern_${each.key}"
  statistic           = "Sum"
  period              = 60
  evaluation_periods  = 1
  threshold           = each.value.threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  alarm_description = "Log pattern '${each.key}' (${each.value.pattern}) seen more than ${each.value.threshold} time(s) in the ${each.value.log_group} log group in the last minute. ${each.value.guidance}"

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# SNS topic for alarms
# ----------------------------------------------------------------------------
resource "aws_sns_topic" "alerts" {
  name = "${var.cluster_name}-clickhouse-alerts"

  tags = var.tags
}

resource "aws_sns_topic_subscription" "email" {
  count     = length(var.alert_emails) > 0 ? length(var.alert_emails) : 0
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = var.alert_emails[count.index]
}

resource "aws_sns_topic_subscription" "webhook" {
  count     = length(var.alert_webhooks) > 0 ? length(var.alert_webhooks) : 0
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "https"
  endpoint  = var.alert_webhooks[count.index]
}

# ----------------------------------------------------------------------------
# Health alarms — writers/keepers/backup (per-node CPU/mem/disk)
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "writer_cpu" {
  count = var.enabled && length(var.writer_hostnames) > 0 ? length(var.writer_hostnames) : 0

  alarm_name          = "${local.alarm_prefix}-writer-${count.index}-cpu-high"
  namespace           = "ClickHouse/Node"
  metric_name         = "cpu_usage_idle"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 6
  threshold           = 100 - var.writer_cpu_threshold
  comparison_operator = "LessThanThreshold"
  # Vacant writer ordinal slots emit no datapoints; notBreaching prevents scaled-in slots from sticking in ALARM.
  treat_missing_data = "notBreaching"

  dimensions = {
    host = var.writer_hostnames[count.index]
    cpu  = "cpu-total"
  }

  alarm_description = "Writer CPU above ${var.writer_cpu_threshold}% for 30 minutes - GUIDANCE ONLY (SNS, no step-scaling action). Aggregate writer CPU scaling is owned by the writers ASG TargetTracking policy (ASGAverageCPUUtilization), and backpressure step-scaling is owned by cluster-level writer-insert-backpressure / ch-parts-per-partition-high. Wiring a scaling policy to a per-host alarm whose metric disappears on scale-in wedges the ASG in a launch/terminate oscillation loop. If sustained across the fleet at max capacity (see writers-asg-cpu-high), raise autoscaling_config.writer_max in tfvars."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# 80%-max rule).
resource "aws_cloudwatch_metric_alarm" "keeper_cpu" {
  count = var.enabled && length(var.keeper_hostnames) > 0 ? length(var.keeper_hostnames) : 0

  alarm_name         = "${local.alarm_prefix}-keeper-${count.index}-cpu-high"
  namespace          = "ClickHouse/Node"
  metric_name        = "cpu_usage_idle"
  statistic          = "Average"
  period             = 300
  evaluation_periods = 6
  # cpu_usage_idle: threshold on the COMPLEMENT (100 - target), matching
  # the writer CPU alarm shape (the agent reports idle, not busy).
  threshold           = 100 - var.keeper_cpu_threshold
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    host = var.keeper_hostnames[count.index]
    cpu  = "cpu-total"
  }

  alarm_description = "Keeper CPU above ${var.keeper_cpu_threshold}% for 30 minutes. ACTION: the chinit autoscale coordinator (keeper-0's timer) reads this signal with the ensemble's own metrics and steps itself up the keeper_type_ladder (stop -> modify -> start, same IP + raft disk). If this persists at keeper_max_type, the ladder ceiling in tfvars is the constraint."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# Why SNS-only: memory is not a horizontal signal (a new writer absorbs query/merge load, but per-node memory pressure is node-local), so…
resource "aws_cloudwatch_metric_alarm" "writer_memory" {
  count = var.enabled && length(var.writer_hostnames) > 0 ? length(var.writer_hostnames) : 0

  alarm_name          = "${local.alarm_prefix}-writer-${count.index}-memory-high"
  namespace           = "ClickHouse/Node"
  metric_name         = "mem_used_percent"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 6
  threshold           = var.writer_memory_threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    host = var.writer_hostnames[count.index]
  }

  alarm_description = "Writer memory above ${var.writer_memory_threshold}% for 30 minutes. Memory pressure is node-local (adding writers absorbs query/merge load, not per-node RAM), so this is guidance, not an automatic action: check the node's top queries via SSM Session Manager; if cache-bound raise metadata_disk_size_gb / cache_disk_size_gb in tfvars, for ingest headroom raise autoscaling_config.writer_max."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Horizontal scale alarms — readers ASG
# ----------------------------------------------------------------------------
# Why: The ASG target-tracking policy scales readers automatically; this
#      alarm fires when CPU stays high even AT max capacity — the signal to
#      raise autoscaling_config.max_readers (or reader_count when static).
resource "aws_cloudwatch_metric_alarm" "readers_asg_cpu" {
  count = var.enabled && var.autoscaling_enabled ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-readers-asg-cpu-high"
  namespace           = "AWS/EC2"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 6
  threshold           = var.reader_cpu_scale_out
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    AutoScalingGroupName = var.readers_asg_name
  }

  alarm_description = "Readers ASG average CPU above ${var.reader_cpu_scale_out}% for 30 minutes. Check whether the ASG hit autoscaling_config.max_readers; if yes, HORIZONTAL SCALE GUIDANCE: raise max_readers in tfvars. Each added reader costs ~35% less on the spot split."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# The step-scaling policies (wired into writer-cpu-high / writer-insert-backpressure as alarm actions) stop having an effect at ASG max capacity —…
resource "aws_cloudwatch_metric_alarm" "writers_asg_cpu" {
  count = var.enabled && var.writers_asg_name != null ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-writers-asg-cpu-high"
  namespace           = "AWS/EC2"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 6
  threshold           = var.writer_cpu_threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    AutoScalingGroupName = var.writers_asg_name
  }

  alarm_description = "Writers ASG average CPU above ${var.writer_cpu_threshold}% for 30 minutes. Check whether the writers ASG is at max capacity; if yes, HORIZONTAL SCALE GUIDANCE: raise autoscaling_config.writer_max in tfvars and apply (writer_max is also the writer ordinal claim ceiling — the CHI spec re-renders CR slots from it, so it must change via apply, never by editing the ASG alone)."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# Idle-capacity shrink alert (weekly window)
# Why: Costs must fall when load falls. Avg CPU under threshold for 7 days
#      means over-provisioning: shrink reader_count or drop an instance size.
resource "aws_cloudwatch_metric_alarm" "readers_asg_idle" {
  count = var.enabled && var.enable_idle_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-readers-asg-idle"
  namespace           = "AWS/EC2"
  metric_name         = "CPUUtilization"
  statistic           = "Average"
  period              = 3600
  evaluation_periods  = 168 # 7 days of hours
  threshold           = var.idle_cpu_threshold
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    AutoScalingGroupName = var.readers_asg_name
  }

  alarm_description = "Readers ASG average CPU below ${var.idle_cpu_threshold}% for 7 days. SCALE-IN GUIDANCE: reduce reader_count (or autoscaling_config.min_readers) — spot split preserves the on-demand floor automatically."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "writer_disk" {
  count = var.enabled && length(var.writer_hostnames) > 0 ? length(var.writer_hostnames) : 0

  alarm_name          = "${local.alarm_prefix}-writer-${count.index}-disk-high"
  namespace           = "ClickHouse/Node"
  metric_name         = "disk_used_percent"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 3
  threshold           = var.disk_percent_threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  # Full dimension set of the disk metric (path/host/device/fstype) - the
  # metadata disk (/var/lib/clickhouse on its own NVMe volume) is the
  # bounded one: parts catalogs grow; S3 is unbounded.
  dimensions = {
    host   = var.writer_hostnames[count.index]
    path   = "/var/lib/clickhouse"
    device = "nvme1n1"
    fstype = "ext4"
  }

  alarm_description = "Writer metadata/cache disk above ${var.disk_percent_threshold}% used. VERTICAL SCALE-UP GUIDANCE: gp3 volumes resize ONLINE (no restart) — raise metadata_disk_size_gb / cache_disk_size_gb in tfvars. Data lives in S3; only metadata/cache is bounded."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# NLB health alarms
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "writers_lb_unhealthy" {
  count = var.enabled ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-writers-lb-unhealthy-hosts"
  namespace           = "AWS/NetworkELB"
  metric_name         = "UnHealthyHostCount"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 5
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"

  # NetworkELB emits per TargetGroup: without the TG dimension the
  # series never resolves (INSUFFICIENT_DATA forever — the blind-LB
  # finding).
  dimensions = {
    LoadBalancer = var.writers_lb_arn_suffix
    TargetGroup  = var.writers_http_tg_arn_suffix
  }

  alarm_description = "Writers NLB has unhealthy targets. A writer node failed its health check — check the node via SSM Session Manager, clickhouse-server logs at /var/log/clickhouse-server/."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "readers_lb_unhealthy" {
  count = var.enabled ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-readers-lb-unhealthy-hosts"
  namespace           = "AWS/NetworkELB"
  metric_name         = "UnHealthyHostCount"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 5
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"

  dimensions = {
    LoadBalancer = var.readers_lb_arn_suffix
    TargetGroup  = var.readers_http_tg_arn_suffix
  }

  alarm_description = "Readers NLB has unhealthy targets. Note: on the spot split, up to floor(reader_count * reader_spot_ratio) readers can be reclaimed at once — the on-demand floor keeps queries served; alarm persists when the floor itself is unhealthy."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# NLB p99 latency — insert-pathology signal for the writers fleet (guidance;
# the writers ASG step policies act on CPU/backpressure, not on NLB latency)
resource "aws_cloudwatch_metric_alarm" "writers_lb_latency" {
  count = var.enabled && var.enable_vertical_guidance ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-writers-lb-p99-latency"
  namespace           = "AWS/NetworkELB"
  metric_name         = "TargetResponseTime"
  extended_statistic  = "p99"
  period              = 300
  evaluation_periods  = 6
  threshold           = 1.0
  comparison_operator = "GreaterThanThreshold"

  dimensions = {
    LoadBalancer = var.writers_lb_arn_suffix
    TargetGroup  = var.writers_http_tg_arn_suffix
  }

  alarm_description = "Writers NLB p99 target response time above 1s for 30 minutes. If inserts are the workload: SCALE GUIDANCE — the writer-cpu-high / writer-insert-backpressure alarms step the writers ASG up; if they persist at max capacity, raise autoscaling_config.writer_max in tfvars. If queries: add readers (autoscaling_config.max_readers)."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Replication catch-up stall alarm (ReplicasMaxAbsoluteDelay)
# a replica whose replication queue stops catching up is INVISIBLE to every node-local alarm — the node is up, CPU is fine, inserts succeed on the…
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "replication_stalled" {
  count = var.enabled && var.enable_writer_alarms ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-replication-stalled"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseAsyncMetrics_ReplicasMaxAbsoluteDelay"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 6 # sustained 30 minutes — schema-sync catch-up bursts pass
  threshold           = 300
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  # The prometheus bridge emits these with the NodeType/ClusterName dims
  # only (append_dimensions with custom keys fails agent validation on
  # EC2 — the node-metric alarms key on the auto-appended InstanceID;
  # these ride the bridge's own labels).
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "writer"
  }

  alarm_description = "A writer's replication queue stopped catching up (ReplicasMaxAbsoluteDelay > 5min for 30 min): a replica is table-less or its fetch path is wedged while the node passes every health check. ACTION: (1) check system.replicas / replication_queue on the writers for a stuck queue; (2) verify the private-zone DNS records match the live lease claims (a stale A record wedges the fetch path — the 0.2.14+ lease-renew convergence heals this; check the chinit version); (3) re-run chinit schema-sync on the laggard node. If one writer persistently diverges, its bootstrap never completed — compare system.tables across writers."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Merge-backlog early-warning alarm (MaxPartCountForPartition)
# parts-to-throw-insert rejects INSERTs at 3000 parts per partition — the terminal state of a merge spiral;
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "parts_per_partition_high" {
  count = var.enabled && var.enable_writer_alarms ? 1 : 0

  alarm_name         = "${local.alarm_prefix}-parts-per-partition-high"
  namespace          = "ClickHouse/Server"
  metric_name        = "ClickHouseAsyncMetrics_MaxPartCountForPartition"
  statistic          = "Maximum"
  period             = 300
  evaluation_periods = 3 # sustained 15 minutes
  # The ch_alert_thresholds variable (default 1500): under heavy single-shard
  # ingest parts legitimately ride 900+ while merges catch up (a 150 hardcode
  # stays chronic-ALARM under sustained load — chronic-alarm debt that trains
  # people to ignore the panel). parts_to_throw_insert is 5000; 1500 keeps
  # the warning genuine.
  threshold           = var.ch_alert_thresholds.max_parts_per_partition
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "writer"
  }

  alarm_description = "Parts per partition above 150 (of the 3000 insert hard-reject) for 15 min — the merge backlog is climbing toward the cliff. ACTION: (1) check writer CPU (merges are the CPU-bound pole; the writers step policy scales out if the writer-cpu-high alarm co-fires); (2) check DiskEffectiveFreeGB in ClickHouse/SelfHeal — merges stall near the keep_free_space floor; (3) the residue case: after a 30TB load test, drop the test partitions (ALTER TABLE ... DROP PARTITION) instead of paying the merge."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Keeper health alarm (keeper_up metric pushed by cron via cloudwatch agent)
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "keeper_down" {
  count = var.enabled ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-keeper-down"
  namespace           = "ClickHouse/Keeper"
  metric_name         = "keeper_up"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = 0
  comparison_operator = "LessThanOrEqualToThreshold"

  # The keeper-health cron pushes keeper_up with a Cluster dimension
  # (derived from the hostname); the alarm must match it or the metric
  # never resolves and the alarm stays INSUFFICIENT_DATA forever.
  dimensions = {
    Cluster = var.cluster_name
  }

  # Self-healing observability contract: a keeper that stops PUSHING is
  # indistinguishable from a keeper that is DOWN (the probe runs on the
  # keeper itself — a dead keeper pushes nothing). "missing" here is
  # exactly how the alarm sat blind for days: the push died (the chinit
  # cutover dropped it) and the alarm treated silence as health.
  treat_missing_data = "breaching"

  alarm_description = "At least one Keeper node is not responding to mntr (or stopped reporting entirely — the probe runs ON the keeper, so silence IS downtime). With 3 keepers the cluster tolerates 1 down; a second failure flips tables to read-only. Runbook: replace the keeper node (restore quorum, then the node)."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# ClickHouse-level alarms (Altinity operator alert-catalog parity)
# Node cpu/mem/disk can't see INSIDE ClickHouse.
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "ch_readonly_replica" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-readonly-replica"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseMetrics_ReadonlyReplica"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name

  }
  alarm_description = "A replicated table flipped read-only (ClickHouseMetrics_ReadonlyReplica > 0). This means Keeper lost quorum or the node lost its Keeper session - writes to that table fail. Runbook: verify 2 of 3 keepers answer mntr. Parity: Altinity alert ClickHouseReadonlyReplica."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ch_replica_delay" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-replica-delay-high"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseAsyncMetrics_ReplicasMaxAbsoluteDelay"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 3
  threshold           = var.ch_alert_thresholds.replica_max_delay_seconds
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name

  }
  alarm_description = "Replication queue absolute delay above ${var.ch_alert_thresholds.replica_max_delay_seconds}s for 15 minutes - a replica is too far behind (or its peer is unreachable). Readers serve increasingly stale data. Check system.replication_queue on the affected node. Parity: Altinity alert ClickHouseReplicasMaxAbsoluteDelay."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ch_distributed_backlog" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-distributed-backlog"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseMetrics_DistributedFilesToInsert"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 3
  threshold           = var.ch_alert_thresholds.distributed_files_to_insert
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name

  }
  alarm_description = "DistributedFilesToInsert above ${var.ch_alert_thresholds.distributed_files_to_insert} - the async insert pipeline is backing up (peer down or distributed_directory_monitor throttling). Inserts via Distributed tables lag. Parity: Altinity alert ClickHouseDistributedFilesToInsertHigh."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ch_parts_per_partition" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-parts-per-partition-high"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseAsyncMetrics_MaxPartCountForPartition"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 3
  threshold           = var.ch_alert_thresholds.max_parts_per_partition
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name

  }
  alarm_description = "MaxPartCountForPartition above ${var.ch_alert_thresholds.max_parts_per_partition} - too many parts in one partition (merge thread starvation or ingest pattern change). ClickHouse slows down sharply and may refuse inserts past max_parts_in_total. Parity: Altinity alert ClickHouseMaxPartCountForPartition."

  # Parts/merge backlog is writer work; step writers up (+1) when wired.
  alarm_actions = compact(concat(local.alert_topic_arns, [var.writers_step_up_policy_arn]))

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ch_s3queue_lag" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-s3queue-lag"
  comparison_operator = "GreaterThanThreshold"
  evaluation_periods  = 6
  threshold           = var.ch_alert_thresholds.s3queue_lag_seconds
  treat_missing_data  = "notBreaching"

  # Multi-node MAX across {ClusterName} over 300s (6 x 5m = 30m) avoids gaps during single-node refreshes.
  metric_query {
    id          = "lag_seconds"
    expression  = "seen - committed"
    label       = "newest-seen minus newest-committed (seconds of drain lag)"
    return_data = true
  }

  metric_query {
    id = "seen"
    metric {
      metric_name = local.s3queue_seen_metric
      namespace   = "ClickHouse/Server"
      period      = 300
      stat        = "Maximum"
      dimensions = {
        ClusterName = var.cluster_name
      }
    }
  }

  metric_query {
    id = "committed"
    metric {
      metric_name = local.s3queue_committed_metric
      namespace   = "ClickHouse/Server"
      period      = 300
      stat        = "Maximum"
      dimensions = {
        ClusterName = var.cluster_name
      }
    }
  }


  alarm_description = "S3Queue drain lag above ${var.ch_alert_thresholds.s3queue_lag_seconds}s for 30 min — drain has fallen behind ingest (bounded-lag breach). ACTION: (1) check the writers ASG is at writer_min and scale out if the fleet is undersized (autoscaling_config.writer_max is the ceiling); (2) verify producer files are ≥100 MB (small files cap throughput 12×); (3) check merge debt (ch-parts-per-partition-high co-firing means writers are merge-bound, not queue-bound); (4) if lag keeps growing after 30 min, page — the staging TTL (3d) is the last backstop before data loss."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# Why "effective": ClickHouse's keep_free_space reservation makes df LIE about headroom.
resource "aws_cloudwatch_metric_alarm" "ch_disk_effective_low" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name         = "${local.alarm_prefix}-ch-disk-effective-low"
  namespace          = "ClickHouse/Server"
  metric_name        = "ClickHouseAsyncMetrics_DiskAvailable"
  statistic          = "Minimum"
  period             = 300
  evaluation_periods = 2
  # 16 GiB effective: the disk-autoscale job heals at 8 GiB (floor),
  # this alarm at 16 GiB means the job has FAILED or is disabled and
  # the operator must intervene — two thresholds, one escalation ladder.
  threshold           = var.ch_alert_thresholds.disk_effective_low_bytes
  comparison_operator = "LessThanThreshold"

  # The EMF disk dimension set includes "disk" (the scrape config
  # declares it); keying on the default data disk only keeps the
  # S3-backed disks (infinite free space) out of the alarm math.
  dimensions = {
    ClusterName = var.cluster_name
    disk        = "default"
  }

  alarm_description = "Effective free space on the default data disk below ${var.ch_alert_thresholds.disk_effective_low_bytes} bytes (the keep_free_space-aware number ClickHouse refuses writes at, NOT df). Merges stall on NOT_ENOUGH_SPACE at this level and parts pile up toward the insert hard-reject. The chinit disk-autoscale timer should have grown the volume already (it heals at its own lower floor) — if this alarm fires, the autoscale job is failing or absent: check `systemctl status chinit-disk-autoscale.timer` on the flagged node and the ClickHouse/SelfHeal metrics."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "ch_running_queries" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-running-queries-high"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseMetrics_Query"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 3
  threshold           = var.ch_alert_thresholds.max_running_queries
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name

  }
  alarm_description = "RunningQueries above ${var.ch_alert_thresholds.max_running_queries} sustained 15 minutes - query load is saturating the nodes. Raise reader_count (horizontal) or drop heavy query concurrency at the client. Parity: Altinity alert ClickHouseTooManyRunningQueries."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# Restart detection (Altinity ClickHouseServerRestartRecently): a node that
# rebooted recently may be crash-looping; one reboot is normal (deploy),
# repeated reboots need investigation.
resource "aws_cloudwatch_metric_alarm" "ch_recent_restart" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-ch-recent-restart"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseAsyncMetrics_Uptime"
  statistic           = "Minimum"
  period              = 60
  evaluation_periods  = 2
  threshold           = var.ch_alert_thresholds.min_uptime_seconds
  comparison_operator = "LessThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name

  }
  alarm_description = "A ClickHouse node reports uptime < ${var.ch_alert_thresholds.min_uptime_seconds}s - it restarted in the last window. If this is not a deployment, check /var/log/clickhouse-server/clickhouse-server.log for OOM/crash (paradoxically: the metric disappears while the node is down - treat_missing_data breaching covers full crashes). Parity: Altinity alert ClickHouseServerRestartRecently."

  # A crashed node stops emitting Uptime - missing data = the alarm fires.
  treat_missing_data = "breaching"
  alarm_actions      = local.alert_topic_arns

  tags = var.tags
}

# Keeper deep-health alarms (Altinity prometheus-alert-rules-chkeeper.yaml):
# the binary keeper_up can't distinguish a struggling ensemble from a healthy
# one - these catch the failure modes BEFORE quorum is lost.
resource "aws_cloudwatch_metric_alarm" "keeper_max_latency" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-keeper-max-latency"
  namespace           = "ClickHouse/Keeper"
  metric_name         = "ClickHouseAsyncMetrics_KeeperMaxLatency"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = var.ch_alert_thresholds.keeper_max_latency_ms
  comparison_operator = "GreaterThanThreshold"

  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "keeper"
  }

  alarm_description = "Keeper max latency above ${var.ch_alert_thresholds.keeper_max_latency_ms}ms for 10 minutes. Raft is struggling (disk fsync or network). Replication write path slows; fix before quorum is lost. Check keeper_disk IO on the offenders. Parity: Altinity alert ClickHouseKeeperHighLatency."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "keeper_outstanding_requests" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-keeper-outstanding-requests"
  namespace           = "ClickHouse/Keeper"
  metric_name         = "ClickHouseMetrics_KeeperOutstandingRequests"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = var.ch_alert_thresholds.keeper_max_outstanding_requests
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "keeper"
  }
  alarm_description = "Keeper outstanding requests above ${var.ch_alert_thresholds.keeper_max_outstanding_requests} - request queue is building (CPU/disk saturation or a client hammering it). Parity: Altinity alert ClickHouseKeeperOutstandingRequests."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# S3Queue's processed-file tracking dominates the znode tree — the 2-h soak at ~198k rec/s drove zk_znode_count to 390k+ (5 writers x ~100k tracked…
resource "aws_cloudwatch_metric_alarm" "keeper_znode_count" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-keeper-znode-count"
  namespace           = "ClickHouse/Keeper"
  metric_name         = "ClickHouseAsyncMetrics_KeeperZnodeCount"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 3
  threshold           = var.ch_alert_thresholds.keeper_znode_count
  comparison_operator = "GreaterThanThreshold"

  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "keeper"
  }

  alarm_description = "Keeper znode count above ${var.ch_alert_thresholds.keeper_znode_count} for 15 min. The keeper's znode tree (S3Queue tracked files + replication metadata) is beyond its operating budget — snapshot/WAL replay and election times grow with tree size. ACTION: (1) verify the S3Queue tracked_files_limit in bootstrap-ddl.sql (the per-writer znode budget) and the tracked_file_ttl_sec (2d) vs the staging TTL; (2) check for a re-claim storm (a rebuilt queue re-adds the whole window); (3) a long-lived breach means the keeper needs the vertical ladder (keeper_type_ladder) before quorum recovery slows."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "keeper_snapshot_failures" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-keeper-snapshot-failures"
  namespace           = "ClickHouse/Keeper"
  metric_name         = "ClickHouseProfileEvents_KeeperSnapshotCreationsFailed"
  statistic           = "Sum"
  period              = 600
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "keeper"
  }
  alarm_description = "Keeper snapshot creation failed in the last 10 minutes - the snapshot disk may be full or the keeper_disk EBS degraded. Raft can still run from logs, but recovery time (and the 25GB log bound) grows. Parity: Altinity alert ClickHouseKeeperSnapshotCreationsFailed."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# KeeperLeaderSynced: followers emit 0 in KeeperAsynchronousMetrics.cpp, so
# Maximum across {ClusterName, NodeType="keeper"} evaluates the active leader's
# synced-follower count (2 when healthy, 1 with 1 follower down, 0 on quorum loss).
resource "aws_cloudwatch_metric_alarm" "keeper_lost_quorum" {
  count = var.enabled && var.enable_ch_alerts && var.keeper_count >= 2 ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-keeper-leader-unsynced"
  namespace           = "ClickHouse/Keeper"
  metric_name         = "ClickHouseAsyncMetrics_KeeperSyncedFollowers"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = var.ch_alert_thresholds.keeper_min_synced_followers
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"

  # EMF metrics in ClickHouse/Keeper carry {ClusterName, NodeType} dimensions
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "keeper"
  }

  alarm_description = "Keeper leader reports fewer than ${var.ch_alert_thresholds.keeper_min_synced_followers} synced followers for 3 minutes (evaluated via Maximum across {ClusterName, NodeType=keeper} since followers always emit 0 in KeeperAsynchronousMetrics.cpp) - quorum is about to be lost (or already is). Tables flip read-only at this point. Runbook: emergency - quorum already lost. Parity: Altinity alert ClickHouseKeeperLostQuorum."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "keeper_fd_usage" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name = "${local.alarm_prefix}-keeper-fd-usage-high"
  namespace  = "ClickHouse/Keeper"
  # EMF math not available for raw alarms; threshold on the open-fd metric
  # with a conservative absolute bound equivalent to the operator's 80% ratio
  # on a default 64k-fd system. Simple and robust without a metric-math pair.
  metric_name         = "ClickHouseAsyncMetrics_KeeperOpenFileDescriptorCount"
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = 52428 # 80% of a typical 65536 ulimit
  comparison_operator = "GreaterThanThreshold"


  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name
    NodeType    = "keeper"
  }
  alarm_description = "Keeper open file descriptors above the equivalent of ${var.ch_alert_thresholds.keeper_max_fd_ratio * 100}% of a default 64k limit - fd exhaustion crashes keeper hard. Investigate watch counts / connection leaks. Parity: Altinity alert ClickHouseKeeperHighFileDescriptorUsage."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Writers ASG step-scaling signal alarms (ingest pressure -> +1 / -1 writer)
# these alarms ARE the horizontal scaling path for writers — their alarm_actions invoke the ASG step-scaling policies from the compute module…
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "writer_insert_backpressure" {
  count = var.enabled && var.enable_ch_alerts ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-writer-insert-backpressure"
  namespace           = "ClickHouse/Server"
  metric_name         = "ClickHouseMetrics_DelayedInserts"
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 5
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  # EMF metrics carry ClusterName/NodeType labels from the scrape config
  dimensions = {
    ClusterName = var.cluster_name
  }

  alarm_description = "Inserts are being DELAYED on the writers (DelayedInserts > 0 for 5 minutes): ingest volume exceeds what the fleet can merge. ACTION: the writers ASG step-scaling policy adds one writer (+1). If this persists at writers ASG max capacity (see the writers-asg-cpu-high alarm), raise autoscaling_config.writer_max in tfvars. Parity: Altinity alert ClickHouseDelayedInsertThrottling."

  # Step-scale UP (+1 writer) on ALARM state; null ARN degrades to
  # SNS-only. SNS first — the notification and the scaling action are
  # independent consumers of the same alarm state.
  alarm_actions = compact(concat(local.alert_topic_arns, [var.writers_step_up_policy_arn]))
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

resource "aws_cloudwatch_metric_alarm" "writers_asg_idle" {
  count = var.enabled && var.autoscaling_enabled ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-writers-asg-idle"
  comparison_operator = "LessThanThreshold"
  evaluation_periods  = 60
  datapoints_to_alarm = 60
  threshold           = var.scale_down_cpu_threshold
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "idle_signal"
    expression  = "IF(FILL(RATE(m_ins), 0) > 100, 100, IF(FILL(DIFF(m_s3q), 0) > 0, 100, m_cpu))"
    label       = "Ingest-gated writers ASG CPU (100% while ClickHouse inserts or S3Queue commits are active)"
    return_data = true
  }

  metric_query {
    id = "m_cpu"
    metric {
      metric_name = "CPUUtilization"
      namespace   = "AWS/EC2"
      period      = 60
      stat        = "Average"
      dimensions = {
        AutoScalingGroupName = var.writers_asg_name
      }
    }
  }

  metric_query {
    id = "m_ins"
    metric {
      metric_name = "ClickHouseProfileEvents_InsertedRows"
      namespace   = "ClickHouse/Server"
      period      = 60
      stat        = "Sum"
      dimensions = {
        ClusterName = var.cluster_name
      }
    }
  }

  metric_query {
    id = "m_s3q"
    metric {
      metric_name = local.s3queue_committed_metric
      namespace   = "ClickHouse/Server"
      period      = 60
      stat        = "Maximum"
      dimensions = {
        ClusterName = var.cluster_name
      }
    }
  }

  alarm_description = "Writers ASG average CPU below ${var.scale_down_cpu_threshold}% for 60 consecutive 1-minute periods with no active ClickHouse inserts or S3Queue commits - the writers fleet is over-provisioned for the load. Any single 60-second minute with active ClickHouse inserts synthesizes 100% and flips the alarm from ALARM -> OK in <=60s. ACTION: the writers ASG StepScaling policy removes one writer (-1, bounded by autoscaling_config.writer_min; the scale-in lifecycle hook drains the terminated writer). Manual alternative: lower autoscaling_config.writer_min in tfvars."

  alarm_actions = compact(concat(local.alert_topic_arns, [var.writers_step_down_policy_arn]))
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# Per-host low-CPU alarms: guidance-only (SNS); writers-asg-idle owns scale-in.
resource "aws_cloudwatch_metric_alarm" "writer_cpu_low" {
  count = var.enabled && length(var.writer_hostnames) > 0 ? length(var.writer_hostnames) : 0

  alarm_name          = "${local.alarm_prefix}-writer-${count.index}-cpu-low"
  namespace           = "ClickHouse/Node"
  metric_name         = "cpu_usage_idle"
  statistic           = "Average"
  period              = 300
  evaluation_periods  = 72
  threshold           = 100 - var.scale_down_cpu_threshold
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    host = var.writer_hostnames[count.index]
    cpu  = "cpu-total"
  }

  alarm_description = "Writer CPU below ${var.scale_down_cpu_threshold}% for 6 hours - GUIDANCE ONLY (no scale action): a host-specific scale-in wedges the fleet (terminating this host orphans the alarm in ALARM state, which re-fires scale-down after every cooldown and kills each scale-out). The scale-in decision belongs to the aggregate ${local.alarm_prefix}-writers-asg-idle alarm, which survives host death."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Backup freshness alarm (ClickHouse/SelfHeal BackupSuccess pushed by chinit)
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "backup_missing" {
  count = var.enabled && var.enable_backup_alerts ? 1 : 0

  alarm_name         = "${local.alarm_prefix}-backup-missing"
  namespace          = "ClickHouse/SelfHeal"
  metric_name        = "BackupSuccess"
  statistic          = "Maximum"
  period             = 3600
  evaluation_periods = var.backup_missing_threshold_hours
  # Absence = the nightly timer never pushed — that IS the incident
  # (a dead timer, a crashed node, or a silent failure).
  treat_missing_data  = "breaching"
  threshold           = 0
  comparison_operator = "LessThanOrEqualToThreshold"

  dimensions = {
    ClusterName = var.cluster_name
    Host        = "backup-0"
  }

  alarm_description = "No backup-success signal for ${var.backup_missing_threshold_hours} hours — the nightly clickhouse-backup pass is not completing. Check clickhouse-backup-daily.timer on the backup replica (SSM Session Manager -> systemctl status clickhouse-backup-daily.timer; journalctl -u that unit for the chinit backup-daily verdict)."

  alarm_actions = local.alert_topic_arns

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Certificate expiry alarm (passthrough mode)
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "cert_expiry" {
  count = var.enabled && var.cert_expiration_date != null ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-tls-cert-expiring"
  namespace           = "ClickHouse/TLS"
  metric_name         = "days_to_expiry"
  statistic           = "Maximum"
  period              = 86400
  evaluation_periods  = 1
  threshold           = 30
  comparison_operator = "LessThanOrEqualToThreshold"

  alarm_description = "TLS certificate expires within 30 days (${var.cert_expiration_date}). Certificates re-issue on terraform apply (ACME module) — re-run apply during a maintenance window."

  alarm_actions = local.alert_topic_arns

  # The metric is pushed by the CloudWatch agent; missing datapoints breach.
  treat_missing_data = "notBreaching"

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Budgets (first 2 budgets per account are free)
# ----------------------------------------------------------------------------
resource "aws_budgets_budget" "monthly" {
  count = var.budget_enabled ? 1 : 0

  name         = "${var.cluster_name}-monthly"
  budget_type  = "COST"
  limit_amount = var.budget_monthly_amount
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  dynamic "notification" {
    for_each = var.budget_alert_thresholds
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value
      threshold_type             = "PERCENTAGE"
      notification_type          = "ACTUAL"
      subscriber_email_addresses = var.budget_alert_emails
    }
  }

  dynamic "cost_types" {
    for_each = [1]
    content {
      include_support  = true
      include_tax      = true
      include_discount = true
    }
  }

  tags = var.tags
}

# Forecast budget notification — catches overruns before they land
resource "aws_budgets_budget" "forecast" {
  count = var.budget_enabled && var.budget_forecast_threshold != null ? 1 : 0

  name         = "${var.cluster_name}-forecast"
  budget_type  = "COST"
  limit_amount = var.budget_monthly_amount
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  dynamic "notification" {
    for_each = [var.budget_forecast_threshold]
    content {
      comparison_operator        = "GREATER_THAN"
      threshold                  = notification.value
      threshold_type             = "PERCENTAGE"
      notification_type          = "FORECASTED"
      subscriber_email_addresses = var.budget_alert_emails
    }
  }

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Orphaned-resource alarm (the chinit orphaned-gc sweep's findings)
# churn debris (rebuilds, failed applies, half-completed rolls) accumulates silently — tens of TB of unattached volumes in DAYS before anyone…
# ----------------------------------------------------------------------------
resource "aws_cloudwatch_metric_alarm" "orphans_found" {
  count = var.enabled ? 1 : 0

  alarm_name          = "${local.alarm_prefix}-orphans-found"
  namespace           = "ClickHouse/LeakGuard"
  metric_name         = "OrphanVolumesFound"
  statistic           = "Maximum"
  period              = 86400
  evaluation_periods  = 1
  threshold           = 0
  comparison_operator = "GreaterThanThreshold"
  # The sweep runs daily; a quiet day is a clean day, not an incident.
  treat_missing_data = "notBreaching"

  alarm_description = "The daily orphan sweep found unowned resources (unattached untagged volumes past the age threshold; EIPs/snapshots reported only). The sweep already reclaimed the deletables - recurring findings mean some path is leaking resources again. Check the ordinal-0 writer's orphaned-gc journal and grep the modules for new inline ebs_block_device usage (docs/TROUBLESHOOTING.md - Orphaned data disks)."

  alarm_actions = local.alert_topic_arns
  ok_actions    = local.alert_topic_arns

  tags = var.tags
}
