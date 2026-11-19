# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Example — a minimal chinit cluster on AWS.
#
# Copy this directory as your root, set the two required variables, and:
#   tofu init && tofu apply
#
# Prereq: a golden AMI built from images/aws (packer build …); pass its id
# as golden_ami_id, or leave null in us-west-2 to auto-resolve the public
# stackgenhq/chinit golden AMI.

provider "aws" {
  region = "us-west-2"
}

# The dns_tls module can issue Let's Encrypt certs via DNS-01. Configure
# the ACME provider once at the ROOT (the provider schema demands
# server_url at config time even when the ACME path is unused).
provider "acme" {
  server_url = "https://acme-staging-v02.api.letsencrypt.org/directory"
}

module "clickhouse" {
  source = "../.."

  cluster_name  = "demo"
  region        = "us-west-2"
  golden_ami_id = null # auto-resolve the public golden AMI (us-west-2)

  # Fleet shape (defaults shown; every value has a secure default)
  writer_max     = 2
  writer_desired = 2
  reader_max     = 6
  reader_desired = 2
  reader_spot    = 0.5 # half the reader slots above the base on Spot
  keeper_count   = 3
  enable_backup  = true

  # Client ingress: none until you name CIDRs (never an open rule)
  client_cidrs = ["10.0.0.0/16"]

  alert_emails = ["ops@example.com"]

  tags = {
    project = "demo"
  }
}
