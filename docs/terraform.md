---
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
title: "Deploy with Terraform"
nav_title: "Deploy with Terraform"
nav_order: 4
permalink: /terraform/
description: >-
  Deploy a complete chinit-bootstrapped ClickHouse cluster on AWS or
  Azure with the turnkey Terraform modules — compute, identity leases,
  Private DNS, secrets, and SHA-256 verified config bundles in one apply.
---

# Deploy a Cluster with Terraform

<p class="lead">The recommended production path: one Terraform module call provisions every cloud resource a <code>chinit</code> cluster needs — no hand-wired IAM policies, lease tables, or DNS zones.</p>

Everything in the [Quickstart]({{ '/quickstart/' | relative_url }}) — the bundle bucket, the lease table, the private DNS zone, the secrets, the instance roles, and the user-data wrappers — is automated by the root-callable modules in [`terraform/`](https://github.com/stackgenhq/chinit/tree/main/terraform).

## AWS

```hcl
module "clickhouse" {
  source = "github.com/stackgenhq/chinit//terraform/aws"

  cluster_name  = "demo"
  region        = "us-west-2"
  golden_ami_id = "ami-…" # built from images/aws, or copied from the public AMI

  dns_zone_name = "clickhouse.internal"
  subnet_cidr   = "10.42.0.0/24"
}
```

## Azure

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

## What Gets Provisioned

| Component | AWS | Azure |
|---|---|---|
| **Writers** (write path) | On-demand Auto Scaling Group, lease identity (`writer-N`), metadata + S3-cache EBS volumes | Flexible VM Scale Set, lease identity (`writer-N`), metadata + cache disks |
| **Readers** (query path) | Spot-capable Auto Scaling Group, lease identity (`reader-N`) | Spot VM Scale Set, lease identity (`reader-N`) |
| **Keepers** (Raft coordination) | 3 fixed EC2 instances, static private IPs, persistent Raft volumes | 3 fixed VMs, static private IPs, persistent Raft disks |
| **Backup replica** | Optional fixed instance running `clickhouse-backup` | Fixed VM running `clickhouse-backup` |
| **Configuration bundle** | S3 bucket with per-role specs, `chi.yaml`, static XMLs, bootstrap DDL (SHA-256 verified) | Storage account + container, same contents |
| **Identity & DNS** | DynamoDB ordinal-lease table (TTL enabled) + Route 53 private zone | Table Storage lease table + Azure Private DNS zone |
| **Secrets** | Secrets Manager (randomly generated; specs hold names only) | Key Vault in RBAC mode (randomly generated) |
| **Access** | Least-privilege instance profile | User-assigned managed identity with scoped RBAC roles |

No static credentials end up in Terraform state, user data, or config files — on Azure everything authenticates via managed identity, on AWS via the instance profile.

## Prerequisites

1. **A golden image** per cloud — build one with [Packer]({{ '/installation/' | relative_url }}) (`images/aws` / `images/azure`), or [copy the public AWS AMI]({{ '/installation/' | relative_url }}) into your account and region.
2. **Cloud credentials** permitted to manage compute, storage, DNS, secrets, and IAM/role assignments.
3. On Azure, an existing **resource group**.

## Customizing with the Contract Modules

If you already manage your own VPCs, buckets, and policies, call the contract modules directly from your own root — they render the per-role `chinit-<role>.yaml` specs, compute the SHA-256 bundle digest, upload the config bundle, and render the user-data wrappers **without creating any infrastructure themselves**:

| Module | Use it to… |
|---|---|
| `terraform/modules/chinit/aws` | Integrate `chinit` into a custom AWS stack |
| `terraform/modules/chinit/azure` | Integrate `chinit` into a custom Azure stack |

Three extension hooks adapt the rendered artifacts to your environment: `config_files` (extra files folded into the verified bundle), `chi_spec` / `keeper_spec` (override the default topology templates), and `wrapper_extra` (extra shell lines in each role's user-data wrapper).

<div class="callout">
  <div class="callout-title">Why the SHA-256 Bundle Contract Matters</div>
  Terraform computes the bundle digest as <code>sha256(jsonencode(merge(config_files, {"chi.yaml" = chi_spec})))</code>, and <code>chinit</code> recomputes the identical digest at boot before touching a single config file. A partially uploaded bundle or an out-of-band S3 edit aborts the boot — instead of silently joining a misconfigured node to your cluster.
</div>

## After the Apply

- **`terraform output node_endpoints`** — private DNS hostnames for writers, readers, keepers, and the backup replica.
- **`terraform output writers_asg_name` / `readers_asg_name`** (AWS) or **`writer_vmss_name` / `reader_vmss_name`** (Azure) — slide desired capacity up or down; `chinit` handles identity claims and cluster joins.
- **`terraform output secrets`** — where the generated admin credentials live.

### Troubleshooting a Node

```bash
# AWS
aws ssm start-session --target <instance-id>
sudo cat /var/lib/chinit/state.json
journalctl -u chinit-boot.service -n 50
systemctl list-timers | grep chinit
```

```bash
# Azure
az vm run-command invoke --command-id RunShellScript \
  --scripts 'cat /var/lib/chinit/state.json' -g <rg> -n <vm>
```

## Go Deeper

- **[terraform/ on GitHub](https://github.com/stackgenhq/chinit/tree/main/terraform)** — full module READMEs, disk layouts, sizing notes, and `examples/cluster/` for each cloud.
- **[Installation & Golden Images]({{ '/installation/' | relative_url }})** — build the golden image the modules consume.
- **[The Spec]({{ '/spec/' | relative_url }})** — every `chinit.yaml` field the modules can render.

The modules also validate offline (`tofu init -backend=false && tofu validate`), so you can check changes in CI without cloud credentials.

## Deploying with Crossplane instead

If you manage infrastructure from the Kubernetes API (GitOps, Argo CD), the [`crossplane/`](https://github.com/stackgenhq/chinit/tree/main/crossplane) tree deploys the same cluster with [Crossplane](https://docs.crossplane.io):

- **Cloud infrastructure as native managed resources** — VPC/VNet, buckets/storage accounts, lease tables, private DNS zones, secrets, IAM/RBAC, ASGs/VMSS, fixed keepers/backup — synchronized by one `ClickHouseCluster` claim.
- **The chinit contract module rendered in-cluster** via [provider-opentofu](https://github.com/upbound/provider-opentofu) `Workspace` resources, so the spec rendering and the SHA-256 bundle digest stay byte-identical to the Terraform deliverables (one renderer, no drift).

A `ClickHouseCluster` claim maps 1:1 onto each module's variables (`writerMax`/`readerMax` are the ASG/VMSS maxima = the claim ceilings, never live counts). See [the Crossplane guide](https://stackgenhq.github.io/chinit/crossplane/) for install, credentials, claims, and scaling.
