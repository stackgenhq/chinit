# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# User Assigned Managed Identity
# ============================================================================
# Why: Used when System Assigned Identity is not sufficient or when
#      stable identity lifecycle independent of VMs is required.
#      Also solves Terraform limitation with exporting principal_id from
#      orchestrated VMSS resources securely.
# ============================================================================

resource "azurerm_user_assigned_identity" "vmss" {
  count               = var.managed_identity_id == null ? 1 : 0
  location            = var.location
  name                = "${var.cluster_name}-vmss-identity"
  resource_group_name = var.resource_group_name
  tags                = var.tags
}
