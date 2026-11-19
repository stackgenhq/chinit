# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Secrets Module — AWS ClickHouse Cluster
# ============================================================================
# Why: Stores the three ClickHouse user passwords (default/admin/writer) in
#      AWS Secrets Manager with scheduled rotation
#      (Key Vault-style secrets + random passwords + automated re-render
#      on rotation, rebuilt on AWS-native primitives).
# Without it: Passwords would live in tfvars/state in plaintext, rotation
#             would be a manual SSH runbook, and instance profiles would have
#             no broker to fetch credentials from at boot.
#
# Rotation design (deliberately NOT the RDS-style native rotation):
# AWS only ships PREDEFINED rotation Lambdas for RDS. For custom passwords
# like ClickHouse's, a custom Lambda is REQUIRED — there is no
# aws_secretsmanager_secret_rotation without a rotation_lambda_arn for
# non-RDS secrets. This module therefore deploys one small inline-code Lambda
# that implements the four-step Secrets Manager rotation protocol:
#   createSecret -> put_secret_value (new AWSPENDING password version)
#   setSecret    -> SSM SendCommand (AWS-RunShellScript) to the instance
#                   fleet: instances pull the pending secret, re-render
#                   /etc/clickhouse-server/users.d/<user>-password.xml and
#                   restart clickhouse-server
#   testSecret   -> deliberate no-op (the SSM invocation status is the real
#                   health signal; the Lambda has no route to the cluster)
#   finishSecret -> move AWSCURRENT to the new version
# ============================================================================

# ============================================================================
# Account context data sources
# The secret policy fallback (account root principal) and the Lambda IAM policy ARNs need the account id, region and partition.
# ============================================================================
data "aws_caller_identity" "current" {}

data "aws_region" "current" {}

data "aws_partition" "current" {}

# ============================================================================
# Random Password: Default User
# Generates a secure random password for the default (read-only) ClickHouse user when credentials_config.
# ============================================================================
resource "random_password" "default_password" {
  length  = 16
  special = false
  upper   = true
  lower   = true
  numeric = true
}

# ============================================================================
# Random Password: Admin User
# Same rationale as the default user, kept as a separate resource (not count/for_each) so each password has an independent lifecycle in state —…
# ============================================================================
resource "random_password" "admin_password" {
  length  = 16
  special = false
  upper   = true
  lower   = true
  numeric = true
}

# ============================================================================
# Random Password: Writer User
# Same rationale as the default user for the writer (insert/DDL) user.
# ============================================================================
resource "random_password" "writer_password" {
  length  = 16
  special = false
  upper   = true
  lower   = true
  numeric = true
}

# ============================================================================
# Secrets Manager Secret: Default User Password
# Secrets Manager (over SSM Parameter Store SecureString) provides versioned values, native rotation scheduling, CloudTrail-audited access and…
# ============================================================================
resource "aws_secretsmanager_secret" "default_password" {
  name        = local.default_secret_name
  description = "ClickHouse default (read-only) user password - cluster ${var.cluster_name}"
  tags        = var.tags

  recovery_window_in_days = 7
}

# ============================================================================
# Secrets Manager Secret: Admin User Password
# The admin user is the highest-privilege credential in the cluster, so it must live in the same auditable, IAM-gated, rotatable store as the other…
# ============================================================================
resource "aws_secretsmanager_secret" "admin_password" {
  name        = local.admin_secret_name
  description = "ClickHouse admin user password - cluster ${var.cluster_name}"
  tags        = var.tags

  recovery_window_in_days = 7
}

# ============================================================================
# Secrets Manager Secret: Writer User Password
# The writer user's insert/DDL password is the credential handed to ingest pipelines;
# ============================================================================
resource "aws_secretsmanager_secret" "writer_password" {
  name        = local.writer_secret_name
  description = "ClickHouse writer user password - cluster ${var.cluster_name}"
  tags        = var.tags

  recovery_window_in_days = 7
}

# ============================================================================
# Secret Versions
# Secrets Manager separates the secret container from its versioned values;
# ============================================================================
resource "aws_secretsmanager_secret_version" "default_password" {
  secret_id     = aws_secretsmanager_secret.default_password.id
  secret_string = local.default_password_value
}

resource "aws_secretsmanager_secret_version" "admin_password" {
  secret_id     = aws_secretsmanager_secret.admin_password.id
  secret_string = local.admin_password_value
}

resource "aws_secretsmanager_secret_version" "writer_password" {
  secret_id     = aws_secretsmanager_secret.writer_password.id
  secret_string = local.writer_password_value
}

# ============================================================================
# Secret Policies — grant GetSecretValue to the instance roles
# A resource-based policy on each secret is how the writer/reader/ keeper/backup instance-profile roles (whose ARNs arrive from the storage module…
# ============================================================================
data "aws_iam_policy_document" "secret_access" {
  for_each = local.password_secret_arns

  statement {
    sid       = "AllowClickHouseInstanceRolesGetSecretValue"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [each.value]

    principals {
      type = "AWS"
      identifiers = length(var.allowed_role_arns) > 0 ? var.allowed_role_arns : [
        "arn:${data.aws_partition.current.partition}:iam::${data.aws_caller_identity.current.account_id}:root",
      ]
    }
  }
}

resource "aws_secretsmanager_secret_policy" "default_password" {
  secret_arn = aws_secretsmanager_secret.default_password.arn
  policy     = data.aws_iam_policy_document.secret_access["default"].json
}

resource "aws_secretsmanager_secret_policy" "admin_password" {
  secret_arn = aws_secretsmanager_secret.admin_password.arn
  policy     = data.aws_iam_policy_document.secret_access["admin"].json
}

resource "aws_secretsmanager_secret_policy" "writer_password" {
  secret_arn = aws_secretsmanager_secret.writer_password.arn
  policy     = data.aws_iam_policy_document.secret_access["writer"].json
}

# ============================================================================
# Rotation Lambda: CloudWatch Log Group
# An explicit log group pins a retention period.
# ============================================================================
resource "aws_cloudwatch_log_group" "rotation_lambda" {
  count = local.rotation_enabled ? 1 : 0

  name              = "/aws/lambda/${local.rotation_lambda_name}"
  retention_in_days = 30
  tags              = var.tags
}

# ============================================================================
# Rotation Lambda: IAM Role
# The rotation Lambda needs its own execution role: Lambda's service principal must be allowed to assume it before the function can run.
# ============================================================================
resource "aws_iam_role" "rotation_lambda" {
  count = local.rotation_enabled ? 1 : 0

  name = local.rotation_lambda_name
  tags = var.tags

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "lambda.amazonaws.com" }
    }]
  })
}

# ============================================================================
# Rotation Lambda: IAM Policy (secretsmanager + ssm + logs)
# The Lambda implements the rotation protocol, so it must write new secret versions (PutSecretValue), read version metadata (DescribeSecret),…
# ============================================================================
resource "aws_iam_role_policy" "rotation_lambda" {
  count = local.rotation_enabled ? 1 : 0

  name = local.rotation_lambda_name
  role = aws_iam_role.rotation_lambda[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "RotateClickHousePasswordSecrets"
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue", "secretsmanager:DescribeSecret", "secretsmanager:UpdateSecretVersionStage"]
        Resource = values(local.password_secret_arns)
      },
      {
        Sid    = "SendFleetRenderCommand"
        Effect = "Allow"
        Action = ["ssm:SendCommand"]
        Resource = [
          # AWS-owned document ARN carries no account id.
          "arn:${data.aws_partition.current.partition}:ssm:${data.aws_region.current.region}::document/AWS-RunShellScript",
          "arn:${data.aws_partition.current.partition}:ec2:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:instance/*",
        ]
      },
      {
        Sid      = "WriteRotationLogs"
        Effect   = "Allow"
        Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
        Resource = ["${aws_cloudwatch_log_group.rotation_lambda[0].arn}:*"]
      },
    ]
  })
}

# ============================================================================
# Rotation Lambda: Package (archive provider)
# aws_lambda_function has no inline-code option for real functions — it needs a zip artifact.
# ============================================================================
data "archive_file" "rotation_lambda" {
  count = local.rotation_enabled ? 1 : 0

  type        = "zip"
  output_path = "${path.module}/rotation_lambda.zip"

  source {
    content  = local.rotation_lambda_source
    filename = "handler.py"
  }
}

# ============================================================================
# Rotation Lambda: Function
# A single Lambda serves all three secrets (it maps secret name -> ClickHouse user), keeping one role, one policy and one log group instead of three…
# ============================================================================
resource "aws_lambda_function" "rotation" {
  count = local.rotation_enabled ? 1 : 0

  function_name = local.rotation_lambda_name
  role          = aws_iam_role.rotation_lambda[0].arn
  runtime       = "python3.12"
  handler       = "handler.handler"
  architectures = ["arm64"]
  timeout       = 60
  memory_size   = 128
  tags          = var.tags

  filename         = data.archive_file.rotation_lambda[0].output_path
  source_code_hash = data.archive_file.rotation_lambda[0].output_base64sha256

  environment {
    variables = {
      SSM_TAG_KEY   = local.ssm_target_tag_key
      SSM_TAG_VALUE = local.ssm_target_tag_value
    }
  }

  depends_on = [aws_cloudwatch_log_group.rotation_lambda]
}

# ============================================================================
# Rotation Lambda: Secrets Manager invoke permission
# Secrets Manager invokes the rotation Lambda as a service principal, which Lambda's resource policy must explicitly allow — scoped with source_arn…
# ============================================================================
resource "aws_lambda_permission" "rotation" {
  for_each = local.rotation_enabled ? local.password_secret_arns : {}

  statement_id  = "AllowSecretsManagerInvoke${title(each.key)}"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.rotation[0].function_name
  principal     = "secretsmanager.amazonaws.com"
  source_arn    = each.value
}

# ============================================================================
# Secrets Manager Rotation Schedules
# Attaches the Lambda to each secret on a rate(<n> days) schedule.
# ============================================================================
resource "aws_secretsmanager_secret_rotation" "rotation" {
  for_each = local.rotation_enabled ? local.password_secret_arns : {}

  secret_id           = each.value
  rotation_lambda_arn = aws_lambda_function.rotation[0].arn
  rotate_immediately  = false

  rotation_rules {
    schedule_expression = "rate(${local.rotation_days} days)"
  }

  depends_on = [
    aws_secretsmanager_secret_version.default_password,
    aws_secretsmanager_secret_version.admin_password,
    aws_secretsmanager_secret_version.writer_password,
    aws_lambda_function.rotation,
    aws_lambda_permission.rotation,
  ]
}
