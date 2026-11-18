# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Packer Template for ClickHouse Golden Image
# ============================================================================
# This template builds a pre-baked VM image with:
# - Ubuntu 22.04 LTS Gen2 base
# - ClickHouse server/client (pinned version)
# - Azure CLI
# - OS optimizations (THP disabled, ulimits configured)
#
# Usage:
#   packer init .
#   packer build -var-file=../vars/dev.pkrvars.hcl clickhouse.pkr.hcl
# ============================================================================

packer {
  required_plugins {
    azure = {
      source  = "github.com/hashicorp/azure"
      version = ">= 2.0.0"
    }
  }
}

# ============================================================================
# Variables
# ============================================================================

variable "subscription_id" {
  type        = string
  description = "Azure subscription ID"
}

variable "resource_group_name" {
  type        = string
  description = "Resource group containing the Azure Compute Gallery"
}

variable "location" {
  type        = string
  description = "Azure region for building the image"
  default     = "eastus"
}

variable "gallery_name" {
  type        = string
  description = "Name of the Azure Compute Gallery"
}

variable "image_name" {
  type        = string
  description = "Name of the image definition in the gallery"
  default     = "clickhouse-node"
}

variable "image_version" {
  type        = string
  description = "Version number for this image (e.g., 1.0.0)"
}

variable "clickhouse_version" {
  type        = string
  description = "ClickHouse version to install"
  default     = "24.8.4.13"
}

variable "vm_size" {
  type        = string
  description = "VM size for the build instance"
  default     = "Standard_D2s_v5"
}

variable "git_sha" {
  type        = string
  description = "Git commit SHA for build traceability"
  default     = "unknown"
}

variable "git_dirty" {
  type        = string
  description = "Whether build has uncommitted changes (empty or 'dirty')"
  default     = ""
}

variable "tags" {
  type        = map(string)
  description = "Tags to apply to resources"
  default = {
    "purpose"    = "golden-image-build"
    "managed-by" = "packer"
  }
}

variable "otel_collector_version" {
  type        = string
  description = "OpenTelemetry Collector version to install"
  default     = "0.118.0"
}

variable "chinit_version" {
  type        = string
  description = "chinit release version to pre-install (from stackgenhq/chinit releases)"
  default     = "0.1.0"
}

variable "clickhouse_backup_version" {
  type        = string
  description = "clickhouse-backup version to install"
  default     = "2.5.0"
}

# ============================================================================
# Source: Azure ARM Builder
# ============================================================================
# Why: Uses ARM builder for Azure Compute Gallery integration. The builder
#      creates a temporary VM, runs provisioners, then captures the image
#      directly to the gallery.
# ============================================================================

source "azure-arm" "clickhouse" {
  subscription_id = var.subscription_id

  # Use Azure CLI or Managed Identity for authentication
  # When running locally: az login
  # When running in CI: use service principal or managed identity
  use_azure_cli_auth = true

  # Build VM configuration
  location = var.location
  vm_size  = var.vm_size

  # Source image: Ubuntu 22.04 LTS Gen2 (same as current VMSS)
  os_type         = "Linux"
  image_publisher = "Canonical"
  image_offer     = "0001-com-ubuntu-server-jammy"
  image_sku       = "22_04-lts-gen2"

  # NOTE: TrustedLaunch disabled for compatibility with azurerm_orchestrated_virtual_machine_scale_set
  # which does not support secure_boot_enabled and vtpm_enabled arguments

  # Target: Azure Compute Gallery
  shared_image_gallery_destination {
    subscription         = var.subscription_id
    resource_group       = var.resource_group_name
    gallery_name         = var.gallery_name
    image_name           = var.image_name
    image_version        = var.image_version
    replication_regions  = [var.location]
    storage_account_type = "Standard_LRS"
  }

  # Note: managed_image_name/managed_image_resource_group_name removed because
  # TrustedLaunch images can only be published directly to Shared Image Gallery

  # Build timeout
  async_resourcegroup_delete = true

  azure_tags = merge(var.tags, {
    "clickhouse-version" = var.clickhouse_version
    "image-version"      = var.image_version
    "git-sha"            = var.git_sha
    "git-dirty"          = var.git_dirty
  })
}

# ============================================================================
# Build Configuration
# ============================================================================

build {
  sources = ["source.azure-arm.clickhouse"]

  # Wait for cloud-init to complete (Azure VMs run cloud-init on first boot)
  provisioner "shell" {
    inline = [
      "echo 'Waiting for cloud-init to complete...'",
      "cloud-init status --wait || true",
      "echo 'Cloud-init completed'"
    ]
  }

  # Install ClickHouse and dependencies
  provisioner "shell" {
    environment_vars = [
      "CLICKHOUSE_VERSION=${var.clickhouse_version}",
      "OTEL_COLLECTOR_VERSION=${var.otel_collector_version}",
      "CLICKHOUSE_BACKUP_VERSION=${var.clickhouse_backup_version}",
      "CHINIT_VERSION=${var.chinit_version}",
      "DEBIAN_FRONTEND=noninteractive"
    ]
    script = "scripts/install-clickhouse.sh"
  }

  # Create marker file to indicate golden image
  # Cloud-init will check for this to skip installation
  provisioner "shell" {
    inline = [
      "echo '${var.clickhouse_version}' | sudo tee /etc/clickhouse-golden-image",
      "echo 'Golden image marker created'"
    ]
  }

  # Cleanup for image generalization
  provisioner "shell" {
    inline = [
      "echo 'Cleaning up for image capture...'",

      # Stop ClickHouse (it will be configured and started by cloud-init)
      "sudo systemctl stop clickhouse-server || true",
      "sudo systemctl disable clickhouse-server || true",

      # Remove machine-specific files
      "sudo rm -f /etc/machine-id",
      "sudo truncate -s 0 /etc/machine-id",

      # Clear ClickHouse data directory (will be mounted from disk)
      "sudo rm -rf /var/lib/clickhouse/*",

      # Clear logs
      "sudo rm -rf /var/log/clickhouse-server/*",
      "sudo rm -rf /var/log/azure/*",
      "sudo rm -rf /var/log/cloud-init*.log",

      # Clear apt cache
      "sudo apt-get clean",
      "sudo rm -rf /var/lib/apt/lists/*",

      # Clear bash history
      "rm -f ~/.bash_history || true",

      # Deprovision the VM (required for Azure)
      "sudo waagent -deprovision+user -force",

      "echo 'Cleanup completed - image ready for capture'"
    ]
  }
}
