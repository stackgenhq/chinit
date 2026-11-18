<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Changelog — ClickHouse Golden AMI (AWS)

Pin and build history for the golden AMI built from [clickhouse.pkr.hcl](./clickhouse.pkr.hcl).

Each update that bumps a component pin (`chinit_version`, `clickhouse_version`, `clickhouse_backup_version`, `otel_collector_version`, or `fluentbit_version`) records what changed and why.

## Adding an Entry

- Add newest entries first under the corresponding component section (`## chinit`, `## ClickHouse`).
- Update the default version in `clickhouse.pkr.hcl` (or the variable file) in the same commit.

## chinit (`chinit_version`)

### chinit 0.1.2 — CloudWatch ordering, vertical scale-down & parallel CAS/upstream AMI builds
What: Runs `configure_cloudwatch` immediately after `start_clickhouse` during boot, adds vertical scale-down + hardware-proportional sizing, and builds both `upstream` (`26.8.36.3`) and `antalya` (`26.6.8.20004.altinityantalya`) golden AMIs in parallel.
Why: Ensures CloudWatch logs/metrics stream before later boot steps, and produces the Altinity Antalya CAS-capable golden AMI alongside upstream LTS.

### chinit 0.1.1 — admin auth for prometheusReload and S3 health probe
What: Authenticates `prometheusReload` and the S3 health probe as `admin` (`#1`).
Why: Fixes readonly default-user rejections during health and Prometheus reload checks.

### chinit 0.1.0 — initial public release
What: Cloud-adaptive ClickHouse node bootstrapper and lifecycle manager for AWS and Azure VMs, including convergent 2-phase node bootstrap, lease and static node identity, recurring systemd maintenance subcommands, graceful spot and shutdown draining, fleet task coordination, and OTLP/Prometheus observability.
Why: Initial public open-source release of `chinit` (`v0.1.0`).

## ClickHouse (`clickhouse_version`)

### 26.8.36.3 — LTS pin
Default pinned ClickHouse LTS version for the golden AMI.
