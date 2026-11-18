# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Example prod override for the ClickHouse golden AMI build.
# Usage: packer build -var-file=defaults.pkrvars.hcl -var-file=prod.pkrvars.hcl clickhouse.pkr.hcl
clickhouse_version = "26.8.36.3" # LTS; e.g. "26.10.3.7" for newest stable
region             = "us-east-1"
instance_type      = "c8g.large"
