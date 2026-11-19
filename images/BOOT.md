<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Node Boot Procedure

This document describes what happens from instance launch to `boot-finished` for each node role (`writer`, `reader`, `keeper`, and `backup`) on AWS and Azure. The `chinit` binary is pre-installed in the golden image, and instance `user_data` / `custom_data` is a small (~1.5 KB) wrapper script.

## Node Roles and Identity Modes

| Role | Compute Shape | Identity Mode | Assignment |
|---|---|---|---|
| `writer` | Auto Scaling Group / VMSS (or fixed instances) | **lease** (or static IP offset) | Claims `writer-N` from the lease table (or `.10+` subnet offset) |
| `reader` | Auto Scaling Group / Spot VMSS | **lease** | Claims `reader-N` from the lease table |
| `keeper` | Fixed instances | **static** | Derived from `.20+` subnet IP offset |
| `backup` | Single fixed instance | **static** | Derived from `.30` subnet IP offset (`backup-0`) |

## Stage 0: User-Data Wrapper (~1 s)

1. Cloud-init runs the wrapper script from `user_data` (AWS) or `custom_data` (Azure).
2. The wrapper verifies that `/usr/local/bin/chinit` is present on the golden image.
3. The wrapper downloads `/var/lib/clickhouse-bootstrap/chinit.yaml` from the object storage bundle prefix (with bounded retries in case storage or managed identity is still initializing).
4. The wrapper writes and starts `chinit-boot.service` (`chinit boot -spec /var/lib/clickhouse-bootstrap/chinit.yaml -cloud <aws|azure>`).

From this point onward, `chinit` runs the same Go bootstrap pipeline on every cloud, using the detected cloud provider adapter.

## The Convergent Boot Pipeline

Steps marked 🔒 are **Critical** (a failure aborts the boot so systemd or the Auto Scaling Group can retry or replace the instance). Steps marked ⚠️ are **BestEffort** (logs a warning and continues). Every step checks current system state first, records completion in `/var/lib/chinit/state.json`, and skips completed work on re-run unless `config_bundle.sha256` has changed.

| Phase | Step | Criticality | Description |
|---|---|---|---|
| Pre | `hooks_pre_boot` | 🔒 | Runs optional `pre_boot` lifecycle hooks. |
| Phase 1 | `fetch_config_bundle` | 🔒 | Downloads the config bundle from S3 or Azure Blob Storage and verifies its SHA-256 digest against `config_bundle.sha256`. |
| Phase 1 | `os_optimizations` | 🔒 | Disables Transparent Huge Pages (THP) and configures `fs.file-max`, `limits.conf`, and systemd `LimitNOFILE`. |
| Phase 1 | `require_binaries` | 🔒 | Verifies `clickhouse-server`, `clickhouse-client`, `chinit`, and cloud CLI binaries are installed. |
| Phase 1 | `mount_data_disks` | 🔒 | Mounts the metadata disk by filesystem `LABEL` (or platform device path on first boot), formatting blank volumes only. Cache and keeper disks degrade gracefully to local directories if absent. |
| Phase 1 | `settle_identity` | 🔒 | Claims an ordinal lease (`writer-N`, `reader-N`) or derives a static hostname from the IP octet, sets the system hostname, registers the private DNS `A` record, and saves identity to `state.json`. |
| Phase 1 | `retrieve_passwords` | 🔒 | Resolves secret names from Secrets Manager or Key Vault into `0600` files before config rendering. |
| Phase 1 | `configure_tls` | 🔒 | Installs TLS certificates from secrets or generates a self-signed certificate pair for interserver replication. |
| Phase 2 | `install_static_configs` | 🔒 | Validates and installs `network.xml`, `ssl.xml`, `storage.xml`, and the backup replica MergeTree overlay (on `backup` nodes only). |
| Phase 2 | `configure_peer_hosts` | 🔒 | Populates `/etc/hosts` with peer short names so replication fetches resolve reliably. |
| Phase 2 | `configure_server_env` | 🔒 | Writes `/etc/default/clickhouse-server` and the systemd environment drop-in (`CH_S3_ENDPOINT`). |
| Phase 2 | `render_node_config` | 🔒 | Renders ClickHouse host sections, cluster inventory, macros, and `users.d` profiles in-process. |
| Phase 2 | `configure_keeper` | 🔒 | On `keeper` nodes, renders the single-file Keeper config, reconciles the 1-based `myid` Raft ID, disables `clickhouse-server`, and enables `clickhouse-keeper`. |
| Phase 2 | `write_node_env` | ⚠️ | Writes `/var/lib/clickhouse-bootstrap/node-env` for shell compatibility. |
| Phase 2 | `install_shutdown_drain` | 🔒 | Installs `chinit-shutdown-drain.service` immediately after identity settlement so any termination releases the lease. |
| Phase 2 | `start_clickhouse` | 🔒 | Enables and starts `clickhouse-server`, waits for `/ping`, and checks S3 disk health (skipped on `keeper` nodes). |
| Phase 2 | `detect_storage_loss` | ⚠️ | Detects when a re-imaged node claims an ordinal that has existing Keeper replica registrations but an empty local data directory. |
| Phase 2 | `bootstrap_schema` | ⚠️ | Applies `bootstrap-ddl.sql` idempotently (`CREATE ... IF NOT EXISTS`). |
| Phase 2 | `health_gate` | ⚠️ | Probes cluster health across live peers. Non-fatal so a node booting during peer maintenance still joins. |
| Phase 2 | `prometheus_reload` | ⚠️ | Reloads config and verifies the `:9363/metrics` endpoint. |
| Phase 2 | `install_units` | 🔒 | Installs and enables systemd units and timers for recurring maintenance subcommands. |
| Phase 2 | `configure_backup` | 🔒 | Configures `clickhouse-backup` and enables the daily backup timer on the backup replica. |
| Phase 2 | `configure_monitoring` | ⚠️ | Configures OpenTelemetry Collector or Fluent Bit when `log_shipping.endpoint` is set. |
| Phase 2 | `configure_cloudwatch` | ⚠️ | Configures the CloudWatch agent on AWS nodes. |
| Phase 2 | `install_spot_watch` | ⚠️ | Installs `chinit-spot-watch.service` on writer + reader nodes to drain on spot interruption notices (both ASG roles carry a replica registration, ordinal lease and DNS record the notice window must release). |
| Phase 2 | `boot_finished` | 🔒 | Writes `/var/lib/cloud/instance/boot-finished` and logs step timing summary. |
| Post | `hooks_post_boot` | ⚠️ | Runs optional `post_boot` lifecycle hooks. |

## Role Walkthroughs

### Lease-Identity Nodes (`writer` / `reader`)

```
launch (ASG / VMSS, spot or on-demand)
  └─ wrapper: verify chinit → fetch chinit.yaml → start chinit-boot.service
      ├─ Phase 1 (parallel): fetch bundle, tune OS, check binaries, mount disks,
      │    claim ordinal lease (e.g. reader-3), fetch secrets, configure TLS
      ├─ Phase 2 (sequential): install static XMLs, configure peer /etc/hosts,
      │    render ClickHouse config with settled identity, install shutdown drain,
      │    start clickhouse-server, apply bootstrap DDL, run health gate,
      │    install maintenance timers (lease-renew, schema-sync, replica-sync, …)
      └─ boot_finished
```

After boot, the `lease-renew` timer extends the node's lease every 5 minutes and converges `/etc/hosts`, private DNS records, and `remote_servers`.

### Static-Identity Backup Replica (`backup`)

```
launch (fixed instance at .30 subnet offset)
  └─ wrapper → chinit boot
      ├─ Phase 1: derive backup-0 from the IP octet and register DNS
      ├─ Phase 2: install static configs + backup-replica-settings.xml overlay,
      │    render config at CR slot 01-(writer_max + reader_max), start server,
      │    install replica-sync, schema-bootstrap, and clickhouse-backup-daily timers
      └─ boot_finished
```

### Keeper Nodes (`keeper`)

```
launch (fixed instance at .20+ subnet offset)
  └─ wrapper → chinit boot
      ├─ Phase 1: mount keeper raft disk, derive keeper-N from IP octet
      ├─ Phase 2: configure_keeper renders single-file config, preserves or seeds
      │    1-based myid, disables clickhouse-server, starts clickhouse-keeper,
      │    and installs the keeper-health timer
      └─ boot_finished
```

## Spot Interruption and Shutdown Draining

On reader nodes, `chinit spot-watch` polls the cloud spot interruption endpoint (EC2 IMDS `spot/instance-action` or Azure Scheduled Events `Preempt`). In addition, every writer and reader runs `chinit-shutdown-drain.service` (`chinit drain-shutdown`) during OS shutdown (`After=clickhouse-server`). Both execute the same drain sequence:

1. `SYSTEM STOP REPLICATION QUEUES` and `SYSTEM STOP DISTRIBUTED SENDS`
2. Drop the node's replica registration on a live peer (falling back to local drop and hourly `dead-replica-gc` if unreachable)
3. Delete the private DNS `A` record and release the ordinal lease in the claims table

## Operational Cheat Sheet

```bash
# View settled identity and step completion status
sudo cat /var/lib/chinit/state.json

# Re-run boot (idempotent; skips completed steps)
sudo chinit boot -spec /var/lib/clickhouse-bootstrap/chinit.yaml

# Dry-run the boot plan
chinit boot -spec /var/lib/clickhouse-bootstrap/chinit.yaml -dry-run

# Run maintenance subcommands manually
sudo chinit schema-sync -retries 3 -prune-stale-self
sudo chinit keeper-health
sudo chinit dead-replica-gc
sudo chinit lease-renew
sudo chinit fleet-status

# Print binary version
chinit version
```
