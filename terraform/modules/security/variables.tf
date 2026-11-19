# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Security Module Variables
# ============================================================================
# Defaults encode the cost-aware security posture: GuardDuty + Security Hub
# ON with the free S3 data-events surface; metered features (EBS malware
# scans, EC2 runtime monitoring / RDKS) OFF until explicitly enabled.
# ============================================================================

variable "cluster_name" {
  description = "Name of the ClickHouse cluster. Derives every resource name in this module (detector, event rule, IAM role). Must be lowercase/DNS-safe."
  type        = string
}

variable "aws_region" {
  description = "Deployment region. Constrains Security Hub aggregation to SPECIFIED_REGIONS = [region] (single-region cluster) and is used for the finding rule arn-namespace checks."
  type        = string
}

variable "alert_topic_arn" {
  description = "ARN of the monitoring module's SNS alerts topic that receives HIGH/CRITICAL findings via EventBridge. Consumed in attribute position (event target), so an apply-time-known ARN (module output on a first apply) is tolerated. null/empty = findings stay in the GuardDuty/Security Hub consoles."
  type        = string
  default     = null
}

variable "findings_to_sns_enabled" {
  description = "Gate for the EventBridge -> SNS findings delivery set (IAM role, 2 rules, 2 targets). MUST be known at plan time: pass a plain bool, NOT an expression derived from another module's output (e.g. wire var.monitoring_config.enabled, not the monitoring module's enabled output). true without a valid alert_topic_arn breaks delivery."
  type        = bool
  default     = true
}

variable "enable_guardduty" {
  description = "Enable GuardDuty managed threat detection (detector + enabled feature set). false disables the whole service for this cluster (e.g. a separate security account owns detection)."
  type        = bool
  default     = true
}

variable "enable_s3_protection" {
  description = "GuardDuty S3 data-events protection (malicious access patterns on the cluster's data/backup/flow-log buckets). No per-use cost; keep enabled unless the account already runs S3 protection elsewhere."
  type        = bool
  default     = true
}

variable "enable_malware_protection" {
  description = "GuardDuty EBS malware protection (scans EBS volumes of EC2 findings). METERED per snapshot scan (~$0.05/GB-month equivalent); default off because writer/reader volumes are large metadata/cache disks."
  type        = bool
  default     = false
}

variable "enable_ec2_runtime_monitoring" {
  description = "GuardDuty EC2 runtime monitoring (kernel-level process/file/network events via the GuardDuty managed agent). METERED per managed host-hour; default off - enable for incident-response-sensitive prod profiles."
  type        = bool
  default     = false
}

variable "enable_security_hub" {
  description = "Enable Security Hub with the AWS Foundational Security Best Practices + CIS v1.2.0 standards (control drift baseline). false disables the service (e.g. a delegated Security Hub administrator already covers this account)."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags merged into every resource created by this module."
  type        = map(string)
  default     = {}
}
