# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Module Version Constraints — Network
# ============================================================================
# Why: Pins the minimum toolchain so the module validates standalone and so
#      consumers get a clear failure when running an incompatible binary.
#      Root versions.tf allows aws >= 5.0; THIS module requires >= 6.27
#      because the aws-ia/vpc 4.9.0 registry module (subnets cidrs +
#      single_az NAT + flow-log plumbing) hard-requires it. The root lock
#      file already resolves aws 6.x, so adoption adds no provider churn.
# Without it: OpenTofu/Terraform falls back to implicit (often ancient)
#             provider versions, producing confusing provider-schema errors
#             that only surface at plan time inside the consumer.
#
# NOTE: this intentionally diverges from the root's >= 5.0 floor: the
#       registry module adoption is the reason. (Root lock already pins
#       provider 6.72.0.)
# ============================================================================
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.27.0"
    }
  }
}
