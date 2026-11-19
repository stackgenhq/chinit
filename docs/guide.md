---
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
title: "Operator's guide"
nav_title: "Operator's guide"
nav_order: 9
permalink: /guide/
mermaid: true
description: >-
  Day-2 operations runbook for chinit clusters: in-place config
  refreshes, cordoning, storage-loss recovery, debugging, and
  fleet-wide status.
---

# Operator's Guide

<p class="lead">How <code>chinit</code> turns fragile VM fleets into a deterministic, self-managing ClickHouse platform — plus practical day-2 runbooks for refreshes, rollouts, cordoning, and recovery.</p>

> Looking for contributor internals — Go package boundaries, retry policies, CR slot calculation, and the provider conformance suite? Jump to [Architecture]({{ '/architecture/' | relative_url }}).

---

## Eleven Failure Modes `chinit` Eliminates Out of the Box

Running a replicated analytical database on elastic cloud VMs means dealing with failure modes that never show up in local testing. Every subsystem in `chinit` exists to close a specific operational hazard:

| Real-World Cloud Failure Mode | How `chinit` Solves It |
|---|---|
| **Per-role bash scripts drift** across writers, readers, keepers, and backup nodes | One identical static Go binary runs everywhere; role behavior is driven purely by validated YAML (`chinit.yaml`) |
| **Transient cloud API throttling** kills `cloud-init`, while bad configs hang forever | Built-in retry engine classifies errors into `Retryable` (exponential backoff + jitter) vs. `Fatal` (fail-fast on SHA mismatch or IAM `AccessDenied`) |
| **Interrupted boots** re-run from scratch or claim a second identity | Convergent steps inspect real OS state first and persist completion to `/var/lib/chinit/state.json`, resuming cleanly mid-pipeline |
| **Cron scripts guess the wrong hostname** after DHCP or IMDS hiccups | Every background subcommand loads the node's settled identity from `state.json` — never re-deriving it |
| **Health-check kills bypass ASG lifecycle hooks**, stranding leases and Keeper metadata | `chinit-shutdown-drain.service` runs inside `systemd` shutdown (`After=clickhouse-server`), draining cleanly on scale-in, Spot, and health-check kills while skipping OS reboots |
| **Sudden hypervisor deaths** leave blackhole IPs in DNS and `remote_servers` | Every 5 minutes, `lease-renew` reconciles `/etc/hosts`, Private DNS `A` records, and `remote_servers` XML against active leases |
| **Replication queues wedge** on `NO_REPLICA_HAS_PART` or lost Keeper sessions | Hourly `queue-heal` restores read-only replicas and restarts/syncs stuck queues whenever background merges are idle |
| **Cluster maintenance stops** if `writer-0` is terminated during churn | **Vacancy election**: when `writer-0` has no active lease, the lowest-ordinal live writer automatically assumes coordinator duties |
| **Rolling updates lack a Kubernetes controller** to sequence changes | Epoch-fenced compare-and-swap task rows in the lease table (`fleet-converge` + `task-poll`) coordinate cluster-wide rollouts |
| **Voluntary rollouts take down the last healthy writer** | Disruption budgets (`min_healthy_writers`, `min_healthy_readers`) gate every task against live peer health status rows |
| **Re-imaged VMs hit `REPLICA_ALREADY_EXISTS`** against old Keeper metadata | `detect_storage_loss` + `sync_schema` (`HostCreateTables` with stale-self pruning) detect empty local disks under existing Keeper paths, re-register cleanly, and re-sync parts from peers |

---

## Cloud Detection & Portable Capabilities

When `chinit` starts on a VM, you don't even have to tell it which cloud it is running on. Detection follows a strict precedence chain:
`-cloud` flag → `CHINIT_CLOUD` env var → Linux DMI `sys_vendor` (`Amazon` / `Microsoft`) → IMDS probe.

Each cloud adapter implements four mandatory core capabilities and opts into rich cluster capabilities probed at runtime:

```go
// pkg/chinit/domain/port/provider.go
type Provider interface {
    Name() string
    MetadataProvider          // instance ID, region, private IP — mandatory
    port.SecretSource         // KMS / Key Vault secret resolution — mandatory
    ConfigSource              // SHA-256 verified bundle download — mandatory
}

// Optional capabilities probed dynamically:
type OrdinalClaimer  interface { ClaimOrdinal(...); RenewLease(...); ReleaseLease(...) }
type DNSRegistrar    interface { ResolveZoneID(...); UpsertARecord(...); DeleteARecord(...) }
type SpotSignals     interface { Watch(ctx) <-chan InterruptionNotice }
type StatusPublisher interface { PublishStatus(...); ListStatuses(...) }
```

If an optional capability isn't needed by the role, `chinit` degrades gracefully with a clear log message. If the spec *requires* a capability (such as `identity_mode: lease` without an `OrdinalClaimer`), `chinit` fails closed immediately rather than booting with a corrupt identity.

---

## Day-2 Operational Playbooks

### Playbook 1: Applying Configuration Changes In-Place (`--refresh`)
Want to tune ClickHouse memory profiles, add an allowed CIDR, or update `storage.xml` without replacing VMs?

1. Upload your updated Configuration Bundle to S3 / Azure Blob Storage and update `config_bundle.sha256` in `chinit.yaml`.
2. Either let the 10-minute `spec-sync` timer pick up the change automatically, or trigger an immediate in-place refresh:

```bash
sudo chinit boot --refresh
```

Unlike an initial boot, `--refresh`:
- **Disables step resume-skipping** so every XML config and `systemd` unit is freshly rendered and pruned.
- **Preserves node identity**: validates and renews the existing ordinal in `state.json` — never re-claiming a different slot.
- **Preserves Keeper Raft state**: never reformats metadata volumes or touches Keeper `myid` / coordination logs.
- **Restarts ClickHouse cleanly** once validated configs are on disk.

### Playbook 2: Coordinated Rolling Updates Across the Fleet
When rolling out a new ClickHouse Golden AMI or configuration version across an entire cluster, use `fleet-rollout` so nodes update sequentially behind your disruption budget:

```bash
# 1. Check current fleet health, versions, and cordon status
chinit fleet-status

# 2. Dispatch a rolling update for readers first, then writers, then keepers
sudo chinit fleet-rollout -target-version v0.1.0 -role reader
sudo chinit fleet-rollout -target-version v0.1.0 -role writer
sudo chinit fleet-rollout -target-version v0.1.0 -role keeper
```

During a `keeper` rollout, `chinit` queries `mntr` before every single node — verifying an active Raft leader and healthy voting quorum, and always rolling `keeper-0` last.

### Playbook 3: Taking a Node Out for Manual Maintenance (`cordon`)
Need to inspect hardware or run manual diagnostics on `writer-1` without triggering false rollout assumptions?

```bash
sudo chinit cordon writer-1
chinit fleet-status          # Shows writer-1 as CORDONED
# ... perform maintenance (lease-renew continues keeping its slot reserved) ...
sudo chinit uncordon writer-1
```

While cordoned, `writer-1` keeps its ordinal lease and DNS record, but is excluded from voluntary task dispatch and is not counted toward `min_healthy_writers` so rollouts won't simultaneously take down `writer-0`.

### Playbook 4: Replacing and Draining a Node Cleanly
If you want to retire a specific VM immediately ahead of an ASG scale-in:

```bash
sudo chinit drain -reason "manual-retirement" -timeout 2m
```

This stops local replication queues and distributed sends, drops the replica registration on a healthy peer, deletes its Private DNS `A` record, and frees the ordinal lease for the next VM.

---

## End-to-End Pipeline Summary

```
PRE-BOOT:
  hooks_pre_boot

PHASE 1 (parallel — errgroup):
  fetch_config_bundle ∥ os_optimizations ∥ require_binaries
  ∥ mount_data_disks ∥ settle_identity ∥ retrieve_passwords ∥ configure_tls

PHASE 2 (sequential — consumes Phase 1 outputs):
  install_static_configs → configure_peer_hosts → configure_server_env
  → render_node_config → configure_keeper → write_node_env
  → install_shutdown_drain → start_clickhouse → detect_storage_loss
  → bootstrap_schema → health_gate → prometheus_reload → install_units
  → configure_backup → configure_monitoring → configure_cloudwatch
  → install_spot_watch → boot_finished

POST-BOOT:
  hooks_post_boot
```

For deep dives into internal implementation, continue to [Architecture]({{ '/architecture/' | relative_url }}).
