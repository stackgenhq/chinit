# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Provider Version Constraints
# ============================================================================
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.1"
    }
    # Why: The rotation Lambda's Python source is zipped by the archive
    #      provider (filename + content blocks) so the whole module stays in
    #      main.tf — no bundled zip artifacts in git, and code edits change the
    #      source_code_hash which triggers a Lambda update.
    # Without it: The Lambda package would have to be pre-built and committed,
    #             or built in CI, which breaks the single-module portability
    #             this port is aiming for.
    archive = {
      source  = "hashicorp/archive"
      version = ">= 2.4"
    }
    time = {
      source  = "hashicorp/time"
      version = ">= 0.9"
    }
  }
}
