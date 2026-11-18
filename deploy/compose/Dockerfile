# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

ARG CLICKHOUSE_VERSION=26.8.36.3

FROM --platform=$BUILDPLATFORM golang:1.27 AS builder

ARG TARGETOS=linux
ARG TARGETARCH
ARG CHINIT_VERSION=compose-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/stackgenhq/chinit/pkg/chinit/app.Version=${CHINIT_VERSION}" -o /out/chinit ./cmd/chinit

FROM clickhouse/clickhouse-server:${CLICKHOUSE_VERSION}

ARG TARGETARCH
ARG OTEL_COLLECTOR_VERSION=0.118.0

ENV DEBIAN_FRONTEND=noninteractive \
    container=docker

# Layer systemd + minimal runtime utilities onto the official ClickHouse image.
RUN apt-get update && apt-get install -y --no-install-recommends \
        systemd \
        systemd-sysv \
        dbus \
        ca-certificates \
        curl \
        openssl \
        iproute2 \
        procps \
        less \
    && rm -rf /var/lib/apt/lists/*

# Mask units that fail or stall inside containers.
RUN systemctl mask \
        dev-hugepages.mount \
        sys-fs-fuse-connections.mount \
        systemd-remount-fs.service \
        getty@.service \
        console-getty.service \
        systemd-udevd.service \
        systemd-logind.service \
        systemd-resolved.service \
    || true

# Mirror the golden AMI: chinit owns starting clickhouse-server / clickhouse-keeper
# after seeding identity, TLS, server env, and rendered configs.
RUN systemctl disable clickhouse-server 2>/dev/null || true \
    && systemctl mask clickhouse-server 2>/dev/null || true \
    && systemctl disable clickhouse-keeper 2>/dev/null || true

# Install OpenTelemetry Collector Contrib at the exact path configureMonitoring expects.
RUN curl -fsSL --retry 3 --retry-delay 3 \
        "https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${OTEL_COLLECTOR_VERSION}/otelcol-contrib_${OTEL_COLLECTOR_VERSION}_linux_${TARGETARCH}.tar.gz" \
        -o /tmp/otel.tar.gz \
    && tar xzf /tmp/otel.tar.gz -C /tmp \
    && mv /tmp/otelcol-contrib /usr/local/bin/otelcol \
    && chmod 0755 /usr/local/bin/otelcol \
    && rm -f /tmp/otel.tar.gz

COPY --from=builder /out/chinit /usr/local/bin/chinit
RUN chmod 0755 /usr/local/bin/chinit

# Bake default local Compose bundle, secrets, and per-role specs into the image
# so a standalone copy-pasted compose.yaml works with zero extra local files,
# while still allowing bind-mounts to override any of these paths.
COPY deploy/compose/bundle /etc/chinit/bundle
COPY deploy/compose/secrets /etc/chinit/secrets
COPY deploy/compose/specs /etc/chinit/specs
RUN chmod -R a+rX /etc/chinit/bundle /etc/chinit/specs \
    && chmod 0700 /etc/chinit/secrets \
    && chmod 0600 /etc/chinit/secrets/*

COPY deploy/compose/entrypoint.sh /usr/local/bin/compose-entrypoint.sh
RUN chmod 0755 /usr/local/bin/compose-entrypoint.sh

RUN printf '%s\n' \
    '[Unit]' \
    'Description=chinit node bootstrapper (Docker Compose)' \
    'After=network-online.target' \
    'Wants=network-online.target' \
    '' \
    '[Service]' \
    'Type=oneshot' \
    'RemainAfterExit=yes' \
    'ExecStart=/usr/local/bin/chinit boot -cloud compose -spec /var/lib/clickhouse-bootstrap/chinit.yaml' \
    'StandardOutput=journal+console' \
    'StandardError=journal+console' \
    '' \
    '[Install]' \
    'WantedBy=multi-user.target' \
    > /etc/systemd/system/chinit-boot.service \
    && systemctl enable chinit-boot.service

STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/usr/local/bin/compose-entrypoint.sh"]
