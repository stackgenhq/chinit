# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse Module Outputs — the contract module's surface, re-exported
# ============================================================================
# The custom_data wrappers and the bundle digest now flow from the chinit
# contract module; this module re-exports them so the root (and any
# existing consumer) keeps its wiring unchanged.
# ============================================================================

output "writer_bootstrap" {
  description = "chinit custom_data wrapper for writer nodes (execs chinit boot)"
  value       = module.chinit.bootstrap_script["writer"]
  sensitive   = true
}

output "reader_bootstrap" {
  description = "chinit custom_data wrapper for reader nodes (execs chinit boot)"
  value       = module.chinit.bootstrap_script["reader"]
  sensitive   = true
}

output "keeper_bootstrap" {
  description = "chinit custom_data wrapper for keeper nodes (execs chinit boot)"
  value       = module.chinit.bootstrap_script["keeper"]
  sensitive   = true
}

output "backup_replica_bootstrap" {
  description = "chinit custom_data wrapper for the backup replica (execs chinit boot); null when the replica is disabled"
  value       = var.backup_config.replica.enabled ? module.chinit.bootstrap_script["backup"] : null
  sensitive   = true
}

# ============================================================================
# Config Files Hash - For VMSS Replacement Triggers
# ============================================================================
# Why: XML configs are downloaded at runtime. The digest is the SHA gate
#      chinit recomputes at boot — a bundle change changes it, and the
#      VMSS custom_data hash follows, so config changes roll the fleet.
# ============================================================================
output "config_files_hash" {
  description = "SHA256 gate over the bundle members - changes trigger VMSS replacement"
  value       = module.chinit.config_files_hash
}

output "keeper_config_files_hash" {
  description = "Hash covering the keeper fleet's rendered config source (keeper.yaml + the bundle digest) - changes trigger Keeper VMSS replacement"
  value       = sha256("${module.chinit.keeper_spec_hash}-${module.chinit.config_files_hash}")
}
