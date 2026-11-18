# Local Docker Compose Cluster for `chinit` (`-cloud compose`)

Run a multi-node ClickHouse + ClickHouse Keeper cluster locally with full **systemd** unit and timer parity (`chinit-boot.service`, `clickhouse-server`, `clickhouse-keeper`, `otelcol.service`, `chinit-exporter.service`, `span-ship.timer`, `lease-renew.timer`, `schema-sync.timer`, `chinit-shutdown-drain.service`).

## Topology

| Service | Role | Identity Mode | IP | Notes |
| --- | --- | --- | --- | --- |
| `otel-collector` | OTLP HTTP sink | — | `10.142.0.5` | Receives logs, metrics, `chinit.boot` traces, and `span-ship` ClickHouse spans on `:4318` |
| `keeper-0` | `keeper` | `static` | `10.142.0.20` | Runs `clickhouse-keeper` via systemd |
| `writer-0` | `writer` | `lease` | `10.142.0.10` | Claims `writer-0` from shared lease volume; ports `8123`, `9000`, `9150` |
| `reader-0` | `reader` | `lease` | `10.142.0.30` | Claims `reader-0` from shared lease volume; ports `18123`, `19150` |

## Quick Start

```bash
cd deploy/compose
docker compose up --build -d
```

Watch boot convergence and systemd units:

```bash
docker compose exec writer-0 journalctl -u chinit-boot.service --no-pager
docker compose exec writer-0 systemctl list-timers --all
docker compose exec writer-0 systemctl status clickhouse-server otelcol chinit-exporter
```

## Testing New Telemetry & Span Shipping

1. Run a query on `writer-0` and check replication on `reader-0`:

   ```bash
   docker compose exec writer-0 clickhouse-client --user admin --password compose-admin-pass \
     --query "INSERT INTO default.events (source, message) VALUES ('compose', 'hello chinit')"

   docker compose exec reader-0 clickhouse-client --user admin --password compose-admin-pass \
     --query "SELECT * FROM default.events"
   ```

2. Trigger or wait for `span-ship.timer` (runs every 30s) and inspect the OTLP collector output:

   ```bash
   docker compose exec writer-0 systemctl start span-ship.service
   docker compose logs --tail=80 otel-collector
   ```

3. Inspect `chinit` exporter metrics:

   ```bash
   curl -s http://localhost:9150/metrics | head -n 30
   ```

## Acceptance Test (E2E)

The one-command gate for the whole stack — the same check CI runs on every PR:

```bash
go test -tags acceptance ./acceptance_test/compose/ -timeout 30m -v
```

The suite (a Ginkgo suite, gated behind the `acceptance` build tag so a plain `go test ./...` never boots docker) builds the `chinit-node` image from the **current checkout**, boots the cluster on fresh volumes, and verifies:

1. Keeper answers `ruok → imok`
2. `chinit-boot` finished on writer-0 and reader-0 (marker + live SQL ping)
3. The `compose-demo` cluster lists both replicas on both nodes
4. **A row written on writer-0 arrives on reader-0** (ReplicatedMergeTree replication — the core promise)
5. The `events` replica is writable with all replicas active

Flags (via `go test -args`): `-args -keep-cluster` leaves the cluster up on success, `-args -reuse-cluster` re-checks a running cluster without rebuilding; a failed run always leaves the cluster up for inspection. A failure prints the compose status and the failing node's journals.

## Teardown

```bash
docker compose down -v
```
