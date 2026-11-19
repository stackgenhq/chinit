# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Monitoring Module Outputs
# ============================================================================

output "log_group_arns" {
  description = "CloudWatch Logs group ARNs the node agents ship to (server/error/keeper/bootstrap/prometheus)"
  value       = { for k, g in aws_cloudwatch_log_group.node_logs : k => g.arn }
}

output "log_group_names" {
  description = "CloudWatch Logs group names the node agents ship to"
  value       = { for k, g in aws_cloudwatch_log_group.node_logs : k => g.name }
}

output "alert_topic_arn" {
  description = "SNS topic ARN for ClickHouse alarms (always created - the convergence/rescale signal channel; human subscriptions are optional)"
  value       = aws_sns_topic.alerts.arn
}

output "alarm_names" {
  description = "All created alarm names"
  value = compact(concat(
    [for a in aws_cloudwatch_metric_alarm.writer_cpu : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writer_memory : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writer_disk : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.readers_asg_cpu : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writers_asg_cpu : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.readers_asg_idle : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writers_lb_unhealthy : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.readers_lb_unhealthy : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writers_lb_latency : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.keeper_down : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.ch_readonly_replica : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.ch_replica_delay : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.ch_distributed_backlog : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.ch_parts_per_partition : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.ch_running_queries : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.ch_recent_restart : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.keeper_max_latency : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.keeper_outstanding_requests : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.keeper_snapshot_failures : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.keeper_lost_quorum : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.keeper_fd_usage : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writer_insert_backpressure : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.writer_cpu_low : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.backup_missing : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.cert_expiry : a.alarm_name],
    [for a in aws_cloudwatch_metric_alarm.log_pattern : a.alarm_name],
  ))
}

output "log_metric_filters" {
  description = "Log-pattern metric filter names -> emitted metric names"
  value       = { for k, f in aws_cloudwatch_log_metric_filter.patterns : k => f.metric_transformation[0].name }
}
