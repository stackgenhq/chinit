# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# chinit contract module (Azure) — the ONLY renderer of the chinit interface
# ============================================================================
# The Azure mirror of ../aws: renders the role specs, computes the
# SHA-gated bundle digest, uploads the bundle to Blob Storage, and
# renders the per-role custom_data wrappers (exporting the
# CHINIT_AZURE_* env contract). The caller owns the cloud resources.
# ============================================================================

# ----------------------------------------------------------------------------
# Inputs: cluster identity
# ----------------------------------------------------------------------------

variable "cluster_name" {
  description = "ClickHouse cluster name (rendered into the chinit specs, the CHI spec, DNS names)."
  type        = string
}

variable "region" {
  description = "Azure region (e.g. westus2)."
  type        = string
}

variable "dns_zone_name" {
  description = "Private DNS zone name for node discovery (keeper FQDNs render from it)."
  type        = string
}

variable "timezone" {
  description = "Node timezone (written into the server's EnvironmentFile as CLICKHOUSE_TZ; empty = skip)."
  type        = string
  default     = ""
}

variable "subscription_id" {
  description = "The subscription id (chinit's ARM scope env: CHINIT_AZURE_SUBSCRIPTION_ID)."
  type        = string
}

variable "resource_group_name" {
  description = "The resource group (chinit's ARM scope env: CHINIT_AZURE_RESOURCE_GROUP)."
  type        = string
}

# ----------------------------------------------------------------------------
# Inputs: identity ceilings (VMSS maxima — never live counts)
# ----------------------------------------------------------------------------

variable "writer_max" {
  description = "Writer ordinal claim ceiling = the writers VMSS max."
  type        = number
  default     = 2

  validation {
    condition     = var.writer_max >= 1
    error_message = "writer_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

# ----------------------------------------------------------------------------
# Vertical autoscaling (the chinit `autoscale` coordinator's inputs;
# see the AWS module's variable block for the spec-rendered contract)
# ----------------------------------------------------------------------------

variable "autoscaling_enabled" {
  description = "Render the autoscaling section (ladders + fleet identity) into the role specs. false = the section renders empty and the chinit autoscale timer no-ops."
  type        = bool
  default     = false
}

variable "keeper_type_ladder" {
  description = "Ascending keeper instance types for the vertical ladder. Empty = keepers do not vertically scale."
  type        = list(string)
  default     = []
}

variable "keeper_min_type" {
  description = "Keeper ladder floor."
  type        = string
  default     = ""
}

variable "keeper_max_type" {
  description = "Keeper ladder ceiling (the runaway cap)."
  type        = string
  default     = ""
}

variable "writer_type_ladder" {
  description = "Ascending writer instance types for the vertical ladder. Empty = writers do not vertically scale."
  type        = list(string)
  default     = []
}

variable "writer_min_type" {
  description = "Writer ladder floor."
  type        = string
  default     = ""
}

variable "writer_max_type" {
  description = "Writer ladder ceiling."
  type        = string
  default     = ""
}

variable "writers_asg_name" {
  description = "The writers scale-set name the ordinal-0 holder steps (the coordinator's fleet identity)."
  type        = string
  default     = ""
}

variable "writer_hostnames" {
  description = "Reserved writer ordinal slots ([writer-0..writer-{writer_max-1}]) — the static-path compat field the spec renders as writer_nodes (length must equal writer_max; live membership is lease-driven)."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.writer_hostnames) == 0 || length(var.writer_hostnames) == var.writer_max
    error_message = "writer_hostnames must be empty or exactly writer_max entries (it is the reserved slot ceiling, not a live count)."
  }
}

variable "reader_max" {
  description = "Reader ordinal claim ceiling = the readers VMSS max."
  type        = number
  default     = 6

  validation {
    condition     = var.reader_max >= 1
    error_message = "reader_max must be >= 1 (lease identity requires a claim ceiling)."
  }
}

variable "reader_count" {
  description = "Reader baseline (render-time inventory; the claim ceiling is reader_max)."
  type        = number
  default     = 2
}

variable "keeper_hostnames" {
  description = "The keeper (Raft) inventory — keeper-N names, in order. Every role's spec carries the list."
  type        = list(string)
}

variable "enable_backup" {
  description = "The cluster has a dedicated backup replica (backup-0): every spec carries its identity, and the backup's own spec renders."
  type        = bool
  default     = true
}

# ----------------------------------------------------------------------------
# Inputs: cloud resources the specs point at (ids — never values)
# ----------------------------------------------------------------------------

variable "storage_account_name" {
  description = "The storage account holding the bundle container, the data container and the node-claims Table (the spec's config_bundle.bucket IS the account name on Azure)."
  type        = string
}

variable "scripts_container_name" {
  description = "The bundle container (the spec's config_bundle.prefix IS the container name on Azure)."
  type        = string
}

variable "data_container_name" {
  description = "The Blob container holding ClickHouse table data (storage.xml's container_name)."
  type        = string
}

variable "data_prefix" {
  description = "Blob prefix inside the data container for table data."
  type        = string
  default     = "data"
}

variable "backup_storage_account_name" {
  description = "The storage account for clickhouse-backup (empty = backups disabled)."
  type        = string
  default     = ""
}

variable "backup_container_name" {
  description = "The backup container (the spec's backup.prefix is the container name on Azure)."
  type        = string
  default     = "backups"
}

variable "backup_retention_days" {
  description = "clickhouse-backup remote retention (days)."
  type        = number
  default     = 7
}

variable "backup_compression_format" {
  description = "clickhouse-backup archive format."
  type        = string
  default     = "tar"
}

variable "claimer_table_name" {
  description = "Table Storage ordinal-lease table name (writer-N/reader-N claims; the caller creates the table and passes its name)."
  type        = string
  default     = ""
}

variable "subnet_cidr" {
  description = "The nodes subnet CIDR (renders into the spec's network section)."
  type        = string
}

variable "admin_allowed_networks" {
  description = "Comma-separated CIDRs the admin user may connect from (empty = subnet only)."
  type        = string
  default     = ""
}

variable "writer_allowed_networks" {
  description = "Comma-separated CIDRs the writer user may connect from (empty = subnet only)."
  type        = string
  default     = ""
}

variable "default_user_allowed_networks" {
  description = "Comma-separated CIDRs the default user may connect from (empty = subnet only)."
  type        = string
  default     = ""
}

# ----------------------------------------------------------------------------
# Inputs: secret NAMES (never values — resolved over the managed identity)
# ----------------------------------------------------------------------------

variable "default_password_secret_name" {
  description = "Key Vault secret name for the default (read-only) user password."
  type        = string
}

variable "admin_password_secret_name" {
  description = "Key Vault secret name for the admin user password."
  type        = string
}

variable "writer_password_secret_name" {
  description = "Key Vault secret name for the writer user password."
  type        = string
}

variable "tls_certificate_secret_name" {
  description = "Key Vault secret name for the TLS certificate chain (empty = self-signed interserver mode)."
  type        = string
  default     = ""
}

variable "tls_private_key_secret_name" {
  description = "Key Vault secret name for the TLS private key (empty = self-signed interserver mode)."
  type        = string
  default     = ""
}

variable "tls_ca_certificate_secret_name" {
  description = "Key Vault secret name for the issuer CA (empty = self-signed interserver mode)."
  type        = string
  default     = ""
}

variable "key_vault_name" {
  description = "The Key Vault name (chinit's env pin CHINIT_AZURE_VAULT_NAME — the spec stays provider-neutral)."
  type        = string
}

# ----------------------------------------------------------------------------
# Inputs: disks (labels — mount by LABEL; LUN on first boot)
# ----------------------------------------------------------------------------

variable "metadata_disk_label" {
  description = "Filesystem label of the metadata (hot-tier) disk."
  type        = string
  default     = "ch-metadata"
}

variable "cache_disk_label" {
  description = "Filesystem label of the Blob read-cache disk."
  type        = string
  default     = "ch-cache"
}

variable "keeper_disk_label" {
  description = "Filesystem label of the keeper raft-log disk."
  type        = string
  default     = "ch-keeper"
}

variable "cache_disk_size_gb" {
  description = "Provisioned cache-disk size (GB). storage.xml's cache <max_size> derives from it at 0.9x and must not exceed the volume."
  type        = number
  default     = 256
}

# ----------------------------------------------------------------------------
# Inputs: log shipping (inert when the endpoint is empty)
# ----------------------------------------------------------------------------


# ----------------------------------------------------------------------------
# Inputs: ClickHouse logging configuration
# ----------------------------------------------------------------------------

variable "ch_log_level" {
  description = "External sink <level> in server-logger.xml (rotating file shipped by the log agent). Default `warning`: keeps remote log ingestion focused on warnings and errors while verbose detail is preserved in system.text_log via ch_text_log_*. Poco levels apply: trace|debug|information|warning|error|critical."
  type        = string
  default     = "warning"
}

variable "ch_text_log_level" {
  description = "system.text_log <level> in server-text-log.xml — the in-cluster verbose log (ZSTD-compressed, TTL-dropped). Sits at or above ch_log_level (the root logger rises to the max of both, Loggers.cpp). Set `warning` to keep only warnings in the table too."
  type        = string
  default     = "information"
}

variable "ch_text_log_ttl_days" {
  description = "system.text_log TTL days (engine `TTL event_date + INTERVAL N DAY DELETE`). 0 = forever (never recommended — a log table without a TTL is unbounded cost)."
  type        = number
  default     = 7

  validation {
    condition     = var.ch_text_log_ttl_days >= 0
    error_message = "ch_text_log_ttl_days must be >= 0 (0 keeps rows forever)."
  }
}
variable "log_shipping_agent" {
  description = "Remote log agent: otel | fluentbit."
  type        = string
  default     = "otel"
}

variable "log_shipping_endpoint" {
  description = "OTLP/HTTP endpoint (empty = remote shipping disabled)."
  type        = string
  default     = ""
}

variable "log_shipping_headers" {
  description = "Comma-separated k=v header pairs sent with each remote export."
  type        = string
  default     = ""
}

variable "fluentbit_output" {
  description = "Fluent Bit output flavor when agent=fluentbit: otlp | http."
  type        = string
  default     = "otlp"
}

# ----------------------------------------------------------------------------
# Inputs: the fleet policy (disruption budget + lifecycle hooks)
# ----------------------------------------------------------------------------

variable "min_healthy_writers" {
  description = "The voluntary-disruption floor for writers (the PDB equivalent): a refresh/rollout waits when fewer healthy writers would remain. 0 = the safe default (1)."
  type        = number
  default     = 1
}

variable "min_healthy_readers" {
  description = "The voluntary-disruption floor for readers. 0 = readers are disposable (the scale-set owns their horizontal scaling)."
  type        = number
  default     = 0
}

variable "lifecycle_hooks" {
  description = "Lifecycle hook commands at the boot/drain boundaries (the operator's spec.reconcile.hooks parity). List of {event, name, command, timeout_seconds, critical}."
  type = list(object({
    event           = string
    name            = string
    command         = string
    timeout_seconds = optional(number, 0)
    critical        = optional(bool, false)
  }))
  default = []
}

# ----------------------------------------------------------------------------
# EXTENSION HOOKS — how consuming stacks extend the bundle
# ----------------------------------------------------------------------------

variable "config_files" {
  description = "EXTRA bundle members beyond the module's static XMLs (extension hook). Map of bundle key → content: each entry uploads to the scripts container and joins the SHA gate digest. KEYS MUST BE AMONG chinit's hashed bundle members (configs/network.xml, configs/ssl.xml, configs/storage.xml, configs/bootstrap-ddl.sql) — the gate digest chinit recomputes covers EXACTLY that closed list plus chi.yaml, so an unknown key would upload but silently desync the digest and Fatal every boot. Override the module's defaults by reusing their keys."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([
      for k in keys(var.config_files) :
      contains(["configs/network.xml", "configs/ssl.xml", "configs/storage.xml", "configs/bootstrap-ddl.sql"], k)
    ])
    error_message = "config_files keys must be chinit's hashed bundle members: configs/network.xml, configs/ssl.xml, configs/storage.xml, configs/bootstrap-ddl.sql (chi.yaml is pinned separately via chi_spec)."
  }
}

variable "chi_spec" {
  description = "Caller-rendered CHI (cluster) spec — the extension hook for the consumer's own settings/profiles/files. When set it REPLACES the module's default topology render; keep the invariants documented in the README (named shard, replica_count ceiling arithmetic, keeper ensemble)."
  type        = string
  default     = null
}

variable "keeper_spec" {
  description = "Caller-rendered CHK (keeper) spec — same contract as chi_spec for the keeper fleet."
  type        = string
  default     = null
}

variable "wrapper_extra" {
  description = "Extra shell appended to every role's custom_data wrapper BEFORE `chinit boot` runs (extension hook: stage artifacts, run stack-specific pre-flight). Must not exec a replacement boot; keep it small."
  type        = string
  default     = ""
}

variable "managed_identity_client_id" {
  description = "The user-assigned managed identity's client id (az login --identity in the wrapper needs it when multiple identities are attached)."
  type        = string
  default     = ""
}

variable "vm_names" {
  description = "Per-role VM/VMSS resource names for chinit's ARM scope env (CHINIT_AZURE_VM_NAME: the volume grower + Private DNS resolve this VM's own resource id)."
  type = object({
    writers = string
    readers = string
    keepers = string
    backup  = optional(string, "")
  })
}

variable "tags" {
  description = "Tags applied to the bundle blobs."
  type        = map(string)
  default     = {}
}

# ----------------------------------------------------------------------------
# The bundle digest — the contract with chinit's Blob FetchBundle
# ----------------------------------------------------------------------------

locals {
  static_config_files = {
    "configs/network.xml" = file("${path.module}/templates/network.xml")
    "configs/ssl.xml"     = file("${path.module}/templates/ssl.xml")
    "configs/storage.xml" = templatefile("${path.module}/templates/storage.xml", {
      storage_account_url  = "https://${var.storage_account_name}.blob.core.windows.net"
      container_name       = var.data_container_name
      cache_max_size_bytes = floor(var.cache_disk_size_gb * 0.9) * 1024 * 1024 * 1024
    })
    "configs/bootstrap-ddl.sql" = file("${path.module}/templates/bootstrap-ddl.sql")
  }

  config_files_all = merge(local.static_config_files, var.config_files)

  default_chi_spec = templatefile("${path.module}/templates/chi.yaml.tftpl", {
    cluster_name                 = var.cluster_name
    replica_count                = var.writer_max + var.reader_max + (var.enable_backup ? 1 : 0)
    keepers                      = [for h in var.keeper_hostnames : "${h}.${var.dns_zone_name}"]
    default_password_secret_name = var.default_password_secret_name
    admin_password_secret_name   = var.admin_password_secret_name
    writer_password_secret_name  = var.writer_password_secret_name
    log_level                    = var.ch_log_level
    text_log_level               = var.ch_text_log_level
    text_log_ttl_line            = var.ch_text_log_ttl_days > 0 ? "event_date + INTERVAL ${var.ch_text_log_ttl_days} DAY DELETE" : ""
  })

  chi_spec = var.chi_spec != null ? var.chi_spec : local.default_chi_spec

  default_keeper_spec = templatefile("${path.module}/templates/keeper.yaml.tftpl", {
    cluster_name = var.cluster_name
    keeper_count = length(var.keeper_hostnames)
  })

  keeper_spec = var.keeper_spec != null ? var.keeper_spec : local.default_keeper_spec

  config_files_hash = sha256(jsonencode(merge(local.config_files_all, {
    "chi.yaml" = local.chi_spec
  })))

  # hooks_yaml: the lifecycle hook list rendered as the spec's
  # hooks.items YAML (an empty list renders the empty section).
  hooks_yaml = join("\n", [for h in var.lifecycle_hooks : format(
    "    - event: %s\n      name: %s\n      command: %s\n      timeout_seconds: %d\n      critical: %t",
    h.event, h.name, h.command, h.timeout_seconds, h.critical
  )])

  chinit_spec_common = {
    cluster_name  = var.cluster_name
    dns_zone_name = var.dns_zone_name
    region        = var.region
    # The fleet policy: the disruption floors + the lifecycle hooks,
    # rendered into every role's spec (the boundaries run everywhere).
    min_healthy_writers = var.min_healthy_writers
    min_healthy_readers = var.min_healthy_readers
    hooks_yaml          = local.hooks_yaml
    bundle_bucket       = var.storage_account_name
    bundle_prefix       = var.scripts_container_name
    config_hash         = local.config_files_hash
    # Azure's blob disk needs no endpoint env (the account URL is in
    # storage.xml; auth is the managed identity): empty is correct.
    s3_data_bucket   = ""
    timezone         = var.timezone
    keeper_hostnames = var.keeper_hostnames
    # Static-writer compat fields (writer_nodes/writer_count): lease
    # binaries ignore them in slot arithmetic; pre-lease binaries on
    # the static path still read them.
    writer_hostnames               = length(var.writer_hostnames) > 0 ? var.writer_hostnames : [for i in range(var.writer_max) : "writer-${i}"]
    writer_count                   = var.writer_max
    writer_nodes_list              = join(", ", [for n in(length(var.writer_hostnames) > 0 ? var.writer_hostnames : [for i in range(var.writer_max) : "writer-${i}"]) : "\"${n}\""])
    keeper_nodes_list              = join(", ", [for n in var.keeper_hostnames : "\"${n}\""])
    keeper_ladder_list             = var.autoscaling_enabled ? join(", ", [for t in var.keeper_type_ladder : "\"${t}\""]) : ""
    keeper_min_type                = var.autoscaling_enabled ? var.keeper_min_type : ""
    keeper_max_type                = var.autoscaling_enabled ? var.keeper_max_type : ""
    writer_ladder_list             = var.autoscaling_enabled ? join(", ", [for t in var.writer_type_ladder : "\"${t}\""]) : ""
    writer_min_type                = var.autoscaling_enabled ? var.writer_min_type : ""
    writer_max_type                = var.autoscaling_enabled ? var.writer_max_type : ""
    writers_asg_name               = var.autoscaling_enabled ? var.writers_asg_name : ""
    metadata_disk_label            = var.metadata_disk_label
    cache_disk_label               = var.cache_disk_label
    keeper_disk_label              = var.keeper_disk_label
    claimer_table_name             = var.claimer_table_name
    writer_max                     = var.writer_max
    reader_max                     = var.reader_max
    reader_count                   = var.reader_count
    default_password_secret_name   = var.default_password_secret_name
    admin_password_secret_name     = var.admin_password_secret_name
    writer_password_secret_name    = var.writer_password_secret_name
    tls_certificate_secret_name    = var.tls_certificate_secret_name
    tls_private_key_secret_name    = var.tls_private_key_secret_name
    tls_ca_certificate_secret_name = var.tls_ca_certificate_secret_name
    subnet_cidr                    = var.subnet_cidr
    # Azure has no NLB-subnet equivalent in front of these nodes by
    # default; an empty nlb_subnet_cidr renders the spec field inert.
    nlb_subnet_cidr           = ""
    admin_networks_list       = join(", ", [for c in compact([for c in split(",", var.admin_allowed_networks) : trimspace(c)]) : "\"${c}\""])
    writer_networks_list      = join(", ", [for c in compact([for c in split(",", var.writer_allowed_networks) : trimspace(c)]) : "\"${c}\""])
    default_networks_list     = join(", ", [for c in compact([for c in split(",", var.default_user_allowed_networks) : trimspace(c)]) : "\"${c}\""])
    backup_bucket             = var.backup_storage_account_name
    backup_prefix             = var.backup_container_name
    backup_retention_days     = var.backup_retention_days
    backup_compression_format = var.backup_compression_format
    is_backup_replica         = "false"
    backup_replica_hostname   = var.enable_backup ? "backup-0" : ""
    log_shipping_agent        = var.log_shipping_agent
    log_shipping_endpoint     = var.log_shipping_endpoint
    log_shipping_headers      = var.log_shipping_headers
    fluentbit_output          = var.fluentbit_output
  }

  chinit_specs = {
    writer = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role          = "writer"
      identity_mode = "lease"
    }))
    reader = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role          = "reader"
      identity_mode = "lease"
    }))
    keeper = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role          = "keeper"
      identity_mode = "static"
    }))
    backup = templatefile("${path.module}/templates/chinit.yaml.tftpl", merge(local.chinit_spec_common, {
      role              = "backup"
      identity_mode     = "static"
      is_backup_replica = "true"
    }))
  }

  uploaded_specs = var.enable_backup ? local.chinit_specs : {
    for role, spec in local.chinit_specs : role => spec if role != "backup"
  }

  # Per-role custom_data wrappers. The vm_name_mode routes the ARM-scope
  # VM name: keepers are FIXED VMs (derive from the platform hostname);
  # VMSS members use the scale-set resource name.
  bootstrap_script = {
    for role, _spec in local.chinit_specs : role => templatefile("${path.module}/templates/user-data.sh.tftpl", merge(local.chinit_env, {
      chinit_spec_key            = "chinit-${role}.yaml"
      container_name             = var.scripts_container_name
      storage_account            = var.storage_account_name
      managed_identity_client_id = var.managed_identity_client_id
      vm_name                    = role == "keeper" ? var.vm_names.keepers : (role == "backup" ? var.vm_names.backup : (role == "writer" ? var.vm_names.writers : var.vm_names.readers))
      vm_name_mode               = role == "keeper" ? "keeper" : "vmss"
      wrapper_extra              = var.wrapper_extra
    }))
  }

  chinit_env = {
    subscription_id     = var.subscription_id
    resource_group_name = var.resource_group_name
    key_vault_name      = var.key_vault_name
  }

  common_tags = merge(var.tags, { cluster = var.cluster_name })
}

# ----------------------------------------------------------------------------
# Bundle uploads — the blobs chinit's Blob FetchBundle downloads
# ----------------------------------------------------------------------------

resource "azurerm_storage_blob" "bundle_configs" {
  for_each = local.config_files_all

  name                 = each.key
  storage_container_id = "/subscriptions/${var.subscription_id}/resourceGroups/${var.resource_group_name}/providers/Microsoft.Storage/storageAccounts/${var.storage_account_name}/blobServices/default/containers/${var.scripts_container_name}"
  type                 = "Block"
  content_type         = can(regex("\\.xml$", each.key)) ? "application/xml" : (can(regex("\\.sql$", each.key)) ? "application/sql" : "text/plain")
  source_content       = each.value
}

resource "azurerm_storage_blob" "chi_spec" {
  name                 = "chi.yaml"
  storage_container_id = "/subscriptions/${var.subscription_id}/resourceGroups/${var.resource_group_name}/providers/Microsoft.Storage/storageAccounts/${var.storage_account_name}/blobServices/default/containers/${var.scripts_container_name}"
  type                 = "Block"
  content_type         = "application/yaml"
  source_content       = local.chi_spec
}

resource "azurerm_storage_blob" "keeper_spec" {
  name                 = "keeper.yaml"
  storage_container_id = "/subscriptions/${var.subscription_id}/resourceGroups/${var.resource_group_name}/providers/Microsoft.Storage/storageAccounts/${var.storage_account_name}/blobServices/default/containers/${var.scripts_container_name}"
  type                 = "Block"
  content_type         = "application/yaml"
  source_content       = local.keeper_spec
}

resource "azurerm_storage_blob" "chinit_specs" {
  for_each = local.uploaded_specs

  name                 = "chinit-${each.key}.yaml"
  storage_container_id = "/subscriptions/${var.subscription_id}/resourceGroups/${var.resource_group_name}/providers/Microsoft.Storage/storageAccounts/${var.storage_account_name}/blobServices/default/containers/${var.scripts_container_name}"
  type                 = "Block"
  content_type         = "application/yaml"
  source_content       = each.value
}

# ----------------------------------------------------------------------------
# Outputs
# ----------------------------------------------------------------------------

output "bootstrap_script" {
  description = "Role → rendered custom_data wrapper (seeds the role's spec, exports the CHINIT_AZURE_* env, execs chinit boot). Inject into each role's VMSS custom_data / VM user_data."
  value       = local.bootstrap_script
}

output "config_files_hash" {
  description = "The SHA256 gate digest (chinit recomputes it at boot)."
  value       = local.config_files_hash
}

output "chinit_specs" {
  description = "Role → rendered chinit node spec (uploaded to the bundle)."
  value       = local.chinit_specs
  sensitive   = true
}

output "bundle_keys" {
  description = "Every bundle key the module uploaded (audit surface)."
  value       = [for k, _ in local.config_files_all : k]
}

output "keeper_spec_hash" {
  description = "sha256 of the rendered CHK (keeper) spec. keeper.yaml is deliberately EXCLUDED from the bundle digest (role-independent digest), so consumers that roll the keeper fleet on a CHK edit key on THIS hash instead."
  value       = sha256(local.keeper_spec)
}
