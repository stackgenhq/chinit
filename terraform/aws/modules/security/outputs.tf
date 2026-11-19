# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Security Module Outputs
# ============================================================================
# Consumed by the root module + operator runbooks: detector/hub identifiers
# and the finding-rule wiring state (so docs can state where findings go in
# each configuration).
# ============================================================================

output "guardduty_detector_id" {
  description = "GuardDuty detector ID to quote in console/CLI investigation commands (aws guardduty list-findings --detector-id ...). null when enable_guardduty is false."
  value       = var.enable_guardduty ? aws_guardduty_detector.primary[0].id : null
}

output "security_hub_regions" {
  description = "Regions Security Hub covers (single-region cluster: exactly [aws_region]; no aggregator - see module comment). Empty when enable_security_hub is false."
  value       = var.enable_security_hub ? [var.aws_region] : []
}

output "findings_alert_enabled" {
  description = "Whether HIGH/CRITICAL findings are routed to the monitoring SNS alerts topic (findings_to_sns_enabled && alert_topic_arn wired). false = findings remain console-only."
  value       = var.findings_to_sns_enabled && var.alert_topic_arn != null && var.alert_topic_arn != ""
}
