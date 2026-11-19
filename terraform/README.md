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

## Contract Modules (`modules/chinit/`)

The interface between Terraform and `chinit` lives in two contract modules:

```
modules/chinit/aws      modules/chinit/azure
```

Each contract module renders the per-role `chinit-<role>.yaml` specs, computes the SHA-256 bundle digest, uploads the configuration bundle to object storage, and renders the per-role user-data wrappers. **It creates no cloud infrastructure itself** — the caller owns the storage buckets/containers, lease table, DNS zone, secrets, IAM/RBAC, and compute resources, and passes their identifiers into the contract module.

You can use these modules in two ways:

1. **All-in-one cluster modules** (`aws/` and `azure/`): Call the contract module and provision the surrounding cluster infrastructure for a quick, self-contained setup.
2. **Custom infrastructure stacks**: Call `modules/chinit/<cloud>` directly from your own Terraform root and customize it using three extension hooks:

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

```
modules/chinit/          Contract modules (spec, bundle, and user-data renderers)
  aws/                   Consumed by aws/ and custom AWS stacks
  azure/                 Consumed by azure/ and custom Azure stacks
aws/                     Self-contained AWS cluster module
  examples/cluster/      Minimal AWS example
azure/                   Self-contained Azure cluster module
  examples/cluster/      Minimal Azure example
```

### Ecosystem Modules (`modules/<name>/`)

Opinionated, production-hardened modules extracted from real cluster
deployments. They pair with the contract module for stacks that want more
of the surrounding infrastructure managed:

| Module | Purpose |
|---|---|
| [`modules/clickhouse`](./modules/clickhouse) | Multi-tenant S3/CAS tiered storage (`storage.xml`, CHI/CHK specs) fed into the contract module's `config_files`/`chi_spec` hooks |
| [`modules/compute`](./modules/compute) | EC2 nodes, writers/readers ASGs (Spot-diversified readers, on-demand writers), NLBs, node-claims identity |
| [`modules/network`](./modules/network) | VPC (aws-ia/vpc pinned), per-role security groups, S3 gateway endpoint, flow logs |
| [`modules/storage`](./modules/storage) | Data/backup buckets, lifecycle tiering, per-tenant IAM policy documents |
| [`modules/secrets`](./modules/secrets) | Secrets Manager passwords + custom rotation Lambda |
| [`modules/dns_tls`](./modules/dns_tls) | Route53 zones/records, ACM or ACME (Let's Encrypt) certs |
| [`modules/monitoring`](./modules/monitoring) | CloudWatch alarms, scaling guidance, budgets |
| [`modules/security`](./modules/security) | GuardDuty + Security Hub + findings delivery |

Each validates standalone: `cd modules/<name> && tofu init -backend=false && tofu validate`.

All modules can be validated offline with OpenTofu or Terraform:

```bash
cd modules/chinit/aws && tofu init -backend=false && tofu validate
cd ../../aws          && tofu init -backend=false && tofu validate
```

## Bundle SHA-256 Verification Contract

Both modules compute the bundle digest as:

```hcl
sha256(jsonencode(merge(config_files, { "chi.yaml" = chi_spec })))
```

covering `configs/network.xml`, `configs/ssl.xml`, `configs/storage.xml`, `configs/bootstrap-ddl.sql`, and `chi.yaml`. At boot, `chinit` recomputes the same digest over the downloaded files. Because Go's `json.Marshal` and Terraform's `jsonencode` serialize string maps with identical sorted keys, any mismatch between the spec's `config_bundle.sha256` and the objects in storage aborts the boot immediately.
