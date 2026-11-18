#!/bin/bash
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

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
