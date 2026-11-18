<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# chinit — ClickHouse Node Bootstrapper for Cloud VMs

**chinit prepares a ClickHouse server to join its cluster on cloud VMs — automatically, reliably, and without a pile of fragile boot scripts.**

When you run ClickHouse on virtual machines instead of Kubernetes, every new server has work to do before it becomes a functioning cluster member: detect which cloud and role it belongs to, fetch its configuration, claim its identity, prepare its disks, join the cluster, and run ongoing maintenance throughout its lifecycle. Doing this with cloud-init shell scripts works at first, until a node gets replaced by a spot interruption or scale-in event and the scripts race the cluster into an inconsistent state.

`chinit` replaces those scripts with **a single static binary** that runs the same tested logic on every node and cloud:

```
instance launches → chinit boot → node is a healthy cluster member (~2-3 min)
```

[![CI](https://github.com/stackgenhq/chinit/actions/workflows/ci.yml/badge.svg)](https://github.com/stackgenhq/chinit/actions/workflows/ci.yml)
[![GitHub release](https://img.shields.io/github/v/release/stackgenhq/chinit?include_prereleases)](https://github.com/stackgenhq/chinit/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/stackgenhq/chinit)](https://img.shields.io/github/go-mod/go-version/stackgenhq/chinit)
[![License](https://img.shields.io/github/license/stackgenhq/chinit)](./LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/stackgenhq/chinit)](https://goreportcard.com/report/github.com/stackgenhq/chinit)

## What It Does

- **Identifies the node**: Detects the cloud provider (AWS or Azure, with a provider registry for additional clouds), reads instance metadata, and determines the node's role (`writer`, `reader`, `keeper`, or `backup`) from a declarative spec.
- **Fetches configuration safely**: Downloads a versioned configuration bundle from object storage and verifies its SHA-256 digest before applying anything.
- **Prepares the machine**: Mounts and formats disks by filesystem label, applies OS-level tuning for ClickHouse, and installs validated configuration files.
- **Claims a stable identity**: Assigns a deterministic identity (via a lease table or static IP offset) so a database node keeps the same hostname and DNS record even when the underlying VM is replaced.
- **Joins the cluster**: Creates schemas, syncs replicas, and registers with peer nodes.
- **Runs ongoing maintenance**: Executes recurring background tasks (schema sync, replica sync, lease renewal, health checks, disk autoscaling, backups, and spot interruption draining) as systemd timers and services calling `chinit` subcommands.
- **Drains cleanly on shutdown**: Releases its identity lease, removes DNS records, and drops replica registrations through a single systemd shutdown hook.

From first boot to termination, the entire lifecycle is driven by one binary and one YAML spec per role.

## Where It Fits

ClickHouse on Kubernetes is well served by operators ([Altinity's operator](https://github.com/Altinity/clickhouse-operator) and [the official ClickHouse operator](https://github.com/ClickHouse/clickhouse-operator)). This project targets **stateful ClickHouse fleets on cloud VMs** — EC2 Auto Scaling Groups and Azure VM Scale Sets — where you want direct NVMe/EBS storage access, spot instance economics, kernel-level tuning, and no Kubernetes control plane in the middle.

The companion **Packer templates** in [`images/`](./images) pre-bake ClickHouse, `chinit`, backup tools, and OS settings into a VM image for AWS and Azure, reducing node bootstrap time from 8–12 minutes to 2–3 minutes.

## Key Features

- **Single static binary**: No external agents or runtime dependencies; requires only a ~1.5 KB user-data wrapper to launch a node.
- **Cloud-adaptive**: Built-in support for AWS and Azure via a clean provider interface.
- **Checksum-verified config bundles**: Pulls configuration from S3 or Azure Blob Storage and enforces SHA-256 verification.
- **Declarative node specs (`chinit.yaml`)**: Typed and validated configuration per node role.
- **Convergent, resumable boot**: Probes actual machine state before each step, records progress in `/var/lib/chinit/state.json`, and safely resumes interrupted boots.
- **Stable node identity**: Supports both static IP-offset mapping and lease-claimed ordinals (DynamoDB on AWS, Table Storage on Azure) for auto-scaling groups.
- **Graceful spot & shutdown draining**: Drains replication queues, drops replicas, and releases leases on spot notices or OS shutdown.
- **Keeper quorum protection**: Verifies ClickHouse Keeper (`mntr`) quorum health before rolling or stopping keeper nodes.
- **Built-in maintenance subcommands**: Installed as simple one-line systemd units and timers.
- **Pre-baked golden images**: Ready-to-use Packer definitions for AWS AMIs and Azure Compute Gallery images.

## Getting Started

The quickest way to deploy a full cluster is with the **Terraform modules**, which provision the lease table, private DNS zone, secrets, instance IAM/RBAC roles, SHA-verified configuration bundle, and the four node roles (`writer`, `reader`, `keeper`, and optional `backup`):

```hcl
module "clickhouse" {
  source = "github.com/stackgenhq/chinit//terraform/aws" # or .../terraform/azure

  cluster_name  = "demo"
  golden_ami_id = "ami-…" # built from images/aws (or a gallery image ID on Azure)
  dns_zone_name = "clickhouse.internal"
  subnet_cidr   = "10.42.0.0/24"
}
```

See [`terraform/`](./terraform) for full module documentation and examples.

To install the `chinit` binary directly on a Linux host:

```bash
CHINIT_VERSION=v0.1.0
curl -fsSL -o chinit \
  "https://github.com/stackgenhq/chinit/releases/download/${CHINIT_VERSION}/chinit-linux-arm64"
curl -fsSL "https://github.com/stackgenhq/chinit/releases/download/${CHINIT_VERSION}/chinit-SHA256SUMS" \
  | grep linux-arm64 | sed 's|chinit-linux-arm64|chinit|' | sha256sum -c -
sudo install -m 0755 chinit /usr/local/bin/chinit
```

Then run `chinit boot -spec /var/lib/clickhouse-bootstrap/chinit.yaml`. See the [Operator's Guide](./docs/chinit.md) for full details on the spec format, boot pipeline, and maintenance subcommands, and [images/BOOT.md](./images/BOOT.md) for a walkthrough of the boot sequence.

## Documentation

**Using chinit**

- [Quickstart](./docs/quickstart.md) — Install the binary, write a spec, and boot a node
- [Operator's Guide](./docs/chinit.md) — Spec configuration, boot pipeline, and maintenance subcommands
- [CLI Reference](./docs/cli.md) — Subcommands, flags, and exit codes
- [Spec Reference (`chinit.yaml`)](./docs/spec.md) — Complete configuration reference
- [Node Boot Procedure](./images/BOOT.md) — Step-by-step lifecycle from instance launch to `boot-finished`
- [Terraform Modules](./terraform/README.md) — Deploy a complete cluster on AWS or Azure

**Building Images & Contributing**

- [Golden Images](./images/README.md) — Packer templates for AWS and Azure
- [Architecture](./docs/chinit-architecture.md) — Internal package layout, boot engine, identity model, and conformance tests
- [Contributing Guide](./CONTRIBUTING.md) — Development workflow and pull request guidelines

## Community & Support

- Open a [GitHub issue](https://github.com/stackgenhq/chinit/issues) to report bugs or request features.
- Read [CONTRIBUTING.md](./CONTRIBUTING.md) before submitting a pull request.
- See [SECURITY.md](./SECURITY.md) for reporting security vulnerabilities.

## Requirements

- Linux VMs on AWS or Azure (`arm64` or `amd64`)
- ClickHouse 24.8+ recommended (tested with 25.12 and 26.x)
- Object storage bucket/container (AWS S3 or Azure Blob Storage) for the configuration bundle

## Releases

- Pushing a `v0.*` tag triggers CI to build and publish static binaries (`linux`/`darwin`, `arm64`/`amd64`) along with `chinit-SHA256SUMS` via GoReleaser.
- Release asset names (`chinit-<os>-<arch>` and `chinit-SHA256SUMS`) are stable so golden-image builds and bootstrap wrappers can pin and verify them before installation.

## License

Copyright (C) 2026 StackGen, Inc. All rights reserved.

`chinit` is licensed under the Apache License, Version 2.0. See [LICENSE](./LICENSE) and [NOTICE](./NOTICE).

This project was originally extracted from a fork of the [Altinity Kubernetes Operator for ClickHouse](https://github.com/Altinity/clickhouse-operator) (Apache-2.0); extracted portions are Copyright (C) 2019 Altinity Ltd and/or its affiliates. Third-party Go dependency attributions are listed in [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md).
