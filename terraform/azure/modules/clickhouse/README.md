<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# ClickHouse Module — an EXTENSION of the open-source chinit contract module

This module no longer renders the chinit node specs, the SHA-gated
bundle, or the custom_data wrappers itself. It consumes the
**open-source contract module**
([`stackgenhq/chinit//terraform/modules/chinit/azure`](https://github.com/stackgenhq/chinit/tree/main/terraform/modules/chinit/azure))
and extends it with THIS stack's business layer:

| Owned here (the extension) | Owned by the open contract module |
|---|---|
| The tuned CHI/CHK specs (operator-parity settings, profiles, logs) | The role specs (`chinit-<role>.yaml`) |
| THIS stack's `storage.xml` (the Azure blob cold tier) | The static XML trio + bootstrap DDL |
| The SRE troubleshooting script gallery | The SHA-gated bundle digest + uploads |
| The scripts container | The custom_data wrappers (incl. the `CHINIT_AZURE_*` env export) |

The chinit interface has exactly one implementation, in the open repo;
this module feeds it through the `chi_spec`, `keeper_spec`, and
`config_files` extension hooks. A chinit spec/bundle fix lands in the
open module and flows here on the next `tofu init`.

---

# ClickHouse Module

This module generates cloud-init and bootstrap scripts for deploying ClickHouse database clusters on Azure Virtual Machine Scale Sets (VMSS).

## Purpose

The ClickHouse module generates:
- Bootstrap scripts that download cloud-init and XML configs from blob storage
- Cloud-init scripts that install and configure ClickHouse on VMs
- XML configuration files for ClickHouse server settings

## Why This Module Exists

**Why**: This module provides the configuration generation for ClickHouse deployment. It creates bootstrap scripts (small enough for Azure's custom_data limit) that download the full cloud-init scripts and XML configurations from blob storage. This separation enables large, maintainable configurations without hitting Azure's size limits.

**Without it**: VMs would be created without ClickHouse installed or configured. The 87,380 character limit on Azure custom_data would prevent passing the full configuration directly.

## Architecture

```
┌─────────────────────┐     ┌──────────────────────────────────────────┐
│   Terraform Apply   │────▶│   Azure Blob Storage (scripts container) │
│                     │     │   ├── cloud-init-writer-<hash>.sh        │
└─────────────────────┘     │   ├── cloud-init-reader-<hash>.sh        │
                            │   ├── cloud-init-keeper-<hash>.sh        │
                            │   └── configs/                           │
                            │       ├── prometheus.xml                 │
                            │       ├── logger.xml                     │
                            │       ├── query-log.xml                  │
                             │       └── settings.xml                   │
                            └──────────────────┬───────────────────────┘
                                               │
                                               │ Managed Identity (at VM boot)
                                               ▼
                            ┌──────────────────────────────────────────┐
                            │   bootstrap.sh (passed via custom_data)  │
                            │   1. Downloads cloud-init.sh             │
                            │   2. Downloads XML configs               │
                            │   3. Executes cloud-init.sh              │
                            └──────────────────┬───────────────────────┘
                                               │
                                               ▼
                            ┌──────────────────────────────────────────┐
                            │   cloud-init.sh                          │
                            │   1. Installs ClickHouse                 │
                            │   2. Copies XML configs to config.d/     │
                            │   3. Configures storage, cluster, etc.   │
                            │   4. Starts ClickHouse service           │
                            └──────────────────────────────────────────┘
```

## Resources Created

### Storage Container for Scripts (`azurerm_storage_container.scripts`)
- **Why**: Stores cloud-init scripts and XML configs that exceed Azure's custom_data limit
- **Without it**: Cannot pass configurations larger than 87,380 characters to VMs

### Cloud-init Script Blobs (`azurerm_storage_blob.{writer,reader,keeper}_cloud_init`)
- **Why**: Full installation and configuration scripts for each node type
- **Without it**: VMs have no instructions for installing ClickHouse

### XML Config Blobs (`azurerm_storage_blob.config_files`)
- **Why**: Externalized ClickHouse XML configurations for easier maintenance
- **Without it**: All XML configs must be embedded in cloud-init (harder to maintain)

### Bootstrap Scripts (`writer_bootstrap`, `reader_bootstrap`, etc.)
- **Why**: Small scripts (fit in Azure custom_data) that download full configs using Managed Identity
- **Without it**: VMs cannot download configurations from blob storage

## Node Types

### Writer Nodes
- Primary nodes for data ingestion
- Full read/write access to data
- Participate in cluster replication

### Reader Nodes
- Optimized for analytical queries
- Read replicas with local caching
- Connect to same cluster for data access

### Keeper Nodes
- ClickHouse Keeper for coordination
- Manages distributed DDL and replication metadata
- 3 nodes for high availability quorum

## Templates

### `templates/bootstrap.sh`
Small script (~3KB) that fits in Azure's custom_data limit:
- Downloads cloud-init script from blob storage
- Downloads XML configuration files
- Executes cloud-init script

### `templates/cloud-init.sh`
Full installation and configuration script (~80KB):
- Installs ClickHouse from official repository
- Mounts data disks
- Copies downloaded XML configs
- Configures storage, networking, TLS, cluster, users
- Registers DNS hostname
- Starts ClickHouse service
- Installs monitoring agents (OTEL, OpsVerse)

### `templates/configs/`
Externalized XML configurations:

| File | Description |
|------|-------------|
| `prometheus.xml` | Prometheus metrics endpoint (port 9363) |
| `logger.xml` | JSON logging for OTEL collector |
| `query-log.xml` | Query and thread logging settings |
| `settings.xml` | Server settings, MergeTree config, compression |

## Inputs

See `variables.tf` for complete list. Key variables:

| Variable | Description |
|----------|-------------|
| `clickhouse_version` | ClickHouse version to install |
| `storage_account_name` | Azure Storage account for data |
| `storage_container_name` | Blob container for ClickHouse data |
| `use_managed_identity` | Use Managed Identity for storage auth (always true) |
| `writer_count` | Number of writer nodes |
| `reader_count` | Number of reader nodes |
| `keeper_count` | Number of keeper nodes |
| `cluster_name` | Name of the ClickHouse cluster |
| `dns_zone_name` | Azure Private DNS zone for node discovery |

## Outputs

| Output | Description |
|--------|-------------|
| `writer_bootstrap` | Bootstrap script for writer VMSS |
| `reader_bootstrap` | Bootstrap script for reader VMSS |
| `keeper_bootstrap` | Bootstrap script for keeper VMSS |
| `backup_replica_bootstrap` | Bootstrap script for backup replica VM |
| `config_files_hash` | Hash of config files to trigger VMSS replacement |

## Dependencies

- Storage account (from `storage` module) - provides blob storage for data
- Key Vault (from `keyvault` module) - provides TLS certificates and secrets
- DNS zone (from `dns` module) - provides private DNS for node discovery

## Usage Example

```hcl
module "clickhouse" {
  source = "./modules/clickhouse"

  clickhouse_version     = "25.12.4.35"
  storage_account_name   = module.storage.storage_account_name
  storage_account_id     = module.storage.storage_account_id
  storage_container_name = "clickhouse-data"
  # Managed Identity is always enabled for storage auth
  
  cluster_name        = "clickhouse"
  dns_zone_name       = module.dns.dns_zone_name
  resource_group_name = azurerm_resource_group.main.name
  
  writer_count = 2
  reader_count = 2
  keeper_count = 3
  
  key_vault_name = module.keyvault.key_vault_name
  key_vault_uri  = module.keyvault.key_vault_uri
  # TLS is mandatory and always enabled
}

# Pass bootstrap scripts to VMSS module
module "vmss" {
  source = "./modules/vmss"
  
  writer_custom_data = module.clickhouse.writer_bootstrap
  reader_custom_data = module.clickhouse.reader_bootstrap
  keeper_custom_data = module.clickhouse.keeper_bootstrap
  # ...
}
```

## Adding New XML Configurations

1. Create the XML file in `templates/configs/`
2. Add the file to `local.config_files` map in `main.tf`
3. Add the filename to `CONFIG_FILES` in `templates/bootstrap.sh`
4. Add copy entry in `install_static_configs()` in `templates/cloud-init.sh`
5. Add verification in the corresponding `configure_*()` function

## Troubleshooting

### Bootstrap Fails to Download Scripts
- Verify VM's Managed Identity has "Storage Blob Data Reader" role on scripts container
- Verify storage account firewall allows access from VNet
- Check VM has network connectivity to blob storage

### XML Config Not Applied
- Check `/var/log/bootstrap.log` for download errors
- Verify configs exist in `/etc/clickhouse-server/config.d/`
- Check file permissions (should be `clickhouse:clickhouse`)

### ClickHouse Fails to Start
- Check `/var/log/clickhouse-server/clickhouse-server.err.log`
- Verify storage account credentials (if using account key)
- Check Managed Identity has Storage Blob Data Contributor role
- Verify disk mounts in `/mnt/data` and `/mnt/metadata`

### Cluster Nodes Not Discovering Each Other
- Verify DNS registration succeeded
- Check private DNS zone has A records for all nodes
- Verify keeper nodes are running and healthy

## Performance Tuning

The `settings.xml` configuration includes optimizations for Azure Blob Storage:

```xml
<merge_tree>
  <!-- Zero-copy replication for shared storage -->
  <allow_s3_zero_copy_replication>1</allow_s3_zero_copy_replication>
  <!-- Larger merge thresholds to reduce API calls -->
  <max_bytes_to_merge_at_max_space_in_pool>161061273600</max_bytes_to_merge_at_max_space_in_pool>
  <max_parts_to_merge_at_once>20</max_parts_to_merge_at_once>
</merge_tree>
```

## Open Standards

This module follows open standards for observability:

- **OpenTelemetry**: OTEL Collector configured for log collection
- **JSON Logging**: Structured logs with OTEL semantic conventions
- **Prometheus Metrics**: Standard `/metrics` endpoint on port 9363
- **Keeper Monitoring**: Standard `mntr` command for quorum health

See `docs/OPEN_STANDARDS_CLUSTER_SETUP.md` for details.
