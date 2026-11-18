<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# ClickHouse Golden AMI (Packer) — AWS

Pre-baked AMIs reduce EC2 node bootstrap time from 8–12 minutes to 2–3 minutes and allow nodes to boot in private subnets without NAT gateway egress.

> **Boot procedure**: See [BOOT.md](../BOOT.md) for a walkthrough of what happens from instance launch to `boot-finished` across node roles (`writer`, `reader`, `keeper`, and `backup`).

## Included Components

| Component | Purpose |
|---|---|
| Ubuntu 24.04 LTS (`arm64`) | Graviton base OS |
| `clickhouse-server` / `clickhouse-client` / `clickhouse-keeper` | Pinned ClickHouse version (for example, `26.8.36.3`) |
| `clickhouse-backup` | Backup utility |
| AWS CLI v2 | S3 downloads via the EC2 instance role |
| `chinit` (pinned) | Node bootstrapper and maintenance runner (`chinit boot` + systemd subcommands) |
| CloudWatch agent | Node metrics and log shipping |
| `otelcol-contrib` (pinned) | Optional OpenTelemetry log and metric shipping (`log_shipping.agent = "otel"`) |
| Fluent Bit (pinned) | Optional log shipping alternative (`log_shipping.agent = "fluentbit"`) |
| OS tuning (THP disabled, limits) | Applied at image build time and verified idempotently by `chinit` at boot |

Remote log-shipping agents remain inactive unless `log_shipping.endpoint` is configured in `chinit.yaml`. The `chinit_version` variable pins the release tag downloaded and SHA-256 verified during the Packer build; version history is tracked in [CHANGELOG.md](./CHANGELOG.md).

## Prebuilt Public AMI

A public golden AMI is published in `us-west-2`:

| Attribute | Value |
|---|---|
| **AMI ID** | `ami-0eaa398497899f070` (`us-west-2`) |
| **Name** | `clickhouse-golden-26.8.36.3-1794878271` |
| **Included Software** | Ubuntu 24.04 `arm64`, ClickHouse 26.8.36.3, `chinit`, `clickhouse-backup` 2.5.0, AWS CLI, CloudWatch agent |
| **Author Tag** | `stackgen.com` |

Because AWS does not allow public AMIs to have encrypted root snapshots, the public AMI uses an unencrypted root snapshot; private builds created with this template enable EBS encryption by default.

## Building Your Own AMI

```bash
cd images/aws
packer init .
packer build -var-file=defaults.pkrvars.hcl -var-file=prod.pkrvars.hcl clickhouse.pkr.hcl
```

The build writes `manifest.json` with the new AMI ID, which you can pass to Terraform:

```hcl
golden_ami_id = "ami-xxxxxxxx"
```

## Version Policy

- By default, `defaults.pkrvars.hcl` pins a current ClickHouse LTS release.
- Full package version strings are required so `apt` pins exact package versions.
- To upgrade, update the version pin, rebuild the AMI, and roll nodes using `chinit fleet-rollout` or an Auto Scaling Group instance refresh.

## Altinity Antalya Build (Optional CAS Tier)

To build an image using the Altinity Antalya ClickHouse distribution (Apache-2.0, which includes content-addressed S3 storage support), pass `build_flavor = "antalya"`:

```bash
packer build \
  -var build_flavor=antalya \
  -var clickhouse_version=26.8.34.20001.altinityantalya \
  -var-file=defaults.pkrvars.hcl clickhouse.pkr.hcl
```

When `clickhouse_version` exists in `builds.altinity.cloud/antalya-apt-repo` (e.g. `26.6.8.20004.altinityantalya`), Packer installs it directly via `apt`. When building a `26.8` Antalya version from the `antalya-26.8` branch before apt promotion (e.g. `26.8.34.20001.altinityantalya`), Packer automatically falls back to downloading the signed `arm64` `.deb` packages from `s3://altinity-build-artifacts/REFs/${antalya_ci_ref}/${antalya_ci_sha}/build_arm_release/` and verifies each package's SHA-512 against `sign_release_arm_release/*.deb.sha512.gpg`.
