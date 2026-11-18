#!/bin/bash
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

# If no spec was bind-mounted at /var/lib/clickhouse-bootstrap/chinit.yaml,
# stage the baked-in spec for CHINIT_ROLE (keeper | writer | reader) so a
# standalone copy-pasted compose.yaml runs with zero extra host files.
if [[ ! -f /var/lib/clickhouse-bootstrap/chinit.yaml && -n "${CHINIT_ROLE:-}" ]]; then
    spec_src="/etc/chinit/specs/${CHINIT_ROLE}.yaml"
    if [[ -f "$spec_src" ]]; then
        mkdir -p /var/lib/clickhouse-bootstrap
        cp "$spec_src" /var/lib/clickhouse-bootstrap/chinit.yaml
    fi
fi

# Export container CHINIT_* environment variables into systemd's manager
# environment so chinit-boot.service and all chinit timer/service units
# receive them.
mkdir -p /etc/systemd/system.conf.d
{
    echo "[Manager]"
    while IFS='=' read -r name val; do
        case "$name" in
            CHINIT_*)
                printf 'DefaultEnvironment="%s=%s"\n' "$name" "$val"
                ;;
        esac
    done < <(env)
} > /etc/systemd/system.conf.d/10-compose-env.conf

exec /lib/systemd/systemd
