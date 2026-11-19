# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Compute Module — EC2 nodes, ASG (writers/readers/keepers/backup), NLBs
# ============================================================================
# Why: Provides the compute layer: writers + keepers as fixed-identity EC2
#      with static private IPs (replication paths and raft quorums need stable
#      identity), readers as a Spot/On-Demand mixed ASG (configurable split),
#      and a dedicated backup replica. Two Network Load Balancers front
#      writers and readers.
# Without it: No compute exists for ClickHouse. The database cannot run,
#             applications cannot connect, and queries fail.
# ============================================================================

# ----------------------------------------------------------------------------
# Data sources
# ----------------------------------------------------------------------------
data "aws_ami" "ubuntu" {
  count = var.ami_id == null ? 1 : 0

  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name = "name"
    # Standard Ubuntu Server 24.04 LTS arm64 (Graviton). NOT "ubuntu-pro-*" —
    # the plain server image under ubuntu/images/hvm-ssd-gp3/ is what exists.
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# ----------------------------------------------------------------------------
# Placement group — colocation for low latency
# cluster placement packs instances into one low-latency group with sub-millisecond RTT and full bisection bandwidth.
# ----------------------------------------------------------------------------
resource "aws_placement_group" "cluster" {
  count = var.placement_strategy == "cluster" ? 1 : 0

  name     = "${var.cluster_name}-clickhouse"
  strategy = "cluster"

  tags = var.tags
}

# ----------------------------------------------------------------------------
# Network Load Balancers
# NLB fronts writers and readers with static IPs and health checks.
# ----------------------------------------------------------------------------
resource "aws_lb" "writers" {
  # Name keys on the VPC id hash: a VPC-swap replacement must create the new
  # LB before destroying the old one, and ELB names are region-unique - a
  # static name would fail the create with "already exists" (same collision
  # the target groups hit). Stable while the VPC is stable, so no churn on
  # normal applies. NLB name max 32 chars - short hash keeps well under it.
  name               = substr("${var.cluster_name}-w-nlb-${substr(sha256(local.nlb_name_key), 0, 6)}", 0, 32)
  internal           = var.nlb_scheme == "internal"
  load_balancer_type = "network"

  dynamic "subnet_mapping" {
    for_each = local.nlb_subnet_ids
    content {
      subnet_id = subnet_mapping.value
    }
  }

  tags = merge(var.tags, { Name = "${var.cluster_name}-writers-nlb" })

  # VPC-swap replacement, NOT in-place SetSubnets: the provider's in-place
  # subnet update races two SetSubnets calls across the subnet swap and the
  # API rejects "You must specify a subnet ID for each subnet mapping" on a
  # partially-applied set. Forcing replacement gives one clean create with
  # the final subnets. The name carries the VPC hash so create precedes
  # destroy without an "already exists" collision (region-unique names).
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_lb" "readers" {
  # Same VPC-keyed naming + forced replacement as aws_lb.writers (see note).
  name               = substr("${var.cluster_name}-r-nlb-${substr(sha256(local.nlb_name_key), 0, 6)}", 0, 32)
  internal           = var.nlb_scheme == "internal"
  load_balancer_type = "network"

  dynamic "subnet_mapping" {
    for_each = local.nlb_subnet_ids
    content {
      subnet_id = subnet_mapping.value
    }
  }

  tags = merge(var.tags, { Name = "${var.cluster_name}-readers-nlb" })

  lifecycle {
    create_before_destroy = true
  }
}

# a booting writer is served the moment ClickHouse accepts the health-check TCP connection, no manual registration to race.
resource "aws_lb_target_group" "writers_http" {
  name        = substr("${var.cluster_name}-w-http-${substr(sha256("${var.vpc_id}instance"), 0, 6)}", 0, 32)
  port        = local.target_port_http
  protocol    = "TCP"
  target_type = "instance"
  vpc_id      = var.vpc_id

  # Internal NLB: preserve_client_ip must be false — pod clients would
  # otherwise hit the node with pod source IPs the node's SG cannot match.
  preserve_client_ip = var.nlb_scheme == "internal" ? false : true

  # Fast drain for zero-downtime rolls : the default
  # 300s deregistration delay keeps routing to a terminated writer for up
  # to 5 min while its health check already fails. 30s fails inserts over
  # fast; producers retry and the insert-dedup guarantees no duplicates.
  deregistration_delay = 30

  health_check {
    protocol            = "HTTP"
    port                = "8123"
    path                = "/replicas_status"
    matcher             = "200"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = var.tags
}

# Writers TG registers via the ASG (target_group_arns) -> instance type;
# a booting writer is served the moment ClickHouse accepts the health-check
# TCP connection, no manual registration to race. Same VPC-hash + type
# name keying as writers_http above (see its comment for the
# ip→instance flip collision the type key prevents).
resource "aws_lb_target_group" "writers_native" {
  name        = substr("${var.cluster_name}-w-native-${substr(sha256("${var.vpc_id}instance"), 0, 6)}", 0, 32)
  port        = local.target_port_native
  protocol    = "TCP"
  target_type = "instance"
  vpc_id      = var.vpc_id

  # See readers_http for why preserve_client_ip is off for internal NLBs.
  preserve_client_ip = var.nlb_scheme == "internal" ? false : true

  # Fast drain for zero-downtime rolls : the default
  # 300s deregistration delay keeps routing to a terminated writer for up
  # to 5 min while its health check already fails. 30s fails inserts over
  # fast; producers retry and the insert-dedup guarantees no duplicates.
  deregistration_delay = 30

  health_check {
    protocol            = "HTTP"
    port                = "8123"
    path                = "/replicas_status"
    matcher             = "200"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = var.tags
}

# Readers TG registers via the ASG (target_group_arns) -> instance type.
resource "aws_lb_target_group" "readers_http" {
  name        = substr("${var.cluster_name}-r-http-${substr(sha256(var.vpc_id), 0, 6)}", 0, 32)
  port        = local.target_port_http
  protocol    = "TCP"
  target_type = "instance"
  vpc_id      = var.vpc_id

  # Internal NLB: preserve_client_ip MUST be off. AWS preserves the client IP
  # for instance targets in the same VPC, but pod clients live on other
  # subnets routed via ENI trunking — the node's SG can't match pod CIDRs and
  # the node never sees the flow consistently (TLS established, then RSTs).
  # Disable it so the node only ever sees the NLB ENI's traffic (which is
  # allowed by the client CIDR rules that already list the NLB subnet).
  preserve_client_ip = var.nlb_scheme == "internal" ? false : true

  deregistration_delay = 30

  health_check {
    protocol            = "HTTP"
    port                = "8123"
    path                = "/replicas_status"
    matcher             = "200"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = var.tags
}

resource "aws_lb_target_group" "readers_native" {
  name        = substr("${var.cluster_name}-r-native-${substr(sha256(var.vpc_id), 0, 6)}", 0, 32)
  port        = local.target_port_native
  protocol    = "TCP"
  target_type = "instance"
  vpc_id      = var.vpc_id

  # See readers_http for why preserve_client_ip is off for internal NLBs.
  preserve_client_ip = var.nlb_scheme == "internal" ? false : true

  deregistration_delay = 30

  health_check {
    protocol            = "HTTP"
    port                = "8123"
    path                = "/replicas_status"
    matcher             = "200"
    interval            = 10
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = var.tags
}

resource "aws_lb_listener" "writers_http" {
  load_balancer_arn = aws_lb.writers.arn
  port              = local.listener_port_http
  protocol          = local.listener_protocol

  # acm mode: TLS termination at the NLB with the ACM cert
  certificate_arn = var.nlb_tls_mode == "acm" ? var.acm_certificate_arn : null

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.writers_http.arn
  }
}

resource "aws_lb_listener" "writers_native" {
  load_balancer_arn = aws_lb.writers.arn
  port              = local.listener_port_native
  protocol          = local.listener_protocol

  certificate_arn = var.nlb_tls_mode == "acm" ? var.acm_certificate_arn : null

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.writers_native.arn
  }
}

resource "aws_lb_listener" "readers_http" {
  load_balancer_arn = aws_lb.readers.arn
  port              = local.listener_port_http
  protocol          = local.listener_protocol

  certificate_arn = var.nlb_tls_mode == "acm" ? var.acm_certificate_arn : null

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.readers_http.arn
  }
}

resource "aws_lb_listener" "readers_native" {
  load_balancer_arn = aws_lb.readers.arn
  port              = local.listener_port_native
  protocol          = local.listener_protocol

  certificate_arn = var.nlb_tls_mode == "acm" ? var.acm_certificate_arn : null

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.readers_native.arn
  }
}

# ----------------------------------------------------------------------------
# Launch templates (IMDSv2 enforced, encrypted gp3, user-data stage 1)
# ----------------------------------------------------------------------------
resource "aws_launch_template" "reader" {
  name_prefix   = "${var.cluster_name}-reader-"
  image_id      = local.ami_id
  instance_type = var.reader_instance_type
  user_data     = base64encode(var.reader_user_data)

  monitoring {
    enabled = true
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  iam_instance_profile {
    name = var.reader_instance_profile_name
  }

  vpc_security_group_ids = [var.readers_sg_id]

  credit_specification {
    cpu_credits = local.reader_cpu_credits
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      encrypted             = true
      volume_size           = var.root_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  block_device_mappings {
    # Stateless reader metadata disk: re-syncs from peers on replacement.
    device_name = "/dev/sdb"
    ebs {
      encrypted             = true
      volume_size           = var.metadata_disk_size_gb
      volume_type           = "gp3"
      iops                  = var.metadata_disk_iops
      throughput            = var.metadata_disk_throughput_mbps
      delete_on_termination = true
    }
  }

  dynamic "block_device_mappings" {
    for_each = ((var.cache_on_nvme_instance_store && local.reader_has_nvme) || var.reader_cache_on_nvme_instance_store) ? [] : [1]
    content {
      device_name = "/dev/sdc"
      ebs {
        encrypted             = true
        volume_size           = var.cache_disk_size_gb
        volume_type           = "gp3"
        delete_on_termination = true
      }
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(var.tags, { Name = "${var.cluster_name}-reader", NodeType = "reader" })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(var.tags, { Name = "${var.cluster_name}-reader", NodeType = "reader" })
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_launch_template" "writer" {
  name_prefix   = "${var.cluster_name}-writer-"
  image_id      = local.ami_id
  instance_type = var.writer_instance_type
  user_data     = base64encode(var.writer_user_data)

  monitoring {
    enabled = true
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  iam_instance_profile {
    name = var.writer_instance_profile_name
  }

  vpc_security_group_ids = [var.writers_sg_id]

  credit_specification {
    cpu_credits = local.writer_cpu_credits
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      encrypted             = true
      volume_size           = var.root_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  block_device_mappings {
    # Stateless writer metadata disk: parts live in S3 under per-replica prefixes;
    # replacement writers rebuild their catalog via replication without orphaning EBS volumes.
    device_name = "/dev/sdb"
    ebs {
      encrypted             = true
      volume_size           = var.metadata_disk_size_gb
      volume_type           = "gp3"
      iops                  = var.metadata_disk_iops
      throughput            = var.metadata_disk_throughput_mbps
      delete_on_termination = true
    }
  }

  dynamic "block_device_mappings" {
    for_each = var.cache_on_nvme_instance_store ? [] : [1]
    content {
      device_name = "/dev/sdc"
      ebs {
        encrypted             = true
        volume_size           = var.cache_disk_size_gb
        volume_type           = "gp3"
        delete_on_termination = true
      }
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(var.tags, { Name = "${var.cluster_name}-writer", NodeType = "writer" })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(var.tags, { Name = "${var.cluster_name}-writer", NodeType = "writer" })
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
    # Track var.writer_instance_type here; aws_autoscaling_group.writers ignores
    # launch_template between applies so runtime vertical-autoscaler steps are preserved.
  }
}

resource "aws_launch_template" "keeper" {
  name_prefix   = "${var.cluster_name}-keeper-"
  image_id      = local.ami_id
  instance_type = var.keeper_instance_type
  user_data     = base64encode(var.keeper_user_data)

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  iam_instance_profile {
    name = var.keeper_instance_profile_name
  }

  vpc_security_group_ids = [var.keepers_sg_id]

  # Burstable credits: UNLIMITED on the t-family (raft fsync latency
  # must never hit the credit-exhaustion throttle; keeper CPU pressure
  # degrades election latency fleet-wide); "standard" on non-burstable
  # families (unlimited fails their launches).
  credit_specification {
    cpu_credits = lookup(local.credits_by_family, regex("^[a-z0-9]+", var.keeper_instance_type), "standard")
  }

  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      encrypted             = true
      volume_size           = var.root_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  block_device_mappings {
    # Keeper raft log/snapshot disk - dedicated for fsync latency isolation.
    # delete_on_termination=true: this template is
    # currently UNUSED (keeper is a fixed aws_instance with a standalone
    # aws_ebs_volume.keeper_raft), but if an ASG-based keeper is ever built
    # from this template, persistent inline volumes would ORPHAN ~64 GiB per
    # churn event (inline volumes get no tags and survive termination).
    device_name = "/dev/sdb"
    ebs {
      encrypted             = true
      volume_size           = var.keeper_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(var.tags, { Name = "${var.cluster_name}-keeper", NodeType = "keeper" })
  }

  # Tag the EBS volumes the template creates: untagged leftovers are
  # invisible in cost dashboards and unidentifiable in cleanup sweeps
  # (untagged orphans can pile up unnoticed this way).
  tag_specifications {
    resource_type = "volume"
    tags          = merge(var.tags, { Name = "${var.cluster_name}-keeper", NodeType = "keeper" })
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

# ----------------------------------------------------------------------------
# Readers ASG — mixed Spot/On-Demand with configurable split
# ----------------------------------------------------------------------------
resource "aws_autoscaling_group" "readers" {
  name                = "${var.cluster_name}-readers"
  desired_capacity    = var.reader_count
  min_size            = min(var.reader_count, var.autoscaling_min)
  max_size            = max(var.reader_count, var.autoscaling_max)
  vpc_zone_identifier = local.readers_subnet_ids

  mixed_instances_policy {
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.reader.id
        version            = aws_launch_template.reader.latest_version
      }

      dynamic "override" {
        for_each = { for idx, t in local.reader_pool_types : idx => {
          type   = t
          weight = local.reader_pool_weights != null ? local.reader_pool_weights[idx] : null
        } }
        content {
          instance_type     = override.value.type
          weighted_capacity = override.value.weight
        }
      }
    }

    instances_distribution {
      on_demand_allocation_strategy            = "prioritized"
      on_demand_base_capacity                  = var.reader_spot_on_demand_base_capacity
      on_demand_percentage_above_base_capacity = 100 - floor(var.reader_spot_ratio * 100)
      spot_allocation_strategy                 = "capacity-optimized"
    }
  }

  instance_refresh {
    strategy = "Rolling"
    preferences {
      min_healthy_percentage = 100 - var.max_unhealthy_percent
      instance_warmup        = var.refresh_instance_warmup_seconds
      auto_rollback          = true
    }
  }

  target_group_arns = [
    aws_lb_target_group.readers_http.arn,
    aws_lb_target_group.readers_native.arn,
  ]

  health_check_grace_period = 300
  health_check_type         = "EC2"
  placement_group           = var.placement_strategy == "cluster" ? aws_placement_group.cluster[0].name : null

  tag {
    key                 = "Name"
    value               = "${var.cluster_name}-reader"
    propagate_at_launch = true
  }
  dynamic "tag" {
    for_each = var.tags
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }

  lifecycle {
    ignore_changes = [desired_capacity]
  }
}

# ----------------------------------------------------------------------------
# Writers ASG — horizontal ingest scaling; on-demand by default, optional Spot pool (writer_spot_ratio)
# ----------------------------------------------------------------------------
resource "aws_autoscaling_group" "writers" {
  name                = "${var.cluster_name}-writers"
  desired_capacity    = var.writer_count
  min_size            = var.writer_min
  max_size            = var.writer_max
  vpc_zone_identifier = local.writers_subnet_ids

  dynamic "launch_template" {
    for_each = local.writer_spot_enabled ? [] : [1]
    content {
      id      = aws_launch_template.writer.id
      version = aws_launch_template.writer.latest_version
    }
  }

  dynamic "mixed_instances_policy" {
    for_each = local.writer_spot_enabled ? [1] : []
    content {
      launch_template {
        launch_template_specification {
          launch_template_id = aws_launch_template.writer.id
          version            = aws_launch_template.writer.latest_version
        }
        dynamic "override" {
          for_each = { for idx, t in local.writer_pool_types : idx => {
            type   = t
            weight = local.writer_pool_weights != null ? local.writer_pool_weights[idx] : null
          } }
          content {
            instance_type     = override.value.type
            weighted_capacity = override.value.weight
          }
        }
      }

      instances_distribution {
        on_demand_allocation_strategy = "prioritized"
        # Keep >= 1 On-Demand slot for writer-0 (backup-wake & maintenance coordinator);
        # writer-1..N above this base capacity use the Spot ratio.
        on_demand_base_capacity                  = var.writer_spot_on_demand_base_capacity
        on_demand_percentage_above_base_capacity = 100 - floor(var.writer_spot_ratio * 100)
        spot_allocation_strategy                 = "capacity-optimized"
      }
    }
  }

  # No `instance_refresh` block here: with a mixed_instances_policy the provider's
  # post-update refresh call sends a bare LaunchTemplate desired-configuration, which
  # AWS rejects ("Valid requests must contain either LaunchTemplate or
  # MixedInstancesPolicy"). The writers_ami_roll provisioner starts refreshes with the
  # full mixed policy instead (see modules/compute/locals.tf writer_refresh_config).
  # ASG-side refresh preferences are still configured via writers_ami_roll's --preferences.

  target_group_arns = [
    aws_lb_target_group.writers_http.arn,
    aws_lb_target_group.writers_native.arn,
  ]

  health_check_grace_period = 300
  health_check_type         = "EC2"
  placement_group           = var.placement_strategy == "cluster" ? aws_placement_group.cluster[0].name : null

  tag {
    key                 = "Name"
    value               = "${var.cluster_name}-writer"
    propagate_at_launch = true
  }
  dynamic "tag" {
    for_each = var.tags
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = true
    }
  }

  lifecycle {
    # Scaling policies own desired_capacity; vertical autoscaler steps launch_template
    # between applies, while writers_ami_roll re-converges on new Terraform LT versions.
    ignore_changes = [desired_capacity, launch_template]
  }
}

# ----------------------------------------------------------------------------
# Writers AMI-roll convergence (terraform_data trigger)
# ----------------------------------------------------------------------------
resource "terraform_data" "writers_ami_roll" {
  triggers_replace = [
    aws_launch_template.writer.latest_version,
  ]

  provisioner "local-exec" {
    command = <<-EOT
      set -euo pipefail
      aws autoscaling update-auto-scaling-group \
        --auto-scaling-group-name "${aws_autoscaling_group.writers.name}" \
        --launch-template "LaunchTemplateId=${aws_launch_template.writer.id},Version='${aws_launch_template.writer.latest_version}'" \
        --region "${var.aws_region}"
      REFRESH_CONFIG='${local.writer_refresh_config}'
      aws autoscaling start-instance-refresh \
        --auto-scaling-group-name "${aws_autoscaling_group.writers.name}" \
        --region "${var.aws_region}" \
        --strategy Rolling \
        --desired-configuration "$REFRESH_CONFIG" \
        --preferences '{"InstanceWarmup":${var.refresh_instance_warmup_seconds},"MinHealthyPercentage":${var.refresh_min_healthy_percentage},"AutoRollback":true}'
    EOT
    when    = create
  }
}

# ----------------------------------------------------------------------------
# Writers & Readers ASG scaling policies
# ----------------------------------------------------------------------------
resource "aws_autoscaling_policy" "writers_cpu" {
  count = var.autoscaling_enabled ? 1 : 0

  name                   = "${var.cluster_name}-writers-cpu"
  autoscaling_group_name = aws_autoscaling_group.writers.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ASGAverageCPUUtilization"
    }
    # Scale-in is disabled here and owned solely by writers-asg-idle (<20% CPU for 60m).
    target_value     = var.writer_cpu_target
    disable_scale_in = true
  }
}

resource "aws_autoscaling_policy" "writers_step_up" {
  name                      = "${var.cluster_name}-writers-step-up"
  autoscaling_group_name    = aws_autoscaling_group.writers.name
  policy_type               = "StepScaling"
  adjustment_type           = "ChangeInCapacity"
  metric_aggregation_type   = "Average"
  estimated_instance_warmup = 180

  step_adjustment {
    metric_interval_lower_bound = 0
    scaling_adjustment          = 1
  }
}

resource "aws_autoscaling_policy" "writers_step_down" {
  name                    = "${var.cluster_name}-writers-step-down"
  autoscaling_group_name  = aws_autoscaling_group.writers.name
  policy_type             = "StepScaling"
  adjustment_type         = "ChangeInCapacity"
  metric_aggregation_type = "Average"

  step_adjustment {
    metric_interval_upper_bound = 0
    scaling_adjustment          = -1
  }
}

resource "aws_autoscaling_policy" "readers_step_up" {
  name                   = "${var.cluster_name}-readers-step-up"
  autoscaling_group_name = aws_autoscaling_group.readers.name
  policy_type            = "SimpleScaling"

  adjustment_type    = "ChangeInCapacity"
  scaling_adjustment = 1
  cooldown           = 600
}

# Target-tracking autoscaling (CPU) — horizontal scale for hourly load swings
resource "aws_autoscaling_policy" "readers_cpu" {
  count = var.autoscaling_enabled ? 1 : 0

  name                   = "${var.cluster_name}-readers-cpu"
  autoscaling_group_name = aws_autoscaling_group.readers.name
  policy_type            = "TargetTrackingScaling"

  target_tracking_configuration {
    predefined_metric_specification {
      predefined_metric_type = "ASGAverageCPUUtilization"
    }
    # Scale out at 70%, scale in at 30%
    target_value = var.autoscaling_target_cpu
  }
}

# ----------------------------------------------------------------------------
# Persistent data volumes (keepers + backup replica)
# Why standalone aws_ebs_volume + aws_volume_attachment instead of inline ebs_block_device: inline volumes are BORN AND DYING with the instance - a…
# ----------------------------------------------------------------------------
resource "aws_ebs_volume" "keeper_raft" {
  count = var.keeper_count

  availability_zone = local.az
  size              = var.keeper_disk_size_gb
  type              = "gp3"
  encrypted         = true

  tags = merge(var.tags, {
    Name     = "${var.cluster_name}-keeper-${count.index}-raft"
    NodeType = "keeper"
    DiskRole = "raft"
  })
}

# Device names: /dev/sdb is reserved by the Ubuntu AMI's block-device
# mapping (ephemeral0 slot) - standalone AttachVolume to that name fails
# with "attachment point in use". /dev/sdf|sdg|sdh are free. Graviton NVMe
# remaps them to nvmeXn1 anyway, and cloud-init mounts by LABEL (never
# device path), so the exact name does not matter beyond uniqueness.
resource "aws_volume_attachment" "keeper_raft" {
  count = var.keeper_count

  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.keeper_raft[count.index].id
  instance_id = aws_instance.keeper[count.index].id

  # Explicitly NOT create_before_destroy: CBD propagates (via
  # ForcedCBDTransformer) to the static-IP instance, whose replacement
  # then cannot bind the IP the old node still holds. The volume moves
  # old->new via plain destroy-then-create: detach from old, destroy
  # old, create new (binds the IP), attach. A volume CAN briefly be
  # attached to neither; it cannot be attached to both.
  lifecycle {
    create_before_destroy = false
  }
}

# Backup replica data volumes (standalone, same rationale as writers: the
# backup node's metadata must survive replacement, and inline
# delete_on_termination=false volumes would orphan untagged on every rebuild
# - the orphaned-volume pattern). Count-gated: dev runs without a backup replica.
resource "aws_ebs_volume" "backup_metadata" {
  count = var.enable_backup_replica ? 1 : 0

  availability_zone = local.az
  size              = var.backup_metadata_disk_size_gb
  type              = "gp3"
  encrypted         = true

  tags = merge(var.tags, {
    Name     = "${var.cluster_name}-backup-0-metadata"
    NodeType = "backup"
    DiskRole = "metadata"
  })
}

resource "aws_ebs_volume" "backup_cache" {
  count = var.enable_backup_replica && !(var.cache_on_nvme_instance_store && local.backup_has_nvme) ? 1 : 0

  availability_zone = local.az
  size              = var.backup_cache_disk_size_gb
  type              = "gp3"
  encrypted         = true

  tags = merge(var.tags, {
    Name     = "${var.cluster_name}-backup-0-cache"
    NodeType = "backup"
    DiskRole = "cache"
  })
}

resource "aws_volume_attachment" "backup_metadata" {
  count = var.enable_backup_replica ? 1 : 0

  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.backup_metadata[0].id
  instance_id = aws_instance.backup[0].id
}

resource "aws_volume_attachment" "backup_cache" {
  count = var.enable_backup_replica && !(var.cache_on_nvme_instance_store && local.backup_has_nvme) ? 1 : 0

  device_name = "/dev/sdg"
  volume_id   = aws_ebs_volume.backup_cache[0].id
  instance_id = aws_instance.backup[0].id
}

# ----------------------------------------------------------------------------
# Keepers — fixed-identity EC2, static IPs, dedicated raft disk
# ----------------------------------------------------------------------------
# Why: keeper raft server IDs and peer addresses must be stable. A
#      Keeper-node scale set can re-image with a wiped raft log
#      on any scaling event; fixed EC2 with persistent EBS avoids split-brain.
resource "aws_instance" "keeper" {
  count = var.keeper_count

  ami           = local.ami_id
  instance_type = var.keeper_instance_type
  subnet_id     = var.subnet_id
  private_ip    = local.keeper_ips[count.index]

  vpc_security_group_ids = [var.keepers_sg_id]
  iam_instance_profile   = var.keeper_instance_profile_name

  # Burstable credits: UNLIMITED on the t-family (the FIXED-IDENTITY
  # keeper instance is where the setting actually applies; raft fsync
  # latency must never hit the credit-exhaustion throttle); "standard"
  # on non-burstable families (unlimited fails their launches).
  credit_specification {
    cpu_credits = lookup(local.credits_by_family, regex("^[a-z0-9]+", var.keeper_instance_type), "standard")
  }

  user_data = var.keeper_user_data

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    encrypted             = true
    volume_size           = var.root_disk_size_gb
    volume_type           = "gp3"
    delete_on_termination = true
    # Tag the root volume: even delete-on-termination disks are untagged at
    # creation, and any root volume that ever ends up detached must be
    # identifiable in cost dashboards and cleanup sweeps (the backup
    # root-disk note below runs the same rule).
    tags = merge(var.tags, {
      Name     = "${var.cluster_name}-keeper-${count.index}-root"
      NodeType = "keeper"
    })
  }

  # NO inline /dev/sdb: the raft-log volume is a standalone aws_ebs_volume
  # (see above) re-attached with its log history + server ID on replacement.

  placement_group = var.placement_strategy == "cluster" ? aws_placement_group.cluster[0].name : null

  tags = merge(var.tags, {
    Name     = "${var.cluster_name}-keeper-${count.index}"
    NodeType = "keeper"
  })

  # Keepers converge role-spec changes in-place via spec-sync.timer every 10m;
  # replacing all 3 keepers simultaneously on a BUNDLE_SHA256 user_data change
  # (e.g. IP allowlist or multi-AZ toggle) would destroy Raft quorum at once.
  user_data_replace_on_change = false

  # See the note on aws_instance.keeper below: the autoscaler Lambda may
  # resize keeper instances out-of-band, and toggling cas_config.enabled
  # (upstream <-> antalya server AMI) or bundle hashes must not replace the 3 Keeper Raft nodes.
  lifecycle {
    ignore_changes = [instance_type, ami, user_data]
  }
}

# ----------------------------------------------------------------------------
# Backup replica — dedicated EC2 (prod only)
# ----------------------------------------------------------------------------
# Why: Backups (clickhouse-backup) run on a dedicated replica so compression
#      and S3 upload never compete with production query CPU. B-series
#      equivalent (t4g) bursts during the nightly window, idles otherwise.
resource "aws_instance" "backup" {
  count = var.enable_backup_replica ? 1 : 0

  ami           = local.ami_id
  instance_type = var.backup_instance_type
  subnet_id     = var.subnet_id
  private_ip    = local.backup_ip

  vpc_security_group_ids = [var.backup_sg_id]
  iam_instance_profile   = var.backup_instance_profile_name

  user_data = var.backup_user_data

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  root_block_device {
    encrypted             = true
    volume_size           = var.root_disk_size_gb
    volume_type           = "gp3"
    delete_on_termination = true
    tags = merge(var.tags, {
      Name     = "${var.cluster_name}-backup-0-root"
      NodeType = "backup"
    })
  }

  # NO inline ebs_block_device for /dev/sdb|sdc: the metadata/cache volumes
  # are standalone aws_ebs_volume.backup_* resources (see above) that
  # re-attach with their DATA across replacements - inline
  # delete_on_termination=false volumes orphan UNTAGGED on every rebuild
  # and accumulate silently.

  placement_group = var.placement_strategy == "cluster" ? aws_placement_group.cluster[0].name : null

  tags = merge(var.tags, {
    Name     = "${var.cluster_name}-backup-0"
    NodeType = "backup"
  })

  user_data_replace_on_change = true

  # See the note on aws_instance.keeper above: the autoscaler Lambda may
  # resize instance_type out-of-band.
  lifecycle {
    ignore_changes = [instance_type]
  }
}

# ----------------------------------------------------------------------------
# Scheduled scaling actions (e.g. dev: shrink nights/weekends)
# Cron-shaped load (business hours) shouldn't pay for 24h capacity.
# ----------------------------------------------------------------------------
resource "aws_autoscaling_schedule" "readers" {
  for_each = { for s in var.scheduled_scaling : s.name => s }

  scheduled_action_name  = each.key
  autoscaling_group_name = aws_autoscaling_group.readers.name
  recurrence             = each.value.recurrence
  time_zone              = each.value.time_zone
  min_size               = each.value.min_size
  max_size               = each.value.max_size
  desired_capacity       = each.value.desired_capacity
}
