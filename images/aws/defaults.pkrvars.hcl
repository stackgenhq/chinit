# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Packer variable defaults for the ClickHouse golden AMI.
# Override per environment with -var-file=<env>.pkrvars.hcl
clickhouse_version        = "26.8.36.3"
clickhouse_backup_version = "2.5.0"
region                    = "us-west-2"
instance_type             = "c8g.large"
