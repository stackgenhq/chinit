# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ClickHouse password rotation Lambda (python3.12). Implements the
# Secrets Manager 4-step protocol (createSecret/setSecret/testSecret/
# finishSecret); the fleet re-renders users.d via SSM so the password
# never transits the Lambda or its logs. Zipped by the archive provider
# into handler.py. templatefile escaping: a literal dollar is safe; a
# literal dollar-brace must be written doubled (dollar-dollar-brace).

"""ClickHouse password rotation - implements the Secrets Manager 4-step protocol."""
import logging, os, secrets, string

import boto3

log = logging.getLogger()
log.setLevel(logging.INFO)
SM, SSM = boto3.client("secretsmanager"), boto3.client("ssm")
# Alphanumeric only: ClickHouse users.xml breaks on quotes/ampersands.
ALPHABET = string.ascii_letters + string.digits
LENGTH = 16
# Secret-name suffix -> ClickHouse user (rendered as users.d/<user>-password.xml)
USERS = {"default-password": "default", "admin-password": "admin", "writer-password": "writer"}

def render_command(arn, token, user, region):
    """SSM payload: instances pull the pending password and re-render users.d."""
    # File name sorts AFTER cloud-init's z_users.xml so the rotated hash
    # wins ClickHouse's users.d merge order (z_* then zz_*).
    xml = f"/etc/clickhouse-server/users.d/zz-{user}-password.xml"
    return f"""set -euo pipefail
mkdir -p /etc/clickhouse-server/users.d
PW=$(aws secretsmanager get-secret-value --secret-id '{arn}' --version-id '{token}' --region '{region}' --query SecretString --output text)
HASH=$(printf %s "$PW" | sha256sum | cut -d' ' -f1)
printf '<clickhouse><users><%s><password_sha256_hex>%s</password_sha256_hex></%s></users></clickhouse>\\n' '{user}' "$HASH" '{user}' > {xml}
chown clickhouse:clickhouse {xml}
systemctl restart clickhouse-server"""

def handler(event, context):
    """Rotation entrypoint; Secrets Manager invokes once per Step."""
    arn, token, step = event["SecretId"], event["ClientRequestToken"], event["Step"]
    suffix = arn.split(":")[-1]
    user = next((u for s, u in USERS.items() if suffix.endswith(s)), None)
    if not user:
        raise ValueError("not a ClickHouse password secret: " + suffix)
    log.info("rotating ClickHouse user %s (step %s)", user, step)
    if step == "createSecret":
        # Store the AWSPENDING version; AWSCURRENT still serves the old password.
        pw = "".join(secrets.choice(ALPHABET) for _ in range(LENGTH))
        SM.put_secret_value(SecretId=arn, ClientRequestToken=token, SecretString=pw)
    elif step == "setSecret":
        # Fleet re-render via SSM: instances pull the secret themselves, so
        # the password never transits the Lambda or its logs. Values must
        # be a real list — boto3 does NOT split commas (unlike the CLI).
        cmd = SSM.send_command(
            DocumentName="AWS-RunShellScript",
            Targets=[{"Key": "tag:" + os.environ["SSM_TAG_KEY"], "Values": os.environ["SSM_TAG_VALUE"].split(",")}],
            Parameters={"commands": [render_command(arn, token, user, os.environ["AWS_REGION"])]},
        )
        log.info("SSM command %s dispatched to fleet", cmd["Command"]["CommandId"])
    elif step == "testSecret":
        # No network path from Lambda to ClickHouse; the SSM invocation
        # status from setSecret is the real signal. Deliberate no-op.
        log.info("connection test skipped; SSM command status is the signal")
    elif step == "finishSecret":
        # Promote the pending version to AWSCURRENT; old one -> AWSPREVIOUS.
        stages = SM.describe_secret(SecretId=arn)["VersionIdsToStages"]
        current = next(v for v, s in stages.items() if "AWSCURRENT" in s)
        if current != token:
            SM.update_secret_version_stage(
                SecretId=arn, VersionStage="AWSCURRENT",
                MoveToVersionId=token, RemoveFromVersionId=current,
            )
    return {"Step": step, "User": user}
