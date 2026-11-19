# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse Module Outputs — the contract module's surface, re-exported
# ============================================================================
# The wrappers and the bundle digest now flow from the chinit contract
# module; this module re-exports them so the root (and any existing
# consumer) keeps its wiring unchanged.
# ============================================================================

output "writer_bootstrap" {
  description = "Stage-0 user-data wrapper for writer nodes (execs chinit boot)"
  value       = module.chinit.bootstrap_script["writer"]
}

output "reader_bootstrap" {
  description = "Stage-0 user-data wrapper for reader nodes (execs chinit boot)"
  value       = module.chinit.bootstrap_script["reader"]
}

output "keeper_bootstrap" {
  description = "Stage-0 user-data wrapper for keeper nodes (execs chinit boot)"
  value       = module.chinit.bootstrap_script["keeper"]
}

output "backup_replica_bootstrap" {
  description = "Stage-0 user-data wrapper for the backup replica (execs chinit boot)"
  value       = module.chinit.bootstrap_script["backup"]
}

output "config_files_hash" {
  description = "Hash of all rendered config files - triggers instance refresh when configs change"
  value       = module.chinit.config_files_hash
}

output "bootstrap_script_size" {
  description = "Largest stage-0 wrapper size in bytes (must stay under the 16KB user_data limit)"
  value = max([
    for role, script in module.chinit.bootstrap_script : length(script)
  ]...)
}

output "role_scripts" {
  description = "Role -> rendered chinit node spec (uploaded to S3 as chinit-<role>.yaml by the contract module)"
  value       = module.chinit.chinit_specs
  sensitive   = true
}
