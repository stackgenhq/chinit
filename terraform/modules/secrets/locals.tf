# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — Secrets Module
# ============================================================================
# All derived values for this module live here, in ONE locals block (open
# source Terraform convention): resources below reference local.* only.
# ============================================================================

locals {
  # ------------------------------------------------------------------------
  # Rotation locals
  # ------------------------------------------------------------------------
  # Why: `rotation` is an object of optional fields; normalize it once so the
  #      rest of the module reads concrete booleans/numbers. coalesce() also
  #      covers a caller explicitly passing { enabled = null }.
  # Without it: Every rotation resource would repeat
  #             coalesce(var.rotation.enabled, true) inline, inviting drift
  #             between the rotation resources, the Lambda count and outputs.
  rotation_enabled = coalesce(var.rotation.enabled, true)
  rotation_days    = coalesce(var.rotation.rotation_days, 30)

  # ------------------------------------------------------------------------
  # Secret value resolution
  # ------------------------------------------------------------------------
  # Why: Each password is either user-provided or generated. Resolving the
  #      final value in a local keeps the three aws_secretsmanager_secret_
  #      version blocks identical and makes the "generated when null"
  #      contract explicit in exactly one place per user.
  # Without it: Each secret_version resource would inline a conditional
  #             against its random_password result, tripling drift risk when
  #             a fourth ClickHouse user is added.
  default_password_value = var.credentials_config.default_password != null ? var.credentials_config.default_password : random_password.default_password.result
  admin_password_value   = var.credentials_config.admin_password != null ? var.credentials_config.admin_password : random_password.admin_password.result
  writer_password_value  = var.credentials_config.writer_password != null ? var.credentials_config.writer_password : random_password.writer_password.result

  # ------------------------------------------------------------------------
  # Secret names
  # ------------------------------------------------------------------------
  # Why: Fixed cluster_name-prefixed names so cloud-init scripts, the SSM
  #      re-render command and operator CLI calls can reference a secret
  #      without a terraform output round-trip. The "-<user>-password"
  #      suffix is what the rotation Lambda parses to map secret -> user.
  # Without it: Every consumer (cloud-init, rotation Lambda, runbooks) would
  #             need injected names, and ad-hoc debugging would require a
  #             list-secrets lookup first.
  default_secret_name = "${var.cluster_name}-default-password"
  admin_secret_name   = "${var.cluster_name}-admin-password"
  writer_secret_name  = "${var.cluster_name}-writer-password"

  # Why: One map keyed by user feeds the secret policies, the Lambda invoke
  #      permissions and the rotation schedules — three resources that must
  #      stay in lockstep across the same three secrets.
  # Without it: Three separate for_each expressions could drift (e.g. a
  #             policy attached to a secret that never gets a rotation rule).
  password_secret_arns = {
    default = aws_secretsmanager_secret.default_password.arn
    admin   = aws_secretsmanager_secret.admin_password.arn
    writer  = aws_secretsmanager_secret.writer_password.arn
  }

  # ------------------------------------------------------------------------
  # SSM fleet targeting
  # ------------------------------------------------------------------------
  # Why: The rotation Lambda targets instances by tag, not by id. Tags are
  #      the only stable identity across ASG scale-out and Spot replacement.
  #      The compute module stamps NodeType on every node (writer/reader/
  #      keeper/backup); only writer/reader/backup run clickhouse-server
  #      (keepers run clickhouse-keeper and hold no users.d), so the target
  #      values exclude keepers — otherwise every rotation would fail the
  #      command on keeper nodes because systemctl restart clickhouse-server
  #      errors there. The value is a comma-joined string passed via a
  #      Lambda env var and split in Python (boto3 needs a real list; it
  #      does NOT split commas the way the AWS CLI does).
  #      Cross-cluster safety does NOT depend on this tag: a foreign
  #      cluster's instance role is not in allowed_role_arns, so its
  #      get-secret-value call fails (secret policy deny) and the script
  #      aborts on set -e BEFORE rendering anything.
  # Without it: SendCommand would need instance ids, which change on every
  #             scale event and are unknown at module scope — or it would
  #             hit keeper nodes and error, wedging rotation.
  ssm_target_tag_key   = "NodeType"
  ssm_target_tag_value = "writer,reader,backup"

  # ------------------------------------------------------------------------
  # Lambda naming (shared by the function, its role, and its log group)
  # ------------------------------------------------------------------------
  # Why: A single local for the Lambda name keeps the CloudWatch log-group ARN
  #      in the role policy pointing at the exact group the function writes
  #      to, and keeps /aws/lambda/<name> consistent if the name ever changes.
  # Without it: Renaming the function would silently orphan the log group ARN
  #             in the IAM policy and logs would land in a group the policy
  #             does not allow.
  rotation_lambda_name = "${var.cluster_name}-password-rotation"

  # ------------------------------------------------------------------------
  # Rotation Lambda source (python3.12)
  # ------------------------------------------------------------------------
  # Why: The Lambda source lives in this local (heredoc) and is zipped by the
  #      archive provider, so the module stays self-contained: code changes
  #      are visible in git diffs of main.tf and change source_code_hash,
  #      which triggers a Lambda code update. AWS only ships predefined
  #      rotation Lambdas for RDS, so a custom Lambda is mandatory for
  #      ClickHouse passwords.
  # Without it: aws_secretsmanager_secret_rotation would have no
  #             rotation_lambda_arn to attach to, and rotation would remain
  #             a manual runbook.
  # NOTE: HCL escaping — "$" is literal in heredocs except the "${" sequence,
  #      which must be written "$${" to reach the shell verbatim.
  rotation_lambda_source = <<-EOT
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
  EOT

  # ------------------------------------------------------------------------
  # Manual re-sync script (fallback when the rotation Lambda fails)
  # ------------------------------------------------------------------------
  # Why: If the Lambda or its SSM step fails mid-rotation, the fleet can end
  #      up rendered with a password that no longer matches AWSCURRENT. This
  #      self-contained snippet pulls ALL THREE current secrets, re-renders
  #      every users.d password file and restarts the server — a single
  #      copy-pasteable recovery path, exported as the exact AWS CLI command
  #      (see outputs.tf ssm_manual_resync_command) and duplicated in README.
  #      Rendered files use the zz- prefix so they override cloud-init's
  #      z_users.xml in ClickHouse's users.d merge order; keepers are not
  #      targeted (they run clickhouse-keeper, not clickhouse-server).
  # Without it: Recovery would require SSH (disabled on this cluster in
  #             favor of SSM Session Manager) and hand-edited XML.
  manual_resync_script = <<-EOT
    set -euo pipefail
    mkdir -p /etc/clickhouse-server/users.d
    for u in default admin writer; do
      PW=$(aws secretsmanager get-secret-value --secret-id "${var.cluster_name}-$u-password" --region "${data.aws_region.current.region}" --query SecretString --output text)
      HASH=$(printf %s "$PW" | sha256sum)
      HASH=$${HASH%% *}
      printf "<clickhouse><users><%s><password_sha256_hex>%s</password_sha256_hex></%s></users></clickhouse>\\n" "$u" "$HASH" "$u" > /etc/clickhouse-server/users.d/zz-$u-password.xml
      chown clickhouse:clickhouse /etc/clickhouse-server/users.d/zz-$u-password.xml
    done
    systemctl restart clickhouse-server
  EOT
}
