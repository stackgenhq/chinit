<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Storage Module

This module creates Azure Blob Storage resources for ClickHouse data storage.

## Purpose

The storage module implements the "separated compute and storage" architecture by creating Azure Blob Storage where ClickHouse stores all its data. This enables:
- Data persistence independent of compute nodes
- Zero-copy replication
- Cost-effective scalable storage
- Data durability and availability

## Why This Module Exists

**Why**: ClickHouse requires persistent storage for data. In the separated architecture, data is stored in Azure Blob Storage rather than on local node disks. This allows data to persist across pod restarts, enables efficient replication, and provides cost benefits by separating storage costs from compute costs.

**Without it**: ClickHouse cannot store data. The storage configuration in ClickHouse will fail validation, pods will not start properly, or data will be lost on pod restarts. The entire data persistence layer is missing, making the deployment non-functional.

## Resources Created

### Storage Account (`azurerm_storage_account.main`)
- **Why**: Provides the Azure Blob Storage backend for ClickHouse data
- **Without it**: No storage backend exists, ClickHouse cannot persist data

### Blob Container (`azurerm_storage_container.clickhouse_data`)
- **Why**: Logical container within storage account where ClickHouse stores data
- **Without it**: ClickHouse cannot write to blob storage, causing write failures

### Storage Account Data Source (`data.azurerm_storage_account.main`)
- **Why**: Retrieves storage account access key for ClickHouse authentication
- **Without it**: ClickHouse cannot authenticate to blob storage, causing access failures

## Inputs

See `variables.tf` for complete list of input variables.

Key variables:
- `storage_account_name`: Name of storage account (auto-generated if null)
- `storage_container_name`: Name of blob container
- `storage_account_tier`: Storage tier (Standard/Premium)
- `storage_account_replication_type`: Replication type (LRS/GRS/ZRS)
- `storage_access_tier`: Blob access tier (Hot/Cool/Archive)

## Outputs

See `outputs.tf` for complete list of outputs.

Key outputs:
- `storage_account_name`: Name of created storage account
- `storage_account_key`: Primary access key (sensitive)
- `storage_account_primary_endpoint`: Primary blob endpoint URL
- `storage_container_name`: Name of blob container

## Dependencies

- Azure Resource Group (created in root module)
- Azure subscription with storage account creation permissions

## Usage Example

```hcl
module "storage" {
  source = "./modules/storage"

  resource_group_name      = "rg-clickhouse"
  location                 = "eastus"
  storage_account_name     = null  # Auto-generated
  storage_container_name   = "clickhouse-data"
  storage_account_tier     = "Standard"
  storage_account_replication_type = "LRS"
}
```

## Storage Account Naming

- If `storage_account_name` is provided, it is used (must be globally unique)
- If `storage_account_name` is null, a name is auto-generated from resource group name + random suffix
- Storage account names must be 3-24 characters, lowercase alphanumeric only
- Auto-generated names are truncated to 24 characters to meet Azure requirements

## Security

- Container access type is set to "private" to prevent public access
- Storage account key is marked as sensitive in outputs
- TLS 1.2 minimum is enforced
- Delete retention policies are configured (7 days) for data protection

## Cost Considerations

- Storage costs depend on data volume and access tier
- API transaction costs (PUT, GET, LIST) can be significant with frequent merges
- Consider using Cool tier for infrequently accessed data
- Monitor transaction costs in Azure Cost Management
