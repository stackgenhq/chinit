# Local Docker Compose Cluster for `chinit` (`-cloud compose`)

Run a multi-node ClickHouse + ClickHouse Keeper cluster locally with full **systemd** unit and timer parity (`chinit-boot.service`, `clickhouse-server`, `clickhouse-keeper`, `otelcol.service`, `chinit-exporter.service`, `span-ship.timer`, `lease-renew.timer`, `schema-sync.timer`, `chinit-shutdown-drain.service`).

The prebuilt multi-arch node image (`ghcr.io/stackgenhq/chinit-node:latest`, `linux/amd64` + `linux/arm64`) is built and published to GitHub Container Registry by [`.github/workflows/publish-compose-image.yaml`](../../.github/workflows/publish-compose-image.yaml) and comes with the default configuration bundle (`/etc/chinit/bundle`), secrets (`/etc/chinit/secrets`), and per-role specs (`/etc/chinit/specs/{keeper,writer,reader}.yaml`) baked in.

## Topology

| Service | Role | Identity Mode | IP | Notes |
| --- | --- | --- | --- | --- |
| `otel-collector` | OTLP HTTP sink | — | `10.142.0.5` | Receives logs, metrics, `chinit.boot` traces, and `span-ship` ClickHouse spans on `:4318` |
| `keeper-0` | `keeper` | `static` | `10.142.0.20` | Runs `clickhouse-keeper` via systemd |
| `writer-0` | `writer` | `lease` | `10.142.0.10` | Claims `writer-0` from shared lease volume; ports `8123`, `9000`, `9150` |
| `reader-0` | `reader` | `lease` | `10.142.0.30` | Claims `reader-0` from shared lease volume; ports `18123`, `19150` |

## Option A: Copy-Paste Standalone `compose.yaml` (No Clone Required)

Save the following file as `compose.yaml` in any empty directory and run `docker compose up -d --wait`:

```yaml
x-chinit-node: &chinit-node
  image: ghcr.io/stackgenhq/chinit-node:latest
  privileged: true
  cgroup: host
  stop_signal: SIGRTMIN+3
  stop_grace_period: 30s
  tmpfs:
    - /run
    - /run/lock
  ulimits:
    nofile:
      soft: 262144
      hard: 262144

services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.118.0
    command:
      - |
        --config=yaml:
        receivers:
          otlp:
            protocols:
              http:
                endpoint: 0.0.0.0:4318
        processors:
          batch: {}
        exporters:
          debug:
            verbosity: detailed
        service:
          pipelines:
            logs:
              receivers: [otlp]
              processors: [batch]
              exporters: [debug]
            metrics:
              receivers: [otlp]
              processors: [batch]
              exporters: [debug]
            traces:
              receivers: [otlp]
              processors: [batch]
              exporters: [debug]
    ports:
      - "4318:4318"
    networks:
      chnet:
        ipv4_address: 10.142.0.5

  keeper-0:
    <<: *chinit-node
    hostname: keeper-0
    environment:
      CHINIT_CLOUD: compose
      CHINIT_ROLE: keeper
      CHINIT_REGION: local
      CHINIT_PRIVATE_IP: 10.142.0.20
      CHINIT_INSTANCE_ID: compose-keeper-0
    volumes:
      - /sys/fs/cgroup:/sys/fs/cgroup:rw
      - ch-leases:/var/lib/chinit-cluster
      - keeper-0-data:/var/lib/clickhouse-keeper
      - keeper-0-ch:/var/lib/clickhouse
    networks:
      chnet:
        ipv4_address: 10.142.0.20
        aliases:
          - keeper-0
          - keeper-0.ch.local
    healthcheck:
      test: ["CMD-SHELL", "test -f /var/lib/cloud/instance/boot-finished && systemctl is-active --quiet clickhouse-keeper"]
      interval: 5s
      timeout: 5s
      retries: 24
      start_period: 15s

  writer-0:
    <<: *chinit-node
    hostname: writer-node-0
    depends_on:
      otel-collector:
        condition: service_started
      keeper-0:
        condition: service_healthy
    environment:
      CHINIT_CLOUD: compose
      CHINIT_ROLE: writer
      CHINIT_REGION: local
      CHINIT_PRIVATE_IP: 10.142.0.10
      CHINIT_INSTANCE_ID: compose-writer-0
    volumes:
      - /sys/fs/cgroup:/sys/fs/cgroup:rw
      - ch-leases:/var/lib/chinit-cluster
      - writer-0-data:/var/lib/clickhouse
    ports:
      - "8123:8123"
      - "9000:9000"
      - "9150:9150"
    networks:
      chnet:
        ipv4_address: 10.142.0.10
    healthcheck:
      test: ["CMD-SHELL", "test -f /var/lib/cloud/instance/boot-finished && curl -sf http://localhost:8123/ping"]
      interval: 5s
      timeout: 5s
      retries: 30
      start_period: 20s

  reader-0:
    <<: *chinit-node
    hostname: reader-node-0
    depends_on:
      otel-collector:
        condition: service_started
      keeper-0:
        condition: service_healthy
    environment:
      CHINIT_CLOUD: compose
      CHINIT_ROLE: reader
      CHINIT_REGION: local
      CHINIT_PRIVATE_IP: 10.142.0.30
      CHINIT_INSTANCE_ID: compose-reader-0
    volumes:
      - /sys/fs/cgroup:/sys/fs/cgroup:rw
      - ch-leases:/var/lib/chinit-cluster
      - reader-0-data:/var/lib/clickhouse
    ports:
      - "18123:8123"
      - "19150:9150"
    networks:
      chnet:
        ipv4_address: 10.142.0.30
    healthcheck:
      test: ["CMD-SHELL", "test -f /var/lib/cloud/instance/boot-finished && curl -sf http://localhost:8123/ping"]
      interval: 5s
      timeout: 5s
      retries: 30
      start_period: 20s

networks:
  chnet:
    driver: bridge
    ipam:
      config:
        - subnet: 10.142.0.0/24

volumes:
  ch-leases:
  keeper-0-data:
  keeper-0-ch:
  writer-0-data:
  reader-0-data:
```

Boot the cluster:

```bash
docker compose up -d --wait
```

## Option B: Run or Build from a Repository Checkout

When working inside this repository, `deploy/compose/docker-compose.yaml` bind-mounts `./specs`, `./bundle`, and `./secrets` so local edits take effect immediately (and can rebuild the image from your current Go checkout with `--build`):

```bash
cd deploy/compose
docker compose up --build -d
```

Watch boot convergence and systemd units:

```bash
docker compose exec writer-0 journalctl -u chinit-boot.service --no-pager
docker compose exec writer-0 systemctl list-timers --all
docker compose exec writer-0 systemctl status clickhouse-server otelcol chinit-exporter
docker compose exec writer-0 chinit fleet-status
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
