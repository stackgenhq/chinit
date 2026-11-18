#!/bin/bash
# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# chinit stage-0: user-data wrapper (kept under the 16 KiB EC2 cap).
# The whole bootstrap is the baked chinit binary — the image IS the
# version.
#
# templatefile() escaping: a literal dollar-dollar-brace below renders
# as a literal bash brace; the bare braces are the module's
# interpolations.
set +e

CHINIT_SPEC_KEY="${chinit_spec_key}"
CHINIT_SPEC_PATH="/var/lib/clickhouse-bootstrap/chinit.yaml"
BOOT_UNIT=/etc/systemd/system/chinit-boot.service
export AWS_REGION="${region}"
export AWS_DEFAULT_REGION="${region}"

${wrapper_extra}

# The golden-image contract: chinit is baked and checksum-gated at image
# build time. A missing binary is an image failure, never something to
# work around at boot (check golden_ami_id and the packer build).
if [ ! -x /usr/local/bin/chinit ]; then
    echo "ERROR: chinit not found in the golden AMI - wrong or broken image" >&2
    exit 1
fi

# Seed the role's spec (chinit's own bundle fetch re-validates it).
# BOUNDED RETRY: a fresh boot can race an in-flight apply that just
# replaced the bundle objects (the writer-0 finding: the fetch 404'd
# and a single-shot user-data silently "succeeded" with no spec — the
# ASG kept a node that never bootstrapped). Retry until the object
# exists, then let chinit's own SHA gate handle the content.
# Always pass --region so aws s3 cp targets s3.<region>.amazonaws.com
# and rides the VPC's free S3 Gateway Endpoint instead of defaulting to
# s3.amazonaws.com (us-east-1) via the NAT Gateway.
mkdir -p "$(dirname "$CHINIT_SPEC_PATH")"
for i in $(seq 1 30); do
    aws s3 cp --region "${region}" "s3://${bundle_bucket}/${bundle_prefix}/$CHINIT_SPEC_KEY" "$CHINIT_SPEC_PATH" && break
    echo "spec fetch attempt $i failed (bundle replaced mid-boot?); retrying in 10s" >&2
    sleep 10
done
if [ ! -s "$CHINIT_SPEC_PATH" ]; then
    echo "ERROR: role spec $CHINIT_SPEC_KEY missing from the bundle prefix" >&2
    exit 1
fi

# Run the boot under systemd, not a bare exec: a Critical step can
# legitimately fail while the cloud is still settling (the backup-0
# finding: retrieve_passwords exhausted its 5 attempts against the
# mid-roll secrets and the node sat unconfigured until a human re-ran
# the boot). A restart-on-failure unit keeps converging by itself.
cat > "$BOOT_UNIT" <<UNIT
[Unit]
Description=chinit node boot (the whole bootstrap; idempotent)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/chinit boot -spec $CHINIT_SPEC_PATH -cloud aws
# Convergence, not alarm spam: retry with a capped backoff. chinit is
# idempotent — every step probes real state, so a re-run after a
# transient failure (secrets propagation, S3 eventual consistency,
# keeper mid-roll) resumes instead of redoing work.
Restart=on-failure
RestartSec=15

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl start --no-block chinit-boot.service

# Success here = the boot is LAUNCHED and self-healing; progress lands
# in state.json and journald. Do not block user-data on it: the ASG's
# health comes from the ClickHouse health-gate units chinit installs.
echo "chinit boot launched (systemd: chinit-boot.service; journalctl -u chinit-boot)"
