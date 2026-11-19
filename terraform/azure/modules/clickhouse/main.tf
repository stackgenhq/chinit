# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ClickHouse Module - an EXTENSION of the chinit contract module
# ============================================================================
# This module no longer renders chinit specs/wrappers/uploads itself: it
# consumes the open-source contract module (stackgenhq/chinit
# terraform/modules/chinit/azure) and extends it with THIS stack's
# business layer — the tuned CHI/CHK specs, the SRE troubleshooting
# gallery, and the bash-era config artifacts the SRE scripts still
# fetch. The chinit interface (BootSpec rendering, the SHA-gated
# bundle digest, the bundle uploads, the custom_data wrappers) has
# exactly one implementation, in the open repo.
# ============================================================================

locals {
  # CHI spec — the chinit-loaded source of truth for config rendering,
  # schema sync and replica drop (cloud-free subset). replica_count MUST
  # be the fixed slot CEILING (writer_max + reader_max), never live
  # counts: CR host names (01-N, writers first then readers offset by
  # the writer ceiling) stay stable across ALL scaling events —
  # rendering live counts would re-key reader CR slots on every
  # scale-out and orphan keeper-path data.
  chi_spec = templatefile("${path.module}/templates/chi-spec.yaml", {
    cluster_name                 = var.cluster_name
    replica_count                = var.writer_count + var.reader_count
    keepers                      = [for i in range(var.keeper_count) : "${var.keeper_hostname_prefix}${i}.${var.dns_zone_name}"]
    default_password_secret_name = "clickhouse-default-password"
    admin_password_secret_name   = "clickhouse-admin-password"
    writer_password_secret_name  = "clickhouse-writer-password"
  })

  # CHK spec for the keeper nodes — rendered with the keeper fleet
  # size; chinit's configure_keeper renders the single-file config
  # from it (raft entries get cloud DNS at render time).
  keeper_spec = templatefile("${path.module}/templates/keeper-spec.yaml", {
    cluster_name = var.cluster_name
    keeper_count = var.keeper_count
  })

  # THIS stack's storage.xml override (the Azure blob cold tier). The
  # static trio's other members (network/ssl) and the bootstrap DDL
  # come from the contract module — the extension overrides only what
  # it owns.
  storage_xml = templatefile("${path.module}/templates/configs/storage.xml", {})
}

# ============================================================================
# Storage Container for Scripts + Specs
# ============================================================================
# Why: Provides a container to store the chinit role specs and static XML
#      configs. The specs are small and could ride in custom_data, but the
#      blob keeps every role's spec reviewable/versioned in one place and
#      lets config changes roll without touching VM custom_data.
# Without it: The specs and configs have nowhere to live for the boot
#             fetch.
# ============================================================================
# Data source to get storage account ID if not provided
# Note: Use var.lookup_storage_account to control this instead of checking storage_account_id != null
# because the ID value may be computed and unknown at plan time.
data "azurerm_storage_account" "main" {
  count               = var.storage_config.lookup_account ? 1 : 0
  name                = var.storage_config.account_name
  resource_group_name = var.resource_group_name != null ? var.resource_group_name : ""
}

resource "azurerm_storage_container" "scripts" {
  name                  = "scripts"
  storage_account_id    = var.storage_config.lookup_account ? data.azurerm_storage_account.main[0].id : var.storage_config.account_id
  container_access_type = "private"
}

# ============================================================================
# Upload SRE Troubleshooting Scripts to Blob Storage
# ============================================================================
# Why: Stores SRE troubleshooting scripts that help operators diagnose and
#      troubleshoot ClickHouse issues. Scripts are downloaded to VMs during
#      bootstrap and available in ~/sre/scripts/ directory.
# Without it: Operators must manually transfer troubleshooting scripts to VMs,
#             making incident response slower and less standardized.
# ============================================================================

# Read SRE scripts from templates directory
locals {
  sre_scripts = {
    "ch-common.sh"        = file("${path.module}/templates/sre-scripts/ch-common.sh")
    "ch-health.sh"        = file("${path.module}/templates/sre-scripts/ch-health.sh")
    "ch-status.sh"        = file("${path.module}/templates/sre-scripts/ch-status.sh")
    "ch-diag.sh"          = file("${path.module}/templates/sre-scripts/ch-diag.sh")
    "ch-replication.sh"   = file("${path.module}/templates/sre-scripts/ch-replication.sh")
    "ch-queries.sh"       = file("${path.module}/templates/sre-scripts/ch-queries.sh")
    "ch-keeper.sh"        = file("${path.module}/templates/sre-scripts/ch-keeper.sh")
    "ch-disk.sh"          = file("${path.module}/templates/sre-scripts/ch-disk.sh")
    "orphaned-data-gc.sh" = file("${path.module}/templates/sre-scripts/orphaned-data-gc.sh")
    "README.md"           = file("${path.module}/templates/sre-scripts/README.md")
  }

  # List of script names for bootstrap download (space-separated for shell)
  sre_script_names = join(" ", keys(local.sre_scripts))
}

# Upload each SRE script to blob storage
# Uses simple naming (sre-scripts/<name>) for easy download via Managed Identity
resource "azurerm_storage_blob" "sre_scripts" {
  for_each = local.sre_scripts

  name                 = "sre-scripts/${each.key}"
  storage_container_id = azurerm_storage_container.scripts.id
  type                 = "Block"
  content_type         = each.key == "README.md" ? "text/markdown" : "text/x-shellscript"
  source_content       = each.value
}

# ============================================================================
# SAS token generation removed - Managed Identity is now the only auth method
# ============================================================================

# ============================================================================
# The chinit contract — the open-source renderer
# ============================================================================
# What flows FROM here (extension hooks): the tuned chi_spec/keeper_spec
# and THIS stack's storage.xml override. What flows BACK: the per-role
# custom_data wrappers, the bundle digest, and the uploaded bundle.
module "chinit" {
  # Co-located in this repo — local path, mirroring the AWS-side
  # ecosystem module. The ?ref pin semantics move to the repo tag for
  # external consumers.
  source = "../chinit"

  cluster_name  = var.cluster_name
  region        = var.location
  dns_zone_name = coalesce(var.dns_zone_name, "")
  timezone      = var.timezone

  # Identity ceilings: the per-role counts this stack passes ARE the VMSS
  # maxima (autoscale maxima raise them via the same variables).
  writer_max       = var.writer_count
  writer_hostnames = [for i in range(var.writer_count) : "writer-${i}"]
  reader_max       = var.reader_count
  reader_count     = var.reader_count
  keeper_hostnames = [for i in range(var.keeper_count) : "${var.keeper_hostname_prefix}${i}"]
  enable_backup    = var.backup_config.replica.enabled

  # Cloud resources the specs point at (this stack owns them).
  storage_account_name        = var.storage_config.account_name
  scripts_container_name      = azurerm_storage_container.scripts.name
  data_container_name         = var.storage_config.container_name
  backup_storage_account_name = var.backup_config.storage_account_name
  backup_container_name       = var.backup_config.storage_container_name
  backup_retention_days       = var.backup_config.retention_days
  backup_compression_format   = var.backup_config.compression_format
  claimer_table_name          = var.storage_config.node_claims_table_name
  subnet_cidr                 = coalesce(var.network_access_config.subnet_cidr, "10.0.1.0/24")

  # Secret NAMES (Key Vault resolves values at boot via managed identity).
  default_password_secret_name   = "clickhouse-default-password"
  admin_password_secret_name     = "clickhouse-admin-password"
  writer_password_secret_name    = "clickhouse-writer-password"
  tls_certificate_secret_name    = coalesce(var.tls_config.certificate_secret_name, "")
  tls_private_key_secret_name    = coalesce(var.tls_config.private_key_secret_name, "")
  tls_ca_certificate_secret_name = coalesce(var.tls_config.ca_certificate_secret_name, "")

  # Per-role user network restrictions.
  admin_allowed_networks        = join(",", var.network_access_config.admin_allowed_networks)
  writer_allowed_networks       = join(",", var.network_access_config.writer_allowed_networks)
  default_user_allowed_networks = join(",", var.network_access_config.default_user_allowed_networks)

  # chinit's Azure env contract.
  subscription_id            = var.subscription_id
  resource_group_name        = var.resource_group_name
  key_vault_name             = var.key_vault_name
  managed_identity_client_id = coalesce(var.storage_config.managed_identity_client_id, "")

  vm_names = {
    writers = var.vm_names.writers
    readers = var.vm_names.readers
    keepers = var.vm_names.keepers
    backup  = coalesce(var.vm_names.backup, "")
  }

  # Telemetry (inert when no endpoint).
  log_shipping_agent    = var.telemetry_config.otel_endpoint != null ? "otel" : ""
  log_shipping_endpoint = coalesce(var.telemetry_config.otel_endpoint, "")
  log_shipping_headers  = coalesce(var.telemetry_config.otel_headers, "")

  # THE EXTENSION: this stack's storage.xml + tuned CHI/CHK specs.
  config_files = {
    "configs/storage.xml" = local.storage_xml
  }
  chi_spec    = local.chi_spec
  keeper_spec = local.keeper_spec

}
