#!/bin/bash
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# chinit stage-0: Azure custom_data wrapper. The whole bootstrap is the
# gallery-baked chinit binary.
#
# templatefile() escaping: a literal dollar-dollar-brace below renders
# as a literal bash brace; the bare braces are the module's
# interpolations.
set +e

CHINIT_SPEC_KEY="${chinit_spec_key}"
CHINIT_SPEC_PATH="/var/lib/clickhouse-bootstrap/chinit.yaml"
BOOT_UNIT=/etc/systemd/system/chinit-boot.service
MANAGED_IDENTITY_CLIENT_ID="${managed_identity_client_id}"

# chinit's Azure provider env contract: written INTO the systemd unit
# below (a systemd service does NOT inherit the wrapper shell's
# environment — the pins must ride the unit's own Environment lines).

${wrapper_extra}

# The golden-image contract: chinit is baked and checksum-gated at image
# build time. A missing binary is an image failure, never something to
# work around at boot.
if [ ! -x /usr/local/bin/chinit ]; then
    echo "ERROR: chinit not found in the gallery image - wrong or broken image" >&2
    exit 1
fi


# Seed the role's spec from the scripts container (chinit's own bundle
# fetch re-validates it). BOUNDED RETRY: a fresh boot can race an
# in-flight apply that just replaced the bundle blobs; a single-shot
# fetch that 404'd would silently leave the node specless. az login
# can also need a beat for the identity endpoint on a fresh VM.
mkdir -p "$(dirname "$CHINIT_SPEC_PATH")"
for i in $(seq 1 30); do
    [ -n "$MANAGED_IDENTITY_CLIENT_ID" ] && az login --identity --client-id "$MANAGED_IDENTITY_CLIENT_ID" --allow-no-subscriptions >/dev/null 2>&1
    az storage blob download \
        --account-name "${storage_account}" \
        --container-name "${container_name}" \
        --name "$CHINIT_SPEC_KEY" \
        --file "$CHINIT_SPEC_PATH" \
        --auth-mode login \
        --only-show-errors 2>/dev/null && [ -s "$CHINIT_SPEC_PATH" ] && break
    echo "spec fetch attempt $i failed (bundle replaced mid-boot / identity warmup?); retrying in 10s" >&2
    sleep 10
done
if [ ! -s "$CHINIT_SPEC_PATH" ]; then
    echo "ERROR: role spec $CHINIT_SPEC_KEY missing from the scripts container" >&2
    exit 1
fi

# Run the boot under systemd, not a bare exec: a Critical step can
# legitimately fail while the cloud is still settling (Key Vault
# replication lag, Table eventual consistency). A restart-on-failure
# unit keeps converging by itself — chinit is idempotent, every step
# probes real state, so a re-run resumes instead of redoing work.
case "${vm_name_mode}" in
    keeper) CHINIT_VM_NAME="$(hostname)" ;;
    *)       CHINIT_VM_NAME="${vm_name}" ;;
esac

cat > "$BOOT_UNIT" <<UNIT
[Unit]
Description=chinit node boot (the whole bootstrap; idempotent)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
Environment=CHINIT_AZURE_SUBSCRIPTION_ID=${subscription_id}
Environment=CHINIT_AZURE_RESOURCE_GROUP=${resource_group_name}
Environment=CHINIT_AZURE_STORAGE_ACCOUNT=${storage_account}
Environment=CHINIT_AZURE_VAULT_NAME=${key_vault_name}
Environment=CHINIT_AZURE_VM_NAME=$CHINIT_VM_NAME
ExecStart=/usr/local/bin/chinit boot -spec $CHINIT_SPEC_PATH -cloud azure
Restart=on-failure
RestartSec=15

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl start --no-block chinit-boot.service

echo "chinit boot launched (systemd: chinit-boot.service; journalctl -u chinit-boot)"
