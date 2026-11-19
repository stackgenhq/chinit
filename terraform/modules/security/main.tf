# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Security Module — GuardDuty + Security Hub + Findings Delivery
# ============================================================================
# Direct aws_* provider resources plus findings delivery into the cluster's
# existing alerting path. NO registry modules are used here: BOTH aws-ia
# security wrappers were evaluated and rejected (decision record below).
# The only aws-ia adoption in this stack is aws-ia/vpc in modules/network.
#
#   - GuardDuty (aws_guardduty_* resources) : managed threat detection. S3
#     data-events protection ON (the cluster's biggest data surface:
#     data/backup/flow-log buckets), EBS malware protection OFF by default
#     (per-scan EBS snapshot costs), EC2 runtime monitoring OFF by default
#     (RDKS agent cost) - all opt-in via variables; metered sources are
#     pinned DISABLED (new-account detectors enable them by default).
#   - Security Hub (aws_securityhub_* resources) : AWS Foundational Security
#     Best Practices + CIS v1.2.0 standards, single-account, single-region,
#     no finding aggregator (the aws-ia wrapper creates one unconditionally
#     and SPECIFIED_REGIONS rejects the calling region; ALL_REGIONS bills
#     every region - neither fits a single-region stack).
#   - EventBridge rules routing HIGH/CRITICAL findings to the monitoring
#     module's SNS alerts topic (no new notification channel).
#
# Why this module exists: the cluster otherwise has NO managed threat
# detection or standards-compliance surface, while the security
# posture (IMDSv2 everywhere, SSM-only access, no static credentials)
# already assumes a hardened baseline. GuardDuty flags compromised nodes and
# credential misuse; Security Hub flags control drift against the AWS
# baseline.
# Without it: A compromised node or leaked credential is visible only via
#             hand-rolled CloudWatch metric alarms, which cannot see
#             GuardDuty-class findings at all.
#
# No provider blocks here: providers are inherited from the root
# (a child provider block would override the consumer's config and
# break SSO/offline validation harnesses).
# ============================================================================


# ============================================================================
# GuardDuty Detector + Features (direct provider resources)
# Managed threat detection across the surfaces this cluster cares about: S3 data events (malicious access patterns on the data / backup / flow-log…
# ============================================================================
resource "aws_guardduty_detector" "primary" {
  count = var.enable_guardduty ? 1 : 0

  enable                       = true
  finding_publishing_frequency = "FIFTEEN_MINUTES"

  tags = var.tags
}

resource "aws_guardduty_detector_feature" "s3_protection" {
  count = var.enable_guardduty && var.enable_s3_protection ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "S3_DATA_EVENTS"
  status      = "ENABLED"
}

resource "aws_guardduty_detector_feature" "ebs_malware_protection" {
  count = var.enable_guardduty && var.enable_malware_protection ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "EBS_MALWARE_PROTECTION"
  status      = "ENABLED"
}

# Metered features the variables keep OFF must be declared DISABLED
# explicitly: a new account-level detector defaults EBS malware protection
# to ENABLED (per-scan billing) and would silently bill otherwise. Declaring
# the resource pins the drift away.
resource "aws_guardduty_detector_feature" "ebs_malware_protection_off" {
  count = var.enable_guardduty && !var.enable_malware_protection ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "EBS_MALWARE_PROTECTION"
  status      = "DISABLED"
}

resource "aws_guardduty_detector_feature" "rds_login_events_off" {
  count = var.enable_guardduty ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "RDS_LOGIN_EVENTS"
  status      = "DISABLED"
}

resource "aws_guardduty_detector_feature" "eks_audit_logs_off" {
  count = var.enable_guardduty ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "EKS_AUDIT_LOGS"
  status      = "DISABLED"
}

resource "aws_guardduty_detector_feature" "lambda_network_logs_off" {
  count = var.enable_guardduty ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "LAMBDA_NETWORK_LOGS"
  status      = "DISABLED"
}

resource "aws_guardduty_detector_feature" "ec2_runtime_monitoring" {
  count = var.enable_guardduty && var.enable_ec2_runtime_monitoring ? 1 : 0

  detector_id = aws_guardduty_detector.primary[0].id
  name        = "RUNTIME_MONITORING"
  status      = "ENABLED"

  additional_configuration {
    name   = "EC2_AGENT_MANAGEMENT"
    status = "DISABLED" # RDKS agent management stays opt-in; runtime events suffice
  }
}

# ============================================================================
# Security Hub + Standards (direct provider resources)
# Continuous control-drift checks against the AWS Foundational Security Best Practices baseline and the CIS v1.
# ============================================================================
resource "aws_securityhub_account" "this" {
  count = var.enable_security_hub ? 1 : 0

  auto_enable_controls = true
}

# Why: The two default standards land via explicit subscriptions instead of
#      enable_default_standards so the exact ARNs (region-scoped for FSBP)
#      are declared resources and removable in-place later.
resource "aws_securityhub_standards_subscription" "fsbp" {
  count = var.enable_security_hub ? 1 : 0

  standards_arn = "arn:aws:securityhub:${var.aws_region}::standards/aws-foundational-security-best-practices/v/1.0.0"

  depends_on = [aws_securityhub_account.this]
}

resource "aws_securityhub_standards_subscription" "cis_v120" {
  count = var.enable_security_hub ? 1 : 0

  standards_arn = "arn:aws:securityhub:::ruleset/cis-aws-foundations-benchmark/v/1.2.0"

  depends_on = [aws_securityhub_account.this]
}

# ============================================================================
# Findings Delivery - EventBridge to the Existing SNS Alerts Topic
# HIGH/CRITICAL findings from either service page the operator through the SAME channel as every other cluster alarm (monitoring module SNS topic),…
# ============================================================================
resource "aws_iam_role" "findings_delivery" {
  count = var.findings_to_sns_enabled ? 1 : 0
  name  = "${var.cluster_name}-sec-findings-eventbridge"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "events.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy" "findings_delivery" {
  count = var.findings_to_sns_enabled ? 1 : 0
  name  = "${var.cluster_name}-sec-findings-publish"
  role  = aws_iam_role.findings_delivery[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = "sns:Publish"
      Resource = var.alert_topic_arn
    }]
  })
}

# DIFFERENT severity shapes, so a single numeric filter cannot cover both.
resource "aws_cloudwatch_event_rule" "guardduty_findings" {
  count = var.findings_to_sns_enabled ? 1 : 0

  name = "${var.cluster_name}-sec-findings-guardduty-high-critical"

  description = "HIGH/CRITICAL GuardDuty findings routed to the cluster SNS alerts topic"

  # GuardDuty: severity >= 7 (7-8.9 HIGH, 9-10 CRITICAL). EventBridge
  # numeric matching syntax: ["<=", ">="] operator-operand pairs - the
  # object form {min=7} is NOT valid pattern syntax (apply-time validated).
  event_pattern = jsonencode({
    source        = ["aws.guardduty"]
    "detail-type" = ["GuardDuty Finding"]
    detail = {
      severity = [{ "numeric" = [">=", 7] }]
    }
  })

  tags = var.tags
}

resource "aws_cloudwatch_event_rule" "securityhub_findings" {
  count = var.findings_to_sns_enabled ? 1 : 0

  name = "${var.cluster_name}-sec-findings-securityhub-high-critical"

  description = "HIGH/CRITICAL Security Hub findings routed to the cluster SNS alerts topic"

  # Security Hub: label-based severity on the imported findings batch
  event_pattern = jsonencode({
    source        = ["aws.securityhub"]
    "detail-type" = ["Security Hub Findings - Imported"]
    detail = {
      findings = {
        Severity = {
          Label = ["HIGH", "CRITICAL"]
        }
      }
    }
  })

  tags = var.tags
}

resource "aws_cloudwatch_event_target" "guardduty_findings_to_sns" {
  count = var.findings_to_sns_enabled ? 1 : 0

  rule     = aws_cloudwatch_event_rule.guardduty_findings[0].name
  arn      = var.alert_topic_arn
  role_arn = aws_iam_role.findings_delivery[0].arn
}

resource "aws_cloudwatch_event_target" "securityhub_findings_to_sns" {
  count = var.findings_to_sns_enabled ? 1 : 0

  rule     = aws_cloudwatch_event_rule.securityhub_findings[0].name
  arn      = var.alert_topic_arn
  role_arn = aws_iam_role.findings_delivery[0].arn
}
