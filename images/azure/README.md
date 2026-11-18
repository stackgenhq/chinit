<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# ClickHouse Golden Image (Packer) — Azure

This directory contains Packer templates for building pre-baked ClickHouse VM images in an Azure Compute Gallery.

> **Boot procedure**: See [BOOT.md](../BOOT.md) for a walkthrough of what happens from VM launch to `boot-finished` across node roles (`writer`, `reader`, `keeper`, and `backup`).

## Why Golden Images?

Pre-baked images reduce Azure VM Scale Set (VMSS) bootstrap time from **8–12 minutes** to **2–3 minutes** by:
- Pre-installing ClickHouse server, client, and Keeper
- Pre-installing the pinned `chinit` binary and `clickhouse-backup`
- Pre-installing the Azure CLI
- Applying OS-level optimizations at build time

## Prerequisites

1. **Packer** (version 1.9+):
   ```bash
   brew install packer  # macOS
   ```

2. **Azure authentication**:
   ```bash
   az login
   az account set --subscription "your-subscription-id"
   ```

3. **An Azure Compute Gallery** in your target subscription and resource group.

## Building an Image

1. **Initialize Packer plugins**:
   ```bash
   cd images/azure
   packer init .
   ```

2. **Create a variables file**:
   ```bash
   cp example.pkrvars.hcl dev.pkrvars.hcl
   # Edit dev.pkrvars.hcl with your subscription and gallery settings
   ```

3. **Validate the template**:
   ```bash
   packer validate -var-file=dev.pkrvars.hcl clickhouse.pkr.hcl
   ```

4. **Build the image** (~15–20 minutes):
   ```bash
   packer build -var-file=dev.pkrvars.hcl clickhouse.pkr.hcl
   ```

## Using the Image

Pass the resulting Compute Gallery image version ID to the Azure Terraform module:

```hcl
golden_image_id = "/subscriptions/.../galleries/sig_clickhouse/images/clickhouse-node/versions/1.1.0"
```

## Files

| File | Purpose |
|------|---------|
| `clickhouse.pkr.hcl` | Main Packer template |
| `scripts/install-clickhouse.sh` | Package and `chinit` installation script |
| `defaults.pkrvars.hcl` | Default build variables |
| `example.pkrvars.hcl` | Example variables template |
