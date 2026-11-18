<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Deploy a `chinit` Cluster with Crossplane

Deploy a complete `chinit`-bootstrapped ClickHouse cluster (AWS or Azure) from
the Kubernetes API: apply one claim, get writers/readers as Auto Scaling
Groups (or VM Scale Sets), keepers/backup as fixed VMs, the SHA-256 verified
configuration bundle, identity leases, private DNS, secrets, and IAM/RBAC —
the same stack the [Terraform modules](../terraform) provision.

## How It Fits Together

Crossplane reconciles every **cloud resource** as native Kubernetes API
objects (Managed Resources), so the cluster lives in `kubectl get` / GitOps /
Argo CD like any other resource. The `chinit` **configuration contract** —
the per-role specs, the CHI/CHK specs, the static XMLs, the bundle SHA-256
digest, and the per-role user-data wrappers — is rendered and applied by the
existing Terraform **contract modules**, run through
[`provider-opentofu`](https://github.com/upbound/provider-opentofu)
`Workspace` resources inside the cluster:

```
ClickHouseCluster claim
  └─ Composition pipeline
       ├─ 1. Workspace (opentofu.upbound.io)            ← terraform/modules/chinit/<cloud>
       │      renders + uploads the bundle (specs,       the single chinit↔provisioner
       │      chi.yaml, XMLs) and computes the           contract implementation
       │      SHA-256 digest; outputs: bootstrap script, digest, bucket ids
       ├─ 2. Patches (function-patch-and-transform)      ← wire Workspace outputs into
       │      bootstrapScript → userData on every         every compute resource
       │      compute resource (bytes → base64)
       └─ 3. auto-ready (function-auto-ready)            ← compute readiness only

Cloud infrastructure (synchronized by the same claim)
  VPC/VNet + subnet + gateway endpoints · S3 buckets / storage accounts +
  containers · DynamoDB / Table Storage node-claims leases · Route53 private
  zone / Azure Private DNS · Secrets Manager / Key Vault passwords ·
  IAM instance profile / user-assigned identity + RBAC ·
  LaunchTemplates + ASGs (spot-capable readers) / VMSS + fixed VMs ·
  keeper/bkp fixed instances at the +20/+30 octet offsets
```

### The `chinit` contract stays single-vendor

The [SHA-256 bundle gate](../pkg/chinit/infra/aws/s3bundle.go) requires the
provisioner to render byte-identical specs and digest. Rather than fork that
logic into template functions (two implementations that must never drift),
the Composition keeps calling the upstream `terraform/modules/chinit/<cloud>`
module inside a `Workspace`. The Crossplane tree in this directory is
**cloud-infrastructure only**; the contract module remains the only renderer
of the `chinit` interface.

## Layout

```
crossplane/
  README.md             this file
  definitions/
    clickhouseclusterdefinition.yaml   XRD (cluster-parameters API surface)
  configuration/
    providerconfig-aws.yaml            opentofu ProviderConfig (AWS credentials + remote state)
    providerconfig-azure.yaml          opentofu ProviderConfig (Azure credentials + remote state)
  aws/
    composition.yaml                   one claim → full AWS cluster
  azure/
    composition.yaml                   one claim → full Azure cluster
  examples/
    example-aws.yaml                   example AWS claim
    example-azure.yaml                 example Azure claim
docs/
  crossplane.md                        rendered guide (install → apply → verify)
```

## Prerequisites

1. **A Kubernetes cluster** with [Crossplane](https://docs.crossplane.io) v2
   installed (`crossplane install --provider … v2+`), including the
   composition-function packages
   [`function-patch-and-transform`](https://github.com/crossplane-contrib/function-patch-and-transform)
   and
   [`function-auto-ready`](https://github.com/crossplane-contrib/function-auto-ready).
2. [`provider-opentofu`](https://github.com/upbound/provider-opentofu)
   (`opentofu.upbound.io/v1beta1`: `Workspace`, `ProviderConfig`).
3. **A golden image** per cloud:
   - AWS — an AMI built from [`images/aws`](../images/aws) (or the public one,
     e.g. `ami-0f8511dd30c68ca21` for the Altinity Antalya build).
   - Azure — a Compute Gallery image version id built from
     [`images/azure`](../images/azure).
4. **Cloud credentials** for Crossplane to provision with:
   - AWS — an access key/secret pair with the same permissions the Terraform
     modules require (EC2/VPC, S3, DynamoDB, Secrets Manager, Route53, IAM).
   - Azure — a service principal (client id/secret + tenant) with Contributor
     over the target resource group (plus Private DNS, storage, Key Vault
     permissions).
5. A `resource_group_name` (Azure) you own — the claim provisions into it;
   Crossplane does **not** create the resource group.

## Install

```bash
# Crossplane packages (skip what you already run)
kubectl apply -f https://raw.githubusercontent.com/upbound/provider-opentofu/main/examples/cluster/install.yaml

# chinit's own API surface, provider configs, and compositions
kubectl apply -f crossplane/definitions/
kubectl apply -f crossplane/configuration/
kubectl apply -f crossplane/aws/
kubectl apply -f crossplane/azure/
```

Fill in the credentials referenced by `crossplane/configuration/providerconfig-*.yaml`
(secrets named `opentofu-aws-creds` / `opentofu-azure-creds` in `upbound-system`,
keys per the comments inside those files), then apply a claim:

```bash
kubectl apply -f crossplane/examples/example-aws.yaml
```

## Watch the Claim

```bash
kubectl get claims
NAME           READY   CONNECTION   SYNCED   AGE
demo-aws       True    ...          True     8m

kubectl get workspaces opentofu.upbound.io -A     # the contract-module run
kubectl get managed                                # every MR this claim owns
```

The claim becomes `READY` when every managed resource and both `Workspace`
runs are `Synced`/`Ready`. Node identity claims (`writer-N`/`reader-N`),
keeper/backup static identities, and every A-record land automatically as
each node boots — no further steps.

## Outputs After Apply

After the cluster claim is ready, read the same outputs the Terraform
modules document from the claim's connection secret (every Workspace output
lands in the connection secret; the composition also mirrors them as claim
labels/annotations):

| Output | Meaning |
|---|---|
| `config_files_hash` | The SHA-256 bundle digest `chinit` verifies at boot |
| `bundle_bucket` / `data_bucket` / `backup_bucket` | S3 bucket (or storage-account) names |
| `node_claims_table` | The ordinal-lease table |
| `node_endpoints` | Writer/reader/keeper/backup FQDNs |
| `secrets` | Secret Manager / Key Vault names |
| `writers_asg_name` / `readers_asg_name` | ASG (or VMSS) names to scale |

## Scaling

Change `writerDesired`, `readerDesired`, or `readerSpot` in the claim — the
MRs that own those fields reconcile in place; the chinit contract module runs
only when its inputs change (the `Workspace`'s vars are patched in), so
re-scales never re-render the bundle. To go **beyond the ceiling**, change
`writerMax`/`readerMax` in the claim: that re-runs the contract module (it
renders `writer_max`/`reader_max` into every spec) and the ASG/VMSS re-rolls
through the `chinit` `spec-sync` timer — no manual refresh.

## Troubleshooting

```bash
kubectl get managed -o wide                       # every MR's SYNCED/READY
kubectl logs -l crossplane.io/provider=opentofu -n upbound-system   # the Workspace runner
kubectl get workspace -o yaml                     # the tofu run's outputs / events
```

Inside a node (the same as the Terraform path): `chinit state` via
`aws ssm start-session --target <id>` (or `az vm run-command invoke`),
`cat /var/lib/chinit/state.json`, `journalctl -u chinit-boot.service`.

## Go Deeper

- [Provider-opentofu Workspace spec](https://github.com/upbound/provider-opentofu) —
  remote-state, credential, and variable-file details
- [Terraform modules](../terraform/README.md) — the same deployment without Crossplane
- [The chinit contract in code](../pkg/chinit/infra/aws/s3bundle.go) — the SHA-256 gate chinit verifies at boot
