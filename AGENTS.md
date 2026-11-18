# AGENTS.md — Engineering Principles & Guardrails for `chinit`

This document captures the architectural rules, Go idioms, and hard-won ClickHouse/cloud invariants for `chinit`. Follow these principles on every change so regressions are caught before a golden AMI ever boots.

---

## 1. Architecture: Domain-Driven Design (DDD) & Hexagonal Boundaries

`chinit` follows a strict **inward dependency rule**:

```
cmd/chinit  ──▶  pkg/chinit/app  ──▶  pkg/chinit/domain/service  ──▶  pkg/chinit/domain/{model,port,fail}
                                                ▲
                                                │ (implements ports)
                                     pkg/chinit/infra/{aws,azure,keeper,operator,system}
```

- **`pkg/chinit/domain/model`**: Pure value objects and aggregates (`BootSpec`, `NodeIdentity`, `PersistentEpoch`). Zero cloud SDK, OS, or systemd dependencies.
- **`pkg/chinit/domain/port`**: Narrow capability interfaces (`Provider`, `OrdinalClaimer`, `LeaseLister`, `DNSRegistrar`, `ClickHouseDataStore`, `InstanceLifecycle`). Optional cloud capabilities are probed via `port.As<Capability>(provider)` and degrade loudly with a warning rather than failing silently.
- **`pkg/chinit/domain/service`**: Cohesive domain services and reconcilers (`Boot`, `LeaseRecovery`, `PersistentEpochReconciler`, `SelfFenceService`, `ConvergePeerHosts`).
  - Keep filesystem/process side effects behind narrow store interfaces or package-level test seams (e.g., `clickhouseDataDir`, `serverUUIDPath`, `peerHostsPath`, `execClientFn`, `runCommandFn`).
- **`pkg/chinit/app`**: Thin CLI subcommand coordinators (`leaseRenewCoordinator`, `runBoot`, `runBackupDaily`) that wire state loading, provider construction, and domain services. Do **not** embed business policies or multi-step raw file/SQL logic directly in `app/` handlers.
- **`pkg/chinit/infra/*`**: Cloud SDK adapters (AWS/Azure), Altinity operator config renderers (`infra/operator`), Keeper TCP probes, and Linux/systemd command execution (`infra/system`). **Never import AWS/Azure SDKs outside `pkg/chinit/infra/<cloud>`.**

---

## 2. Go Craftsmanship: Libraries & Cyclomatic Complexity

- **Prefer standard library and existing `go.mod` dependencies** over hand-rolling common logic:
  - UUID generation, parsing, and formatting: `github.com/google/uuid` (`uuid.FromBytes`, `uuid.Parse`, `u.String()`), not manual `fmt.Sprintf("%08x-...")`.
  - Structured state/markers on disk: `encoding/json` (`json.Marshal` / `json.Unmarshal`), not ad-hoc delimiter parsing (`strings.Cut(s, "|")`).
  - Comparisons, slices, concurrency, errors: `bytes.Equal`, `slices`, `maps`, `errors.Is` / `errors.As`, `golang.org/x/sync/errgroup`.
- **Keep cyclomatic complexity low**:
  - Break multi-stage flows (like `runLeaseRenew`) into small coordinator structs with single-purpose methods (`<= 30–40` lines, early returns, flat nesting).
  - Extract predicate helpers (`usesLeaseInventory()`, `driftReason()`) and switch/policy objects instead of deep `if/else` trees.
- **Error classification is contractual**:
  - Wrap unrecoverable config/invariant errors with `fail.Fatal(err)` so the retry engine aborts immediately rather than masking a bug for 15 minutes.
  - Leave transient infrastructure/network errors unwrapped so the step's retry policy (`apiRetry`, `bootWait`, `ebsAttachWait`, `claimSweep`) retries cleanly.

---

## 3. Production Traps & Invariants (Do Not Repeat These)

1. **ClickHouse `/var/lib/clickhouse/uuid` MUST be exact 36 bytes (no trailing `\n`)**:
   - ClickHouse `ServerUUID::load` (`src/Core/ServerUUID.cpp`) runs `readUUIDText()` followed immediately by `assertEOF()`. A trailing newline triggers `Code: 27 CANNOT_PARSE_INPUT_ASSERTION_FAILED`, causing ClickHouse to overwrite the file with a random UUIDv4 and break CAS ownership (`Code: 246`).
2. **Distro `clickhouse-server.service` vs `chinit-boot` race**:
   - The golden AMI (`images/aws/clickhouse.pkr.hcl`) must keep `clickhouse-server` disabled and **masked** at bake time, and `start_clickhouse` must `unmask` -> `enable` -> `start` only **after** `configure_server_env` seeds `/var/lib/clickhouse/uuid` and `CH_CAS_SERVER_ROOT_ID`.
3. **CAS lineage protection (never overwrite UUID on a stateful node)**:
   - Altinity Antalya CAS anchors each `cas_server_root_id` to `server_uuid` in S3 (`gc/server-roots/<id>/owner`). Rewriting `/var/lib/clickhouse/uuid` on a node that already has `first_boot_done` and non-empty `/var/lib/clickhouse/store` causes `Code: 246` on the very next start.
4. **Persistent EBS bucket/epoch drift (`backup-0`)**:
   - Unlike ASG writers/readers (ephemeral EBS), `backup-0` re-attaches persistent EBS metadata volumes across replacements. Switching `s3_data_bucket` (non-CAS ↔ CAS bucket) or downgrading ClickHouse (e.g. `26.8` → `26.6`, where `PackedFilesReader` throws `Code: 287 Unknown format (1) of packed data`) leaves broken stubs referencing missing keys (`Code: 499`, `Code: 107`, `Code: 231` > 100 broken parts).
   - `PersistentEpochReconciler` (`boot_epoch.json`) must run **before** `clickhouse-server` starts and reset stale local metadata/caches (preserving `uuid`) on bucket switches or legacy non-CAS residue, and `reconcileStoragePolicyMetadata` must run **before** `systemctl start/restart clickhouse-server`.
5. **Static nodes in lease fleets (`backup-0`)**:
   - `RoleBackup` has `identity_mode: static`, yet writers/readers use DynamoDB leases. `clusterHostsWithSource()` must query `ListActiveClaims` whenever `claimer_table_name` + `writer_max > 0` are configured; falling back to `staticSpecInventory` writes vacant slots (`writer-4..8`) into `remote_servers.xml` and floods logs with `Code: 198 DNS_ERROR`.
6. **Flush ClickHouse DNS cache when `/etc/hosts` changes**:
   - Updating `/etc/hosts` via `ConvergePeerHosts` is insufficient on its own if ClickHouse's `DNSResolver` already cached a terminated peer's IP (`:9010` timeouts). Issue `SYSTEM DROP DNS CACHE` whenever `/etc/hosts` bytes change.
7. **Resumed `chinit boot` skips `settle_identity`**:
   - Always hydrate identity from `/var/lib/chinit/state.json` (`Boot.HydrateIdentityFromState`) before `engine.Run` so `configure_keeper` and `bootstrap_schema` never observe an empty hostname.
8. **HTTP responses & replica sync**:
   - Drain HTTP response bodies to `EOF` before `Close()` (`Code: 210 Broken pipe` / `Code: 236`).
   - Use `SYSTEM SYNC REPLICA ... LIGHTWEIGHT` (bounded timeout) rather than full sync (`Code: 159 TIMEOUT_EXCEEDED` behind heavy background merges).
9. **CI / Golden AMI release pipeline**:
   - Never add a `push` trigger on `images/aws/**` to `.github/workflows/build-golden-ami.yml`; AMI builds must chain from `Release chinit` (`workflow_run`) or `workflow_dispatch` so the AMI always embeds the freshly published release version rather than a stale `main` pin.

---

## 4. Mandatory Verification Before Commit/Push

```bash
gofmt -w .
go vet ./...
go test ./... -count=1
golangci-lint run ./...
```
If modifying `images/aws/*.pkr.hcl`, also validate locally:
```bash
packer init images/aws && packer validate images/aws
```
