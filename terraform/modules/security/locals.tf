# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — Security Module
# ============================================================================
# All derived values for this module live here, in ONE locals block (open
# source Terraform convention): resources below reference local.* only.
# ============================================================================

locals {
  # Guardduty and Security Hub publish findings under DIFFERENT severity
  # shapes (detail.severity float vs detail.findings[].Severity.Label enum),
  # so each service gets its own dedicated count-gated rule below.
}
