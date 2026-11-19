# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Writers AMI-roll convergence: when the writers launch-template gains a
# new version (AMI roll, user-data change), point the ASG at it and start
# a rolling instance refresh. Run from the terraform_data local-exec
# provisioner in main.tf. A literal dollar-brace must be written doubled
# (dollar-dollar-brace) — the render-smoke escaping rules apply.

set -euo pipefail
aws autoscaling update-auto-scaling-group \
  --auto-scaling-group-name "${asg_name}" \
  --launch-template "LaunchTemplateId=${lt_id},Version='${lt_version}'" \
  --region "${aws_region}"
REFRESH_CONFIG='${refresh_config}'
aws autoscaling start-instance-refresh \
  --auto-scaling-group-name "${asg_name}" \
  --region "${aws_region}" \
  --strategy Rolling \
  --desired-configuration "$REFRESH_CONFIG" \
  --preferences '{"InstanceWarmup":${warmup_seconds},"MinHealthyPercentage":${min_healthy_percentage},"AutoRollback":true}'
