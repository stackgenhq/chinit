# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.0"

  required_providers {
    # Allow AVM module to specify its own version constraints
    # AVM modules require >= 4.36.0, but we allow broader range for compatibility
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 3.0"
    }

    # Required for creating custom Log Analytics tables
    # The azurerm provider doesn't support creating custom table schemas,
    # so we use azapi to interact with the Azure REST API directly
    azapi = {
      source  = "azure/azapi"
      version = "~> 2.7.0" # Pinned to 2.7.x - v2.8.0 has "Missing Resource Identity" bug
    }
  }
}
