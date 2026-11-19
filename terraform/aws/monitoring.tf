# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Monitoring + security posture
# ============================================================================
# CloudWatch alarms with scaling guidance, budgets, and the threat-detection
# layer (GuardDuty + Security Hub with findings routed onto the cluster's
# own SNS topic — no separate notification channel to wire).
# ============================================================================

module "monitoring" {
  source = "./modules/monitoring"

  cluster_name = var.cluster_name
  enabled      = true

  alert_emails = var.alert_emails

  writer_hostnames = local.writer_hostnames
  keeper_count     = var.keeper_count
  keeper_hostnames = local.keeper_hostnames

  readers_asg_name    = module.compute.readers_asg_name
  autoscaling_enabled = var.autoscaling_enabled

  writers_asg_name = module.compute.writers_asg_name

  enable_idle_alerts       = true
  enable_ch_alerts         = true
  enable_writer_alarms     = true
  enable_vertical_guidance = true
  enable_backup_alerts     = var.enable_backup

  backup_bucket_name = module.storage.backup_bucket_name

  budget_enabled        = var.budget_monthly_amount > 0
  budget_monthly_amount = var.budget_monthly_amount
  budget_alert_emails   = var.alert_emails

  tags = local.common_tags

  depends_on = [module.compute]
}

module "security" {
  source = "./modules/security"

  cluster_name = var.cluster_name
  aws_region   = var.region

  findings_to_sns_enabled = true
  alert_topic_arn         = module.monitoring.alert_topic_arn

  enable_guardduty     = var.enable_guardduty
  enable_s3_protection = var.enable_s3_protection
  enable_security_hub  = var.enable_security_hub
  # Metered GuardDuty sources stay OFF unless explicitly enabled.
  enable_malware_protection     = false
  enable_ec2_runtime_monitoring = false

  tags = local.common_tags
}
