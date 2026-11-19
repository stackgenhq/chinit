---
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
title: "CLI reference"
nav_title: "CLI reference"
nav_order: 6
permalink: /cli/
mermaid: true
description: >-
  Every chinit subcommand, flag, schedule, and structured exit code —
  one static binary for bootstrap, day-2 healing, fleet rollouts, and
  shutdown draining.
---

# CLI Reference

<p class="lead">Why <code>chinit</code> ships every bootstrap pipeline, background healer, fleet rollout coordinator, and shutdown hook inside a single static binary — and how to use each subcommand.</p>

In traditional VM deployments, operational logic gets scattered across `user-data` bash scripts, `/usr/local/bin` helper scripts, `cron.d` entries, and cloud Lambda functions. Over time, those scripts disagree on where state lives, parse CLI output differently, and drift out of sync with the running ClickHouse version.

`chinit` consolidates **every lifecycle action into one binary**. Whether `systemd` is firing a 5-minute timer or an on-call engineer is diagnosing a node over SSH, every subcommand reads the node's settled identity from `/var/lib/chinit/state.json` and emits structured JSON logs and deterministic exit codes.

---

## Global Commands & Structured Exit Codes

```bash
chinit version        # Print compiled version and commit metadata
chinit help           # Print full subcommand overview
```

Every subcommand returns a machine-actionable exit code so `systemd` units, health checks, and CI scripts never have to `grep` log messages:

| Exit Code | Meaning | Typical Examples |
|---|---|---|
| `0` | **Healthy / Completed** | Boot converged, lease renewed, Keeper quorum healthy |
| `1` | **Usage / Validation Error** | Invalid flag, malformed `chinit.yaml` field |
| `2` | **Fatal Boot or Degraded Status** | Bundle SHA-256 mismatch, IAM `AccessDenied`, degraded Keeper ensemble |
| `3` | **Drain or Unhealthy Failure** | Unhealthy Keeper quorum, drain timeout exceeded |

---

## Node Bootstrap & Live Refresh: `chinit boot`

```bash
chinit boot [-spec /var/lib/clickhouse-bootstrap/chinit.yaml] [-cloud aws|azure] [-dry-run] [-refresh] [-state /var/lib/chinit/state.json]
```

| Flag | Default | When & Why to Use It |
|---|---|---|
| `-spec` | `/var/lib/clickhouse-bootstrap/chinit.yaml` | Path to the role's declarative `chinit.yaml` spec. |
| `-cloud` | *(auto-detected)* | Explicit cloud override (`aws` or `azure`). When omitted, probes `-cloud` → `CHINIT_CLOUD` → DMI `sys_vendor` → IMDS. |
| `-dry-run` | `false` | Validates `chinit.yaml` and prints the exact 26-step plan without touching disks, cloud APIs, or services. |
| `-refresh` | `false` | **In-place live update**: re-renders and applies an updated bundle/spec on a running VM while preserving its claimed identity, data disks, and Keeper Raft `myid`. |
| `-state` | `/var/lib/chinit/state.json` | Path to the persistent state & step-resume ledger. |

---

## Day-2 Subcommands by Operational Domain

### 1. Identity, Routing & Peer Convergence

| Subcommand | Default Timer | What It Accomplishes |
|---|---|---|
| `lease-renew` | Every 5 min | Extends the VM's ordinal lease in DynamoDB / Azure Table Storage, reconciles `/etc/hosts`, sweeps stale Private DNS `A` records, hot-reloads `remote_servers`, and publishes the node's health status row. |
| `spec-sync` | Every 10 min | Polls the object storage bundle prefix for `chinit.yaml` or `config_bundle.sha256` updates and runs `chinit boot --refresh` automatically when configuration changes. |

### 2. Schema & Replication Self-Healing

| Subcommand | Default Timer | What It Accomplishes |
|---|---|---|
| `schema-sync` | Hourly | Propagates DDL definitions from live peers onto this node via `HostCreateTables` (`-retries N`, `-prune-stale-self`). |
| `replica-sync` | Every 5 min | Issues `SYSTEM SYNC REPLICA` across local `ReplicatedMergeTree` tables. |
| `queue-heal` | Hourly | Inspects stuck replication queues while respecting active merges: runs `SYSTEM RESTORE REPLICA` on read-only tables, `SYSTEM SYNC REPLICA` on orphan part references, and `SYSTEM RESTART REPLICA` when merges are idle. |
| `dead-replica-gc` | Hourly | Sweeps ClickHouse Keeper metadata and drops replica registrations belonging to ordinals that no longer hold an active lease. |

### 3. Fleet Coordination, Cordoning & Rolling Upgrades

| Subcommand | Mode | What It Accomplishes |
|---|---|---|
| `fleet-status` | On demand | Prints a real-time cluster table (hostname, role, health probe, ClickHouse version, active spec hash, cordon state, and heartbeat age). Pass `-json` for automation. |
| `cordon <host>` / `uncordon <host>` | On demand | Marks a node as cordoned for maintenance — excluding it from voluntary task dispatch and healthy disruption-budget counts while keeping its lease alive. |
| `fleet-rollout` | On demand | Dispatches a cluster-wide rolling update (`-target-version V [-role writer\|reader\|keeper]`), enforcing disruption budgets and Keeper Raft `mntr` quorum gates. |
| `fleet-converge` | Every 5 min | Runs on the active coordinator writer (`writer-0` or vacancy-elected successor) to monitor fleet tasks, bump `epoch` on stalled executors, and re-dispatch within retry budgets. |
| `task-poll` | Every 2 min | Polls pending fleet tasks targeting this node, verifies the disruption budget, executes `refresh` or `rollout`, and writes an epoch-fenced completion status. |

### 4. Storage, Backups & Garbage Collection

| Subcommand | Default Timer | What It Accomplishes |
|---|---|---|
| `disk-autoscale` | Every 5 min | Monitors free space on data volumes and expands AWS EBS / Azure Managed Disks + Linux filesystems online before ClickHouse runs out of space. |
| `backup-daily` | Daily | Orchestrates full and incremental `clickhouse-backup` uploads to S3 or Azure Blob Storage on the dedicated `backup` replica. |
| `orphaned-gc` | Daily | Drops tables removed from the bundle schema and cleans up unattached, untagged cloud volumes past the configured retention window. |

### 5. Observability & Graceful Teardown

| Subcommand | Mode | What It Accomplishes |
|---|---|---|
| `keeper-health` | Every 1 min | Queries the ClickHouse Keeper 4-letter `mntr` port, validates quorum/leader state, and emits `chinit_keeper_up`. |
| `exporter` | Daemon (`:9150`) | Exposes Prometheus fleet metrics on `/metrics` and proxies local ClickHouse server metrics (`:9363`) on `/clickhouse-metrics`. |
| `spot-watch` | Every 2 s | Watches AWS Spot Instance Action / Azure Scheduled Events on `writer` and `reader` nodes and triggers an immediate graceful drain on preemption notice. |
| `drain` | On demand | Manually drains the node (`-reason`, `-timeout`): stops replication queues and distributed sends, drops replica metadata on a live peer, removes Private DNS, and releases the lease. |
| `drain-shutdown` | OS Shutdown | Invoked by `chinit-shutdown-drain.service` (`After=clickhouse-server`) during VM termination; automatically skips cleanup on normal OS reboots (`reboot.target`). |

---

## Common On-Call Diagnostic Recipes

```bash
# Preview what a boot or refresh would do without changing anything
chinit boot -spec /var/lib/clickhouse-bootstrap/chinit.yaml -dry-run

# Inspect the entire cluster's health and spec convergence as JSON
chinit fleet-status -json | jq .

# Manually trigger schema propagation and queue recovery on a lagging reader
sudo chinit schema-sync -retries 3 -prune-stale-self
sudo chinit queue-heal

# Check Keeper Raft quorum health from a keeper node
sudo chinit keeper-health && echo "Quorum healthy"
```

See [The Boot Process]({{ '/boot/' | relative_url }}) and [Maintenance Jobs]({{ '/maintenance/' | relative_url }}) for architectural details behind these commands, or jump to the [Operator's Guide]({{ '/guide/' | relative_url }}) for complete day-2 workflows.
