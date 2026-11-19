# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Manual fleet re-sync (fallback when the rotation Lambda fails): pulls
# ALL THREE current secrets, re-renders every users.d password file and
# restarts the server — a single copy-pasteable recovery path, exported
# as the exact AWS CLI command (outputs.tf ssm_manual_resync_command).
# Rendered files use the zz- prefix so they override cloud-init's
# z_users.xml in ClickHouse's users.d merge order; keepers are not
# targeted (they run clickhouse-keeper, not clickhouse-server).
# templatefile escaping: dollar is literal; dollar-brace interpolation
# is used for cluster_name/region below, and a literal dollar-brace in
# shell must be written doubled.

set -euo pipefail
mkdir -p /etc/clickhouse-server/users.d
for u in default admin writer; do
  PW=$(aws secretsmanager get-secret-value --secret-id "${cluster_name}-$u-password" --region "${aws_region}" --query SecretString --output text)
  HASH=$(printf %s "$PW" | sha256sum)
  HASH=$${HASH%% *}
  printf "<clickhouse><users><%s><password_sha256_hex>%s</password_sha256_hex></%s></users></clickhouse>\\n" "$u" "$HASH" "$u" > /etc/clickhouse-server/users.d/zz-$u-password.xml
  chown clickhouse:clickhouse /etc/clickhouse-server/users.d/zz-$u-password.xml
done
systemctl restart clickhouse-server
