---
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
title: "Deploy with Crossplane"
nav_title: "Deploy with Crossplane"
nav_order: 5
permalink: /crossplane/
description: >-
  Deploy a chinit-bootstrapped ClickHouse cluster from the Kubernetes API
  with Crossplane — native managed resources for the cloud infra, the
  chinit contract module rendered inside the cluster via provider-opentofu.
---

# Deploy a Cluster with Crossplane

<p class="lead">One <code>ClickHouseCluster</code> claim deploys the full fleet — compute, identity leases, Private DNS, secrets, and the SHA-256 verified config bundle — from <code>kubectl</code>, GitOps, or Argo CD.</p>

[Crossplane](https://docs.crossplane.io) manages the cloud resources as
Kubernetes objects; the [`chinit`
contract](https://github.com/stackgenhq/chinit/tree/main/terraform/modules/chinit)
renders inside the cluster through
[provider-opentofu](https://github.com/upbound/provider-opentofu) `Workspace`
resources. The bundle layout, per-role specs, and the SHA-256 gate are
byte-identical to the Terraform deliverables — one renderer, no drift
between the two deployment paths.

## What Gets Provisioned

Same resources as the [Terraform modules]({{ '/terraform/' | relative_url }}) —
the claim's `Composition` maps 1:1 onto the module variables:

| Component | AWS | Azure |
|---|---|---|
| Writers (write path) | On-demand Auto Scaling Group + lease identity (`writer-N`), metadata + cache volumes | Flexible VM Scale Set + lease identity, metadata + cache disks |
| Readers (query path) | Spot-capable Auto Scaling Group + lease identity (`reader-N`) | Spot VM Scale Set + lease identity |
| Keepers (Raft coordination) | 3 fixed EC2 instances, static private IPs (+20..), persistent Raft EBS volumes | 3 fixed VMs, static private IPs, persistent Raft disks |
| Backup replica | Optional fixed EC2 instance at the `+30` offset | Optional fixed VM at the `+30` offset |
| Configuration bundle | S3 bucket with per-role specs, `chi.yaml`, static XMLs, bootstrap DDL (SHA-256 verified) | Storage account + container, same contents |
| Identity & DNS | DynamoDB ordinal-lease table (TTL) + Route 53 private zone | Table Storage lease table + Azure Private DNS zone |
| Secrets | Secrets Manager (randomly generated; specs hold names only) | Key Vault (randomly generated; specs hold names only) |
| Access | Least-privilege instance profile | User-assigned managed identity with scoped RBAC |

## Install

```bash
# 1. Crossplane v2 with the composition functions
kubectl install-packages crossplane-contrib/function-patch-and-transform
kubectl install-packages crossplane-contrib/function-auto-ready

# 2. provider-opentofu (runs the chinit contract module in-cluster)
kubectl install-packages provider-opentofu

# 3. chinit's API + compositions from the repo checkout
kubectl apply -f crossplane/definitions/
kubectl apply -f crossplane/configuration/
kubectl apply -f crossplane/aws/ -f crossplane/azure/
```

## Credentials

The `provider-opentofu` `ProviderConfig`s read secrets in `upbound-system`:

```bash
# AWS — access-key credentials for the contract Workspace's aws provider
export AWS_ACCESS_KEY_ID=… AWS_SECRET_ACCESS_KEY=…
kubectl -n upbound-system create secret generic opentofu-aws-creds \
  --from-literal=credentials="AWS_ACCESS_KEY_ID=$AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY=$AWS_SECRET_ACCESS_KEY"

# Azure — service principal for the contract Workspace's azurerm provider
kubectl -n upbound-system create secret generic opentofu-azure-creds \
  --from-literal=credentials="CHINIT_AZURE_CLIENT_ID=$AZ_CLIENT_ID
CHINIT_AZURE_CLIENT_SECRET=$AZ_CLIENT_SECRET
CHINIT_AZURE_TENANT_ID=$AZ_TENANT_ID
CHINIT_AZURE_SUBSCRIPTION_ID=$AZ_SUBSCRIPTION_ID"
```

Scope them exactly like the Terraform modules require (EC2/VPC, S3,
DynamoDB, Secrets Manager, Route53, IAM on AWS; Contributor + Private DNS +
Key Vault on Azure).

The **native managed resources** (the VPC, buckets, ASGs, VMSS, …) consume
your Crossplane cloud providers' own `ProviderConfig`s
(`aws.upbound.io` / `azure.upbound.io`) — set those up per the providers'
install docs.

## Apply a Claim

```bash
kubectl apply -f crossplane/examples/example-aws.yaml     # or example-azure.yaml
```

```yaml
apiVersion: chinit.stackgenhq.io/v1alpha1
kind: ClickHouseCluster
metadata:
  name: demo-aws
spec:
  cloud: aws
  clusterName: demo
  region: us-west-2
  dnsZoneName: clickhouse.internal
  goldenAmiId: ami-0f8511dd30c68ca21
  subnetCidr: 10.42.0.0/24
  writerMax: 2
  readerMax: 6
  keeperCount: 3
```

Every field mirrors the Terraform module's variables — the [claim
reference]({{ site.baseurl }}{% link docs/spec.md %}) holds the full list
(`writerMax`/`readerMax` are the ASG/VMSS **maxima** = the ordinal claim
ceilings, never live counts).

## After the Apply

```bash
kubectl get clickhouseclusters        # the claims
kubectl get managed                   # every cloud resource + both Workspaces
kubectl describe clickhousecluster demo-aws
```

Read the outputs (same surfaces the `terraform output node_endpoints` path
documents) from the claim's connection secret:

```bash
kubectl get secret demo-aws -o jsonpath='{.data.node_endpoints}' | base64 -d
kubectl get secret demo-aws -o jsonpath='{.data.config_files_hash}' | base64 -d
```

- **`node_endpoints`** — private DNS hostnames for writers, readers,
  keepers, and the backup replica.
- **`writers_asg_name` / `readers_asg_name`** — slide desired capacity up or
  down via the claim (`writerDesired`/`readerDesired`); `chinit` handles
  identity claims and cluster joins.
- **`secrets`** — where the generated admin credentials live.
- **`config_files_hash`** — the bundle's SHA-256 digest.

## Scaling

| Change | Effect |
|---|---|
| `writerDesired` / `readerDesired` in the claim | Scales the ASG/VMSS in place — the contract module does **not** re-render (no bundle churn). |
| `writerMax` / `readerMax` in the claim | Re-runs the contract module (the ceilings render into every spec) and the `chinit` spec-sync timer converges the fleet. |
| `instanceType` / `vmSize` | Re-renders the launch templates/VMSS; scale-in replacements pick the new size — `chinit` rejoins them. |

### Troubleshooting a Node

```bash
# AWS
aws ssm start-session --target <instance-id>
sudo cat /var/lib/chinit/state.json
journalctl -u chinit-boot.service -n 50
```

```bash
# Azure
az vm run-command invoke --command-id RunShellScript \
  --scripts 'cat /var/lib/chinit/state.json' -g <rg> -n <vm>
```

For the runner side: `kubectl get workspace -A` shows each contract-module
run, `kubectl get managed -o wide` shows every cloud resource's sync/ready
state, and the provider's `Deployment` logs (`-n upbound-system`) show tofu
errors verbatim.

## Design Notes

- **One renderer.** The bundle/spec rendering and the SHA-256 digest stay in
  [`terraform/modules/chinit/<cloud>`](https://github.com/stackgenhq/chinit/tree/main/terraform/modules/chinit) —
  the only implementation of the `chinit` interface. Crossplane's composition
  is cloud-infrastructure only. A future fully-native composition (a Go
  composition function recreating `terraformPairsDigest`) could drop the
  tofu dependency, but until then two renderers would risk digest drift —
  the gate chinit Fatals on at boot.
- **Ordering.** The claim's pipeline runs the `Workspace` first; the
  compute resources receive their `user_data` from the Workspace's
  `bootstrap_script` output, so a node never launches before its bundle
  exists (the same race the Terraform module closes with `depends_on`).
- **Golden images cross-cloud.** Pass `goldenAmiId` (AWS) / `goldenImageId`
  (Azure) resolved from SSM Parameter Store / Compute Gallery versions — or
  the fixed ids the packer build prints. EC2 launch templates also accept
  `resolve:ssm:/chinit/golden-ami/antalya` values directly.

## Go Deeper

- [crossplane/ in the repo](https://github.com/stackgenhq/chinit/tree/main/crossplane) —
  XRD, compositions, provider configs, examples
- [Deploy with Terraform]({{ '/terraform/' | relative_url }}) — the same deployment without a Kubernetes control plane
- [The Spec]({{ '/spec/' | relative_url }}) — every `chinit.yaml` field the claim surface can render
