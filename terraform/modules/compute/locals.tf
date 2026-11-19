# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — Compute Module
# ============================================================================

locals {
  az     = var.subnet_az
  ami_id = coalesce(var.ami_id, try(data.aws_ami.ubuntu[0].id, null))

  # Static private IPs for keepers (.20..29) and backup (.30); writers/readers use lease identity.
  keeper_ips = [for i in range(var.keeper_count) : cidrhost(var.subnet_cidr, 20 + i)]
  backup_ip  = var.enable_backup_replica ? cidrhost(var.subnet_cidr, 30) : null

  reader_instance_family = regex("^[a-z0-9]+", var.reader_instance_type)
  reader_alt_families = {
    r8g  = ["r8g", "r7g"]
    r7g  = ["r7g", "r6g"]
    m8g  = ["m8g", "m7g"]
    r8gd = ["r8gd", "r7gd"]
    r7gd = ["r7gd", "r6gd"]
    m8gd = ["m8gd", "m7gd"]
    c8gd = ["c8gd", "c7gd"]
    t4g  = ["t4g"]
  }

  # Keep reader_pool_weights null unless weighted_capacity is explicitly set so 1 ASG unit = 1 instance.
  reader_pool_types = var.reader_instance_type_pool != null ? (
    [for e in var.reader_instance_type_pool : e.instance_type]
  ) : local.reader_instance_types
  reader_pool_has_weights = var.reader_instance_type_pool != null && anytrue([
    for e in var.reader_instance_type_pool : e.weighted_capacity != null
  ])
  reader_pool_weights = local.reader_pool_has_weights ? (
    [for e in var.reader_instance_type_pool : coalesce(e.weighted_capacity, 1)]
  ) : null
  reader_families = distinct(concat(
    [local.reader_instance_family],
    lookup(local.reader_alt_families, local.reader_instance_family, [local.reader_instance_family])
  ))
  reader_instance_size  = replace(var.reader_instance_type, "/^${local.reader_instance_family}\\./", "")
  reader_instance_types = [for family in local.reader_families : "${family}.${local.reader_instance_size}"]

  # Writer mixed-instances pool (opt-in; null = single writer_instance_type).
  # Mirrors the reader pool semantics. Weights stay null unless explicit so 1 ASG unit = 1 writer.
  writer_pool_types = var.writer_instance_type_pool != null ? (
    [for e in var.writer_instance_type_pool : e.instance_type]
  ) : null
  writer_pool_has_weights = var.writer_instance_type_pool != null && anytrue([
    for e in var.writer_instance_type_pool : e.weighted_capacity != null
  ])
  writer_pool_weights = local.writer_pool_has_weights ? (
    [for e in var.writer_instance_type_pool : coalesce(e.weighted_capacity, 1)]
  ) : null
  writer_spot_enabled = var.writer_spot_ratio > 0 && local.writer_pool_types != null

  # Desired-configuration payload for the writers instance refresh. Mixed-instances
  # (spot) ASGs reject a bare LaunchTemplate refresh, so carry the override list —
  # nested under MixedInstancesPolicy.LaunchTemplate, the API-validated shape
  # (DesiredConfiguration accepts only top-level keys LaunchTemplate|MixedInstancesPolicy).
  # Pre-encoded as two JSON strings so the selector branches stay string-typed.
  writer_refresh_config_lt = jsonencode({ LaunchTemplate = { LaunchTemplateId = aws_launch_template.writer.id, Version = tostring(aws_launch_template.writer.latest_version) } })
  writer_refresh_config_mixed = jsonencode({
    MixedInstancesPolicy = {
      LaunchTemplate = {
        LaunchTemplateSpecification = { LaunchTemplateId = aws_launch_template.writer.id, Version = tostring(aws_launch_template.writer.latest_version) },
        Overrides                   = [for t in local.writer_pool_types : { InstanceType = t }],
      }
    }
  })
  writer_refresh_config = local.writer_spot_enabled ? local.writer_refresh_config_mixed : local.writer_refresh_config_lt


  # "unlimited" CPU credits only apply to burstable t-family instances.
  credits_by_family = {
    t4g = "unlimited"
    t3  = "unlimited"
  }
  reader_cpu_credits = lookup(local.credits_by_family, local.reader_instance_family, "standard")
  writer_cpu_credits = lookup(local.credits_by_family, regex("^[a-z0-9]+", var.writer_instance_type), "standard")

  # acm mode terminates TLS at the NLB; passthrough terminates TLS on ClickHouse.
  listener_protocol    = var.nlb_tls_mode == "acm" ? "TLS" : "TCP"
  listener_port_http   = var.nlb_tls_mode == "acm" ? 8443 : 8123
  listener_port_native = var.nlb_tls_mode == "acm" ? 9440 : 9000
  target_port_http     = 8123
  target_port_native   = 9000

  nlb_subnet_ids     = var.nlb_subnet_ids != null ? var.nlb_subnet_ids : (var.public_subnet_id != null ? [var.public_subnet_id] : [])
  nlb_name_key       = var.nlb_scheme == "internal" ? "${var.vpc_id}-internal" : var.vpc_id
  asg_subnet_ids     = var.asg_subnet_ids != null ? var.asg_subnet_ids : [var.subnet_id]
  writers_subnet_ids = coalesce(var.writers_asg_subnet_ids, var.asg_subnet_ids, [var.subnet_id])
  readers_subnet_ids = coalesce(var.readers_asg_subnet_ids, var.asg_subnet_ids, [var.subnet_id])

  reader_has_nvme = alltrue([for t in local.reader_pool_types : can(regex("^([a-z]+[0-9]+gd|i[0-9]+g)[.]", t))])
  backup_has_nvme = can(regex("^([a-z]+[0-9]+gd|i[0-9]+g)[.]", var.backup_instance_type))
}
