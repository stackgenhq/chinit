<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Terraform Modules for `chinit` Clusters

Deploy a `chinit`-bootstrapped ClickHouse cluster on cloud VMs.

## Install (AWS)

```hcl
module "clickhouse" {
  source = "github.com/stackgenhq/chinit//terraform/aws?ref=<release-tag>"

  cluster_name = "demo"
  region       = "us-west-2"
  alert_emails = ["ops@example.com"] # where alarms + security findings go
}
```

```bash
tofu init
tofu apply
tofu output node_endpoints   # private DNS names to connect to
```

That single call provisions a secure, HA-shaped fleet: on-demand writers
ASG, Spot-diversified readers ASG, 3 keepers, a backup replica, S3-backed
tiered storage, Secrets Manager passwords with rotation, GuardDuty +
Security Hub, and CloudWatch alarms with scaling guidance — private-DNS
only, no load balancers, no public exposure, no static credentials. See
[`aws/README.md`](./aws/README.md) for every default and knob.

| Cloud | Module | Prerequisites |
|---|---|---|
| AWS | [`aws/`](./aws) | A golden AMI (built from [`images/aws`](../images/aws), or auto-resolved from the public AMI in us-west-2) + an ACME provider block in your root |
| Azure | [`azure/`](./azure) | A golden Compute Gallery image (built from [`images/azure`](../images/azure)) and an Azure Resource Group |

## Contract Modules (`<cloud>/modules/chinit`)

The interface between Terraform and `chinit` lives in two contract modules:

```
aws/modules/chinit      azure/modules/chinit
```

Each contract module renders the per-role `chinit-<role>.yaml` specs, computes the SHA-256 bundle digest, uploads the configuration bundle to object storage, and renders the per-role user-data wrappers. **It creates no cloud infrastructure itself** — the caller owns the storage buckets/containers, lease table, DNS zone, secrets, IAM/RBAC, and compute resources, and passes their identifiers into the contract module.

You can use these modules in two ways:

1. **All-in-one cluster modules** (`aws/` and `azure/`): Call the contract module and provision the surrounding cluster infrastructure for a quick, self-contained setup.
2. **Custom infrastructure stacks**: Call `<cloud>/modules/chinit` directly from your own Terraform root and customize it using three extension hooks:

   | Hook | Description |
   |---|---|
   | `config_files` | Additional files to include in the configuration bundle alongside the static XMLs (included in the SHA-256 verification digest) |
   | `chi_spec` / `keeper_spec` | Custom ClickHouse (`CHI`) or Keeper (`CHK`) specs overriding the default topology template |
   | `wrapper_extra` | Additional shell commands inserted into each role's user-data wrapper before `chinit boot` |

## Design Philosophy

The all-in-one modules compose the ecosystem modules — nothing is
duplicated. They focus on the core resources required to run a
`chinit`-managed ClickHouse cluster: storage, identity leases, private
DNS, secrets, IAM/RBAC, compute instances, monitoring, and threat
detection. Surrounding infrastructure such as existing VPCs, NLBs, public
DNS, mixed instance pools, and organization-specific stacks belong in
your own root module — compose the ecosystem modules directly (the
facade is the on-ramp, not the ceiling).

## Directory Layout

One modules directory per cloud — an ecosystem module and its cloud
siblings live side by side, mirroring the contract layout:

```
aws/                            Self-contained AWS cluster module
  examples/cluster/             Minimal AWS example
  modules/                      AWS modules (contract + ecosystem)
    chinit/                     Contract: spec/bundle/user-data renderer
    clickhouse/ compute/ dns_tls/ monitoring/
    network/ secrets/ security/ storage/
azure/                          Self-contained Azure cluster module
  examples/cluster/             Minimal Azure example
  modules/                      Azure modules (contract + ecosystem)
    chinit/                     Contract: spec/bundle/user-data renderer
    acme/ clickhouse/ dns/ image_gallery/
    keyvault/ monitoring/ security/ storage/ vmss/
```

### Ecosystem Modules

Opinionated, production-hardened modules extracted from real cluster
deployments. They pair with the contract module for stacks that want more
of the surrounding infrastructure managed:

**AWS** (`aws/modules/`):

| Module | Purpose |
|---|---|
| [`clickhouse`](./aws/modules/clickhouse) | Multi-tenant S3/CAS tiered storage (`storage.xml`, CHI/CHK specs) fed into the contract module's `config_files`/`chi_spec` hooks |
| [`compute`](./aws/modules/compute) | EC2 nodes, writers/readers ASGs (Spot-diversified readers, on-demand writers), NLBs, node-claims identity |
| [`network`](./aws/modules/network) | VPC (aws-ia/vpc pinned), per-role security groups, S3 gateway endpoint, flow logs |
| [`storage`](./aws/modules/storage) | Data/backup buckets, lifecycle tiering, per-tenant IAM policy documents |
| [`secrets`](./aws/modules/secrets) | Secrets Manager passwords + custom rotation Lambda |
| [`dns_tls`](./aws/modules/dns_tls) | Route53 zones/records, ACM or ACME (Let's Encrypt) certs |
| [`monitoring`](./aws/modules/monitoring) | CloudWatch alarms, scaling guidance, budgets |
| [`security`](./aws/modules/security) | GuardDuty + Security Hub + findings delivery |

**Azure** (`azure/modules/`):

| Module | Purpose |
|---|---|
| [`clickhouse`](./azure/modules/clickhouse) | Blob-tiered `storage.xml` + tuned CHI/CHK specs + the SRE troubleshooting scripts, fed into the contract module's hooks |
| [`vmss`](./azure/modules/vmss) | VMSS writers/readers (Spot-eviction-aware), keepers, backup replica, bastion, network interfaces |
| [`dns`](./azure/modules/dns) | Private DNS zone + node records |
| [`storage`](./azure/modules/storage) | Storage account: bundle/data/backup containers, lifecycle management |
| [`keyvault`](./azure/modules/keyvault) | Key Vault secrets + the fleet re-render automation |
| [`acme`](./azure/modules/acme) | Let's Encrypt (DNS-01) cert issuance + renewal automation |
| [`monitoring`](./azure/modules/monitoring) | Azure Monitor: log analytics, alert rules, action groups, budgets, maintenance windows |
| [`security`](./azure/modules/security) | Defender + diagnostic settings on the data surfaces |
| [`image_gallery`](./azure/modules/image_gallery) | Compute Gallery image definitions + versions for the golden image |

Each validates standalone: `cd <cloud>/modules/<name> && tofu init -backend=false && tofu validate`.

All modules can be validated offline with OpenTofu or Terraform:

```bash
cd aws/modules/chinit  && tofu init -backend=false && tofu validate
cd ../../..           && tofu init -backend=false && tofu validate
cd ../azure           && tofu init -backend=false && tofu validate   # azure azurerm provider needed
```

## Bundle SHA-256 Verification Contract

Both modules compute the bundle digest as:

```hcl
sha256(jsonencode(merge(config_files, { "chi.yaml" = chi_spec })))
```

covering `configs/network.xml`, `configs/ssl.xml`, `configs/storage.xml`, `configs/bootstrap-ddl.sql`, and `chi.yaml`. At boot, `chinit` recomputes the same digest over the downloaded files. Because Go's `json.Marshal` and Terraform's `jsonencode` serialize string maps with identical sorted keys, any mismatch between the spec's `config_bundle.sha256` and the objects in storage aborts the boot immediately.
