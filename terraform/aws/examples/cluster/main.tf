# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Example — a minimal chinit cluster on AWS
#
# Prereq: a golden AMI built from images/aws (packer build …); pass its
# id as golden_ami_id (or -var / a tfvars file).

provider "aws" {
  region = "us-west-2"
}

module "clickhouse" {
  source = "../.."

  cluster_name  = "demo"
  region        = "us-west-2"
  golden_ami_id = "ami-0123456789abcdef0" # from images/aws packer build

  # Identity ceilings (ASG maxima — never live counts)
  writer_max     = 2
  writer_desired = 2
  reader_max     = 6
  reader_desired = 2
  keeper_count   = 3
  enable_backup  = true

  # Network: a fresh VPC in 10.42.0.0/24 (set create_network = false and
  # pass vpc_id/subnet_id to plug into an existing one)
  create_network = true
  subnet_cidr    = "10.42.0.0/24"
  client_cidrs   = ["10.42.0.0/24"] # your clients' CIDRs

  dns_zone_name = "clickhouse.internal"
}

output "writer_0" {
  value = "writer-0.${module.clickhouse.dns_zone_name}"
}

output "admin_password_secret" {
  value = module.clickhouse.secrets.admin_password # fetch with aws secretsmanager
}
