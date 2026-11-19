# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Compute Module Outputs
# ============================================================================

output "keeper_ips" {
  description = "Static private IPs of keeper nodes"
  value       = aws_instance.keeper[*].private_ip
}

output "backup_ip" {
  description = "Static private IP of the backup replica (null in dev)"
  value       = var.enable_backup_replica ? aws_instance.backup[0].private_ip : null
}

output "writers_lb_dns_name" {
  description = "Writers NLB DNS name"
  value       = var.enable_nlbs ? aws_lb.writers[0].dns_name : null
}

output "writers_lb_zone_id" {
  description = "Writers NLB hosted zone id"
  value       = var.enable_nlbs ? aws_lb.writers[0].zone_id : null
}

output "readers_lb_dns_name" {
  description = "Readers NLB DNS name"
  value       = var.enable_nlbs ? aws_lb.readers[0].dns_name : null
}

output "readers_lb_zone_id" {
  description = "Readers NLB hosted zone id"
  value       = var.enable_nlbs ? aws_lb.readers[0].zone_id : null
}

output "writers_lb_arn" {
  description = "Writers NLB ARN (for monitoring)"
  value       = var.enable_nlbs ? aws_lb.writers[0].arn : null
}

output "readers_lb_arn" {
  description = "Readers NLB ARN (for monitoring)"
  value       = var.enable_nlbs ? aws_lb.readers[0].arn : null
}

# Target-group ARN suffixes ("targetgroup/<name>/<id>") for the
# per-target-group CloudWatch alarms. NetworkELB publishes
# UnHealthyHostCount/TargetResponseTime under LoadBalancer+TargetGroup
# pairs — an alarm with only the LoadBalancer dimension sat
# INSUFFICIENT_DATA forever (a past audit found all four LB
# alarms blind since creation for exactly this reason).
output "writers_http_tg_arn_suffix" {
  description = "Writers HTTP target-group ARN suffix (for monitoring)"
  value       = var.enable_nlbs ? split("targetgroup/", aws_lb_target_group.writers_http[0].arn)[1] : null
}

output "readers_http_tg_arn_suffix" {
  description = "Readers HTTP target-group ARN suffix (for monitoring)"
  value       = var.enable_nlbs ? split("targetgroup/", aws_lb_target_group.readers_http[0].arn)[1] : null
}

output "readers_asg_name" {
  description = "Readers ASG name (for monitoring/scaling alarms)"
  value       = aws_autoscaling_group.readers.name
}

output "writers_asg_name" {
  description = "Writers ASG name (consumed by root for scaling policies and the scale-in lifecycle hook)"
  value       = aws_autoscaling_group.writers.name
}

output "writers_step_up_policy_arn" {
  description = "Step-scaling scale-UP policy ARN on the writers ASG (+1 writer, SimpleScaling). Consumed by modules/monitoring as an alarm_action on writer-cpu-high / writer-insert-backpressure, so those alarms add ingest capacity instead of merely notifying."
  value       = aws_autoscaling_policy.writers_step_up.arn
}

output "readers_step_up_policy_arn" {
  description = "Step-scaling scale-UP policy ARN on the readers ASG (+1 reader, SimpleScaling, cooldown 300). Consumed by modules/monitoring as an alarm_action on the parts-per-partition alarm — the queue/backlog scaling signal the 30TB load test identified (replication catch-up and merge backlog are background work that CW CPU-average dilutes, so target-tracking alone under-scales)."
  value       = aws_autoscaling_policy.readers_step_up.arn
}

output "writers_step_down_policy_arn" {
  description = "Step-scaling scale-DOWN policy ARN on the writers ASG (-1 writer, cooldown 600, bounded by writer_min). Consumed by modules/monitoring as an alarm_action on writer-cpu-low (sustained over-provisioning)."
  value       = aws_autoscaling_policy.writers_step_down.arn
}

output "keeper_instance_ids" {
  description = "Keeper EC2 instance ids"
  value       = aws_instance.keeper[*].id
}

# The keeper's Name tag (the autoscale coordinator's live identity
# seam): the instance ids go stale on replacement applies — the tag
# is the stable key (the crash-loop lesson).
output "keeper_name_tag" {
  description = "The keeper EC2's Name tag (live tag-lookup identity)"
  value       = "${var.cluster_name}-keeper"
}

output "backup_instance_id" {
  description = "Backup replica EC2 instance id (null in dev)"
  value       = var.enable_backup_replica ? aws_instance.backup[0].id : null
}

output "reader_launch_template_id" {
  description = "Reader launch template id"
  value       = aws_launch_template.reader.id
}

output "instance_ids_all" {
  description = "All statically-known EC2 instance ids (for SSM commands and monitoring). Writers/readers are ASG members - their instance ids resolve dynamically via tags (ASG lifecycle), so only keepers and the backup replica appear here."
  value = compact(concat(
    aws_instance.keeper[*].id,
    var.enable_backup_replica ? aws_instance.backup[*].id : []
  ))
}

output "placement_group_name" {
  description = "Placement group name (cluster mode)"
  value       = var.placement_strategy == "cluster" ? aws_placement_group.cluster[0].name : null
}

output "writers_launch_template_id" {
  description = "The writers ASG launch template id - the chinit autoscale coordinator (the ordinal-0 writer's timer) versions it on a vertical step (spec-rendered for the bundle upload's dependency)"
  value       = aws_launch_template.writer.id
}
