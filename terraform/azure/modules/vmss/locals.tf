# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Local: Resource Group ID
# ============================================================================
# Why: Uses the resource_group_id variable directly instead of a data source
#      to avoid "(known after apply)" issues that cause resource replacement.
#      When using data sources, the ID is computed on every apply, causing
#      AVM modules to show parent_id as "known after apply" and force
#      replacement of resources like VNet and subnets.
# Without it: Resources would be replaced on every apply due to parent_id
#             being computed from a data source.
# ============================================================================
locals {
  resource_group_id = var.resource_group_id

  # Health Probe Configuration
  # Hardcoded as locals to enforce standard configuration (HTTPS/8443)
  health_probe_protocol = "https"
  health_probe_port     = 8443
  health_probe_path     = "/ping"

  # Hash of cloud-init scripts AND config files to trigger VM replacement
  # Why: Azure VMs don't automatically recreate when user_data changes. By
  #      creating terraform_data resources that depend on the hash, we can use
  #      replace_triggered_by to force VM replacement when scripts are updated.
  #      Separate hashes allow independent replacement of each VM type when only
  #      its script changes, avoiding unnecessary replacements.
  # IMPORTANT: We include config_files_hash because XML configs are downloaded
  #            at runtime (not embedded in cloud-init.sh). Without this, changing
  #            settings.xml or other configs would NOT trigger VM replacement.
  # Without it: Changes to cloud-init scripts or XML configs don't trigger VM
  #             recreation, requiring manual VM replacement or taint operations.
  combined_config_hash   = var.config_files_hash != "" ? var.config_files_hash : "no-config"
  writer_cloud_init_hash = md5("${var.writer_cloud_init}${local.combined_config_hash}")
  reader_cloud_init_hash = md5("${var.reader_cloud_init}${local.combined_config_hash}")
  keeper_config_hash     = var.keeper_config_files_hash != "" ? var.keeper_config_files_hash : local.combined_config_hash
  keeper_cloud_init_hash = md5("${var.keeper_cloud_init}${local.keeper_config_hash}")
}

# ============================================================================
# Terraform Data Resources to Trigger VM Replacement on Cloud-init Script Changes
# ============================================================================
# Why: Azure VMs don't automatically recreate when user_data changes. These
#      terraform_data resources change when the cloud-init script hash changes,
#      allowing us to use replace_triggered_by to force VM replacement.
#      Separate resources allow independent replacement of each VM type when
#      only its script changes, avoiding unnecessary replacements of other types.
#      terraform_data is the modern, built-in alternative to null_resource
#      and doesn't require a provider.
# Without it: Changes to cloud-init scripts don't trigger VM recreation,
#             requiring manual VM replacement or taint operations.
# ============================================================================
resource "terraform_data" "writer_cloud_init_trigger" {
  # Use a map for triggers_replace to ensure idempotency
  # The hash value is stable as long as the cloud-init script content doesn't change
  triggers_replace = {
    hash = local.writer_cloud_init_hash
  }
}

resource "terraform_data" "reader_cloud_init_trigger" {
  # Use a map for triggers_replace to ensure idempotency
  # The hash value is stable as long as the cloud-init script content doesn't change
  triggers_replace = {
    hash = local.reader_cloud_init_hash
  }
}

resource "terraform_data" "keeper_cloud_init_trigger" {
  # Use a map for triggers_replace to ensure idempotency
  # The hash value is stable as long as the cloud-init script content doesn't change
  triggers_replace = {
    hash = local.keeper_cloud_init_hash
  }
}
