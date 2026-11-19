# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Provider Version Constraints — Storage Module
# ============================================================================
# Why: The module must be independently `init`-able and `validate`-able for
#      standalone use and CI checks. Re-declaring the provider floor keeps
#      it in lockstep with the AWS root (aws/versions.tf), which pins the
#      same aws >= 5.0 provider the root resolves.
# Without it: Terraform cannot resolve the aws provider when the module is
#             validated standalone, and a provider regression introduced by
#             the root would go undetected at the module boundary.
# ============================================================================
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}
