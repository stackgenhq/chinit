# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Stage-0 node tuning inserted into each role's user-data wrapper before
# chinit boot. Escaping notes: a literal dollar needs no escaping here
# (templatefile only reacts to the dollar-brace sequence); a literal
# dollar-brace must be written doubled, like in the chinit cloud-init
# templates (see scripts/render-smoke.sh escaping rules).
# The # comment lines at the top of this file are stripped at render time
# so the embedded user-data stays minimal.

IMDS_TOK=$(curl -sS -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60" || true)
EC2_IID=$(curl -sS -H "X-aws-ec2-metadata-token: $IMDS_TOK" "http://169.254.169.254/latest/meta-data/instance-id" || hostname)
if [ "$CHINIT_SPEC_KEY" = "chinit-backup.yaml" ]; then
  printf 'CH_CAS_SERVER_ROOT_ID="backup-0"\n' > /etc/default/clickhouse
else
  printf 'CH_CAS_SERVER_ROOT_ID="%s"\n' "$EC2_IID" > /etc/default/clickhouse
fi
chmod 0644 /etc/default/clickhouse
mkdir -p /etc/systemd/system/clickhouse-server.service.d
printf '[Service]\nLimitNOFILE=262144\nLimitNPROC=65536\nTasksMax=infinity\nExecStartPre=-/bin/sh -c "mkdir -p /var/lib/clickhouse/disks/hot; if [ -f /var/lib/clickhouse/uuid ]; then tr -d '\''\\r\\n'\'' < /var/lib/clickhouse/uuid > /var/lib/clickhouse/uuid.tmp && mv /var/lib/clickhouse/uuid.tmp /var/lib/clickhouse/uuid; fi"\n' > /etc/systemd/system/clickhouse-server.service.d/limits.conf
