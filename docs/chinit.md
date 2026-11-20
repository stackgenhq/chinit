<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# chinit — Operator's Guide

> Contributor-facing internals — package layers, pipeline architecture, identity contracts, and the conformance test suite — live in [chinit-architecture.md](./chinit-architecture.md). This page is the operational guide.

`chinit` is a single Go binary that bootstraps and manages a ClickHouse node on cloud VMs: it detects the cloud platform (AWS or Azure, extensible via a provider registry), downloads a SHA-256 verified configuration bundle, converges the node to a declarative spec, and installs recurring maintenance jobs as systemd units.

## Why It Exists

Bootstrapping a stateful, replicated database cluster on auto-scaling cloud VMs involves subtle failure modes and race conditions. Instances launch, scale in, and get replaced by health checks or spot interruptions — and every event must leave cluster identity and replication metadata consistent.

| Operational Challenge | How `chinit` Solves It |
|---|---|
| Node configuration varies by instance (identity, IP, role) while needing identical bootstrap logic | `chinit.yaml` is a typed, validated spec fetched from object storage and verified by SHA-256; the binary is identical across roles and clouds |
| Cloud APIs can fail transiently, while configuration errors should fail fast | Built-in retry engine with `Retryable` vs. `Fatal` error classification, exponential backoff with jitter, per-step deadlines, and `Critical` vs. `BestEffort` step handling |
| Re-running a partially completed boot from scratch is slow and can re-claim identity mid-flight | Convergent execution: each step inspects real machine state first, records completion in `state.json`, and resumes cleanly; bundle hash changes invalidate and re-render configs |
| Post-boot tasks (schema sync, replica sync, queue healing, lease renewal, backups, spot draining) must run reliably with the right node identity | Built-in subcommands (`schema-sync`, `replica-sync`, `queue-heal`, `lease-renew`, `keeper-health`, `dead-replica-gc`, `orphaned-gc`, `backup-daily`, `spot-watch`, `drain-shutdown`, `volume-release`) wired as one-line systemd units |
| Terminating instances must release their ordinal lease, DNS record, and replica registration even when external lifecycle hooks fail or are skipped | `chinit-shutdown-drain.service` runs `chinit drain-shutdown` inside systemd's shutdown transaction (`After=clickhouse-server`), covering scale-in, spot preemption, and health-check replacements while skipping normal reboots |
| A terminated node's stateless volumes can outlive it when `delete_on_termination` cannot act (a wedged instance) or drifts | `chinit-volume-release.service` runs `chinit volume-release` in the same shutdown transaction (`Before=clickhouse-server`), deleting the node's own non-root volumes after the server stops; `track_own_volumes` records them in `state.json` at boot as the mid-teardown fallback |
| Terminated nodes can leave stale DNS records, `/etc/hosts` entries, or `remote_servers` entries that stall cluster queries | Every 5-minute `lease-renew` tick reconciles `/etc/hosts`, private DNS `A` records, and rendered `remote_servers` configs against the live claims table |
| Replication queues can stall on parts belonging to a replaced peer (`NO_REPLICA_HAS_PART`) or lost Keeper sessions | The hourly `queue-heal` timer syncs or restarts stuck replica queues when merges are idle and restores read-only replicas after Keeper session recovery |
| Cluster-wide maintenance tasks gated on `writer-0` would stop running if `writer-0` is temporarily absent during churn | Vacancy election: if `writer-0` has no active lease, the lowest-ordinal live writer automatically assumes cluster maintenance duties |
| Rolling refreshes and AMI rollouts across VMs lack a central Kubernetes controller | Epoch-fenced fleet task rows in the claims table, coordinated by `fleet-converge` and executed by `task-poll` on each node |
| Voluntary rollouts could take down the last healthy writer | Disruption budgets enforce `min_healthy_writers` and `min_healthy_readers` using live status rows published by `lease-renew` |
| Re-imaged nodes claiming an existing ordinal have empty local disks while Keeper still holds old replica metadata | `detect_storage_loss` identifies the condition at boot, and `sync_schema` (`HostCreateTables` with `-prune-stale-self`) + `replica-sync` drop the stale registration and re-sync the replica cleanly |

## Cloud Detection and Adaptation

Cloud detection runs in order of precedence: `-cloud` flag → `CHINIT_CLOUD` environment variable → DMI `sys_vendor` signature (`Amazon` / `Microsoft`) → IMDS probe. The matched provider is constructed from the provider registry:

```go
// pkg/chinit/domain/port/provider.go
type Provider interface {
    Name() string
    MetadataProvider          // instance ID, region, private IP — required
    port.SecretSource         // secret resolution — required
    ConfigSource              // SHA-256 verified bundle fetch — required
}

// Optional capabilities probed at runtime:
type OrdinalClaimer interface { ClaimOrdinal(...); RenewLease(...); ReleaseLease(...) }
type DNSRegistrar interface   { ResolveZoneID(...); UpsertARecord(...); DeleteARecord(...) }
type SpotSignals interface    { Watch(ctx) <-chan InterruptionNotice }
```

If a cloud provider does not implement an optional capability, `chinit` logs a clear warning and continues rather than failing unexpectedly.

## The Boot Pipeline

`chinit boot` executes its convergent pipeline in two main phases:

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
  → bootstrap_schema → sync_schema → health_gate → prometheus_reload → install_units
  → configure_backup → configure_monitoring → configure_cloudwatch
  → install_spot_watch → boot_finished

POST-BOOT:
  hooks_post_boot
```

Once `settle_identity` completes, the node's identity (hostname, CR slot name, lease details, and DNS zone) is persisted to `/var/lib/chinit/state.json`. All subsequent steps and background subcommands read `state.json` directly.

### Shutdown Draining

Every writer and reader installs `chinit-shutdown-drain.service`, a systemd unit whose `ExecStop` runs `chinit drain-shutdown` during OS shutdown (ordered `After=clickhouse-server` so ClickHouse is still running when the drain executes):

```
stop replication queues → stop distributed sends → peer DROP REPLICA
→ DNS A-record DELETE → ordinal lease release → sweep expired peer DNS records
```

- **Reboots are preserved**: If `reboot.target` is active in `systemctl list-jobs`, `drain-shutdown` exits without releasing the lease or dropping the replica so the node retains its identity across reboots.
- **No systemd deadlock**: Because the drain runs inside systemd's shutdown transaction, it skips issuing a separate `systemctl stop clickhouse-server` call.
- **Covers every termination path**: Whether an instance is terminated by an ASG scale-in, an EC2/VMSS health check, a spot interruption, or a manual stop, the OS shutdown transaction runs the drain reliably.

### Shutdown Volume Release

Writers and readers also install `chinit-volume-release.service`, ordered `Before=clickhouse-server` so its `ExecStop` (`chinit volume-release`) runs after the server has stopped and the drain has released the identity:

```
reboot guard → role gate (writers/readers) → live-list own non-root volumes
→ force-detach + delete each (state-file tracked_volumes as the fallback list)
```

- **No orphaned EBS**: a wedged termination or a drifted `delete_on_termination` flag leaves no unattached volume billing until the daily orphan sweep finds it.
- **Persistent roles untouched**: keepers and the backup replica keep their standalone, re-attached volumes — the release is a stateless-role concern.
- **Reboot-aware**: the same `reboot.target` guard as the drain; a reboot keeps its volumes.

### In-Place Configuration Refresh (`chinit boot --refresh`)

To apply an updated spec or configuration bundle to a running node without replacing the VM:

```bash
chinit boot --refresh
```

How `--refresh` differs from an initial boot:

- **Re-evaluates all steps**: Resume-skipping is disabled so updated configuration files are rendered and applied even if steps were previously marked `done`.
- **Preserves node identity**: The existing identity in `state.json` is validated and renewed, never re-claimed from scratch.
- **Preserves Keeper state**: Keeper coordination directories and metadata disks are never wiped during a refresh.
- **Restarts ClickHouse cleanly**: Restarts the service after rendering so configuration changes take effect.

## Example Node Spec

`chinit.yaml` is rendered per role, uploaded to the configuration bundle prefix, and fetched by the node at boot:

```yaml
role: reader
identity_mode: lease        # static (IP-octet map) or lease (ordinal claim)
cluster_name: demo
config_bundle:
  bucket: data-bucket
  prefix: bootstrap/clickhouse
  sha256: <config_files_hash>
disks:
  metadata_label: ch-metadata
secrets:
  default_password: demo-default
region: us-east-1
```

## Installing the Binary

```bash
CHINIT_VERSION=v0.1.0
curl -fsSL -o chinit \
  "https://github.com/stackgenhq/chinit/releases/download/${CHINIT_VERSION}/chinit-linux-arm64"
curl -fsSL "https://github.com/stackgenhq/chinit/releases/download/${CHINIT_VERSION}/chinit-SHA256SUMS" \
  | grep linux-arm64 | sed 's|chinit-linux-arm64|chinit|' | sha256sum -c -
sudo install -m 0755 chinit /usr/local/bin/chinit
```

Releases are built automatically from `v0.*` tags via GoReleaser (`.goreleaser.chinit.yaml`), producing stripped static binaries for `linux` and `darwin` (`arm64` and `amd64`) along with `chinit-SHA256SUMS`.

## Repository Layout

```
cmd/chinit/                 CLI entry point
pkg/chinit/app/             Flag parsing, subcommands, provider wiring
pkg/chinit/domain/
  model/                    BootSpec, NodeIdentity, and state models
  port/                     Provider capability interfaces and registry
  service/                  Retry engine, boot pipeline, and state store
  fail/                     Retryable vs. Fatal error classification
pkg/chinit/infra/
  detect/                   Cloud detection (flag, env, DMI, IMDS)
  operator/                 ClickHouse config rendering, schema, and health adapters
  system/                   Disks, OS tuning, systemd units, and XML validation
  aws/                      AWS adapters (IMDS, Secrets Manager, S3, DynamoDB, Route 53, Spot)
  azure/                    Azure adapters (IMDS, Key Vault, Blob, Table Storage, Private DNS)
  fakes/                    In-memory test doubles for the provider conformance suite
```
