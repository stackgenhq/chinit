<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# `chinit` Cluster Module — Azure

Deploy a ClickHouse cluster on Azure VMs, bootstrapped and managed by [chinit](https://github.com/stackgenhq/chinit):

```hcl
module "clickhouse" {
  source = "github.com/stackgenhq/chinit//terraform/azure"

  cluster_name        = "chinitdemo"
  location            = "westus2"
  resource_group_name = azurerm_resource_group.this.name
  subscription_id     = var.subscription_id

  golden_image_id = "/subscriptions/<sub>/galleries/<gallery>/images/clickhouse-node/versions/1.1.0"

  dns_zone_name = "clickhouse.internal"
  subnet_cidr   = "10.42.0.0/24"
}
```

## Provisioned Resources

| Component | Resources Created |
|---|---|
| Writers (write path) | Flexible VM Scale Set (VMSS), lease identity (`writer-N`), LUN 0 metadata disk + LUN 1 cache disk |
| Readers (query path) | Spot VM Scale Set, lease identity (`reader-N`) |
| Keepers (Raft coordination) | 3 fixed VMs with static private IPs and persistent Raft disks (LUN 2) |
| Backup replica | Fixed VM at the `+30` subnet offset running `clickhouse-backup` |
| Configuration bundle | Azure Storage Account + `scripts` container storing role specs, `chi.yaml`, `keeper.yaml`, static XMLs, and bootstrap DDL (SHA-256 verified) |
| Identity & DNS | Azure Table Storage ordinal-lease table and Azure Private DNS Zone |
| Secrets | Azure Key Vault (RBAC mode, randomly generated; specs reference secret names only) |
| Managed Identity & RBAC | User-assigned managed identity granted `Key Vault Secrets User`, `Storage Blob Data Contributor`, `Storage Table Data Contributor`, and `Private DNS Zone Contributor` |

## Keyless Authentication

This module uses Azure Managed Identity and RBAC throughout so no storage account keys or static credentials are stored in Terraform state, user data, or configuration files:

- **`chinit` API calls** (bundle download, Table Storage leases, Key Vault secrets, Private DNS updates, and online disk resizing) authenticate using the VM's managed identity token.
- **ClickHouse's Azure Blob Storage disk** (`azure_blob_storage`) authenticates via `ManagedIdentityCredential` backed by the `Storage Blob Data Contributor` role assignment.

## Prerequisites

1. **A golden Compute Gallery image** built from [`images/azure`](../../images/azure):
   ```bash
   cd images/azure
   packer init .
   packer build -var-file=example.pkrvars.hcl clickhouse.pkr.hcl
   ```
2. **An Azure Resource Group** in your subscription.
3. **Azure credentials** with permissions to manage Network, Storage, Key Vault, Compute, and Role Assignment resources.

## How This Module Works with `chinit`

- **Bundle storage**: Uses the storage account name as `config_bundle.bucket` and the container name as `config_bundle.prefix`.
- **Lease identity**: Claims ordinals from an Azure Storage Table (`readers` / `writers` partitions), matching the DynamoDB lease model on AWS.
- **Azure environment variables**: The `custom_data` wrapper sets `CHINIT_AZURE_SUBSCRIPTION_ID`, `CHINIT_AZURE_RESOURCE_GROUP`, `CHINIT_AZURE_STORAGE_ACCOUNT`, `CHINIT_AZURE_VAULT_NAME`, and `CHINIT_AZURE_VM_NAME` on the `chinit-boot` systemd unit so `chinit.yaml` remains cloud-neutral.

## Disk Layout

| LUN | Filesystem Label | Mount Point | Role |
|---|---|---|---|
| 0 | `ch-metadata` | `/var/lib/clickhouse` | Hot data and metadata (`writer`, `reader`, `backup`) |
| 1 | `ch-cache` | `/var/lib/clickhouse/disks` | Local Blob read cache (`writer`, `reader`) |
| 2 | `ch-keeper` | `/var/lib/clickhouse-keeper` | Persistent Raft log (`keeper`) |

`chinit` resolves disks by filesystem `LABEL` after formatting and uses `/dev/disk/azure/scsi1/lunN` symlinks on first boot.

## Key Outputs

- `node_endpoints` — Private DNS hostnames for writers, readers, keepers, and the backup replica
- `secrets` — Key Vault secret names
- `writer_vmss_name` / `reader_vmss_name` — VM Scale Set names
- `config_files_hash` — SHA-256 digest of the configuration bundle

## Troubleshooting a Node

```bash
az vm run-command invoke --command-id RunShellScript \
  --scripts 'cat /var/lib/chinit/state.json' -g <rg> -n <vm>
```

Or connect via SSH using the generated key (`admin_ssh_public_key`) and inspect `journalctl -u chinit-boot.service` and `/var/lib/chinit/state.json`.
