# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Compute — the four node roles (AWS)
# ============================================================================
# writers/readers: lease-identity ASGs (a replacement re-claims its
#   ordinal; user_data embeds config_files_hash via the spec so a config
#   change rolls the fleet through user_data_replace_on_change / instance
#   refresh).
# keepers/backup: fixed instances with static private IPs (the +20/+30
#   octet offsets chinit's StaticHostname maps) and persistent volumes.
# ============================================================================

# ----------------------------------------------------------------------------
# Shared launch-template settings (IMDSv2 required — the credential path
# chinit and the S3 disk use)
# ----------------------------------------------------------------------------

# ----------------------------------------------------------------------------
# Public Golden AMI auto-discovery (cross-account safe)
# ----------------------------------------------------------------------------
# AWS hides resource tags on public AMIs from other accounts, so cross-account
# discovery filters on public visibility, architecture, name, and description
# (which embeds `(antalya)` or `(upstream)` plus the stackgenhq/chinit URL).
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
# Writers ASG
# ----------------------------------------------------------------------------
resource "aws_launch_template" "writer" {
  name_prefix   = "${local.name_prefix}-writer-"
  image_id      = local.golden_ami_id
  instance_type = var.instance_type
  user_data     = base64encode(module.chinit.bootstrap_script["writer"])

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  iam_instance_profile {
    name = aws_iam_instance_profile.node.name
  }

  vpc_security_group_ids = [aws_security_group.nodes["writers"].id]

  # Root volume: public AMIs have an unencrypted backing snapshot (AWS
  # requirement for public sharing); setting encrypted = true here encrypts
  # every launched instance's root EBS volume with the consumer account's
  # default EBS KMS key.
  block_device_mappings {
    device_name = "/dev/sda1"
    ebs {
      encrypted             = true
      volume_size           = var.root_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  # delete_on_termination: writers are stateful-ish (hot parts), but the
  # durable tier is S3 + replication; a replaced writer re-syncs via
  # schema-sync + replica-sync. Persistent ASG member volumes would
  # orphan on every scale-in.
  block_device_mappings {
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

  block_device_mappings {
    device_name = "/dev/sdc"
    ebs {
      encrypted             = true
      volume_size           = var.cache_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.common_tags, { Name = "${local.name_prefix}-writer", NodeType = "writer" })
  }

  # NodeType on the VOLUMES: the GrowOwnTaggedVolume condition keys on it.
  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.common_tags, { NodeType = "clickhouse" })
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_autoscaling_group" "writers" {
  name                = "${local.name_prefix}-writers"
  desired_capacity    = var.writer_desired
  min_size            = 1
  max_size            = var.writer_max
  vpc_zone_identifier = [local.subnet_id]

  launch_template {
    id      = aws_launch_template.writer.id
    version = "$Latest"
  }

  # user_data changes roll the fleet via instance refresh: the spec
  # embeds config_files_hash, so any config/bundle edit creates a new
  # launch-template version ($Latest picks it up) — start the refresh
  # from your root (or the console) after a config apply. Note:
  # refresh AUTO-ROLLBACK needs a pinned numeric version; $Latest
  # trades that for always-current config.
  instance_refresh {
    strategy = "Rolling"

    preferences {
      # The boot's Critical steps must finish (server start + health
      # gate) before the next node rolls.
      instance_warmup        = 300
      min_healthy_percentage = 50
    }
  }

  tag {
    key                 = "Name"
    value               = "${local.name_prefix}-writer"
    propagate_at_launch = true
  }
  tag {
    key                 = "Cluster"
    value               = var.cluster_name
    propagate_at_launch = true
  }

}

# ----------------------------------------------------------------------------
# Readers ASG — Spot-capable (chinit's spot-watch drains on the EC2
# interruption notice; the lease TTL releases the ordinal when the drain
# cannot)
# ----------------------------------------------------------------------------
resource "aws_launch_template" "reader" {
  name_prefix   = "${local.name_prefix}-reader-"
  image_id      = local.golden_ami_id
  instance_type = var.instance_type
  user_data     = base64encode(module.chinit.bootstrap_script["reader"])

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  iam_instance_profile {
    name = aws_iam_instance_profile.node.name
  }

  vpc_security_group_ids = [aws_security_group.nodes["readers"].id]

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

  block_device_mappings {
    device_name = "/dev/sdc"
    ebs {
      encrypted             = true
      volume_size           = var.cache_disk_size_gb
      volume_type           = "gp3"
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.common_tags, { Name = "${local.name_prefix}-reader", NodeType = "reader" })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.common_tags, { NodeType = "clickhouse" })
  }

  tags = local.common_tags

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_autoscaling_group" "readers" {
  name                = "${local.name_prefix}-readers"
  desired_capacity    = var.reader_desired
  min_size            = 0
  max_size            = var.reader_max
  vpc_zone_identifier = [local.subnet_id]

  launch_template {
    id      = aws_launch_template.reader.id
    version = "$Latest"
  }

  # Mixed Spot/On-Demand: 100% spot with on-demand base 0 is the cheap
  # default; readers are stateless (S3 + replication is the durability),
  # so a reclaim costs a lease claim + a re-sync, not data.
  dynamic "mixed_instances_policy" {
    for_each = var.reader_spot ? [1] : []
    content {
      instances_distribution {
        on_demand_base_capacity                  = 0
        on_demand_percentage_above_base_capacity = 0
        spot_allocation_strategy                 = "lowest-price"
      }

      launch_template {
        launch_template_specification {
          launch_template_id = aws_launch_template.reader.id
          version            = "$Latest"
        }
      }
    }
  }

  instance_refresh {
    strategy = "Rolling"

    preferences {
      instance_warmup        = 300
      min_healthy_percentage = 0
    }
  }

  tag {
    key                 = "Name"
    value               = "${local.name_prefix}-reader"
    propagate_at_launch = true
  }
  tag {
    key                 = "Cluster"
    value               = var.cluster_name
    propagate_at_launch = true
  }
}

# ----------------------------------------------------------------------------
# Keepers — fixed instances, static IPs at +20+i (the octet band
# chinit's StaticHostname maps), persistent raft volume re-attached
# across replacements (the log history + server id live on it)
# ----------------------------------------------------------------------------
resource "aws_ebs_volume" "keeper_raft" {
  count = var.keeper_count

  availability_zone = data.aws_availability_zones.available.names[0]
  size              = var.keeper_disk_size_gb
  type              = "gp3"
  encrypted         = true

  tags = merge(local.common_tags, {
    Name     = "${local.name_prefix}-keeper-${count.index}-raft"
    NodeType = "keeper"
  })
}

resource "aws_instance" "keeper" {
  count = var.keeper_count

  ami           = local.golden_ami_id
  instance_type = var.keeper_instance_type
  subnet_id     = local.subnet_id
  private_ip    = local.keeper_ips[count.index]

  vpc_security_group_ids = [aws_security_group.nodes["keepers"].id]
  iam_instance_profile   = aws_iam_instance_profile.node.name

  user_data = base64encode(module.chinit.bootstrap_script["keeper"])

  # A config change replaces the keeper (spec hash rides user_data).
  user_data_replace_on_change = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  # Metadata disk INLINE at /dev/sdb (the first metadata candidate; the
  # ephemeral0 slot that blocks STANDALONE attachment is only a
  # standalone-AttachVolume problem). Small: keepers never run
  # clickhouse-server, but chinit's Critical mount step requires the
  # metadata volume on every role.
  ebs_block_device {
    device_name           = "/dev/sdb"
    volume_size           = min(var.metadata_disk_size_gb, 32) # keepers need little
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = false # survives replacement
    tags = merge(local.common_tags, {
      Name     = "${local.name_prefix}-keeper-${count.index}-metadata"
      NodeType = "clickhouse"
    })
  }

  root_block_device {
    encrypted             = true
    volume_size           = var.root_disk_size_gb
    volume_type           = "gp3"
    delete_on_termination = true
    tags = merge(local.common_tags, {
      Name     = "${local.name_prefix}-keeper-${count.index}-root"
      NodeType = "clickhouse"
    })
  }

  tags = merge(local.common_tags, {
    Name     = "${local.name_prefix}-keeper-${count.index}"
    NodeType = "keeper"
  })
}

# Keeper raft volume: standalone (the log history + server id survive
# replacement). Attached at /dev/sdf — the first KEEPER candidate and
# NOT the sdb slot that blocks standalone attachment; metadata rides
# inline at /dev/sdb inside aws_instance.keeper above.
resource "aws_volume_attachment" "keeper_raft" {
  count = var.keeper_count

  device_name = "/dev/sdf"
  volume_id   = aws_ebs_volume.keeper_raft[count.index].id
  instance_id = aws_instance.keeper[count.index].id
}

# ----------------------------------------------------------------------------
# Backup replica — single fixed instance at the +30 offset
# ----------------------------------------------------------------------------
resource "aws_instance" "backup" {
  count = var.enable_backup ? 1 : 0

  ami           = local.golden_ami_id
  instance_type = var.instance_type
  subnet_id     = local.subnet_id
  private_ip    = local.backup_ip

  vpc_security_group_ids = [aws_security_group.nodes["backup"].id]
  iam_instance_profile   = aws_iam_instance_profile.node.name

  user_data = base64encode(module.chinit.bootstrap_script["backup"])

  user_data_replace_on_change = true

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "enabled"
  }

  # Metadata disk INLINE at /dev/sdb (the first metadata candidate; the
  # backup's hot tier). Persistent: the backup replica's replicated
  # tables re-attach on replacement.
  ebs_block_device {
    device_name           = "/dev/sdb"
    volume_size           = var.metadata_disk_size_gb
    volume_type           = "gp3"
    encrypted             = true
    delete_on_termination = false # survives replacement
    tags = merge(local.common_tags, {
      Name     = "${local.name_prefix}-backup-metadata"
      NodeType = "clickhouse"
    })
  }

  root_block_device {
    encrypted             = true
    volume_size           = var.root_disk_size_gb
    volume_type           = "gp3"
    delete_on_termination = true
    tags = merge(local.common_tags, {
      Name     = "${local.name_prefix}-backup-root"
      NodeType = "clickhouse"
    })
  }

  tags = merge(local.common_tags, {
    Name     = "${local.name_prefix}-backup-0"
    NodeType = "backup"
  })
}