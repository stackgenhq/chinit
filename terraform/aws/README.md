<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# `chinit` Cluster Module — AWS

Deploy a secure, production-shaped ClickHouse fleet on AWS with one module
call. The module composes the hardened ecosystem modules
([network](../modules/network), [storage](../modules/storage),
[secrets](../modules/secrets), [clickhouse](../modules/clickhouse),
[compute](../modules/compute), [dns_tls](../modules/dns_tls),
[monitoring](../modules/monitoring), [security](../modules/security))
around the [chinit](https://github.com/stackgenhq/chinit) contract — the
same modules a large-scale fleet runs on.

```hcl
module "clickhouse" {
  source = "github.com/stackgenhq/chinit//terraform/aws?ref=<tag>"
  # local checkout: source = "../terraform/aws"

  cluster_name  = "demo"
  region        = "us-west-2"
  golden_ami_id = null # auto-resolve the public golden AMI in us-west-2
  alert_emails  = ["ops@example.com"]
}
```

Four commands from zero to a running cluster:

```bash
tofu init
tofu apply -var 'cluster_name=demo' -var 'region=us-west-2' -var 'alert_emails=["ops@example.com"]'
tofu output node_endpoints     # who to connect to
tofu output connect_writer     # ready-to-run client command
```

## The security posture you get by default

| Control | Default |
|---|---|
| Node access | SSM Session Manager only — no SSH keys, no bastion |
| Metadata service | IMDSv2 required, hop limit 1 |
| Credentials | No static keys anywhere: S3 via instance role, secrets by name |
| Secrets | Secrets Manager with a rotation Lambda (30d) that re-renders the fleet via SSM |
| Data at rest | EBS encrypted everywhere; SSE-S3 buckets, public access blocked, TLS-only transport enforced by bucket policy |
| Network | Private subnet, NAT-less (S3/DynamoDB ride free gateway endpoints), per-role security groups, client ingress only from `client_cidrs` |
| Ingress | None until you pass `client_cidrs` — never an open rule |
| Threat detection | GuardDuty (S3 data events on) + Security Hub (AWS FSBP + CIS), HIGH/CRITICAL findings routed to your alerts topic |
| Fleet identity | DynamoDB ordinal leases (TTL) — Spot churn and ASG replacement re-claim identity safely |
| Config integrity | SHA-256-gated config bundle; a mismatch aborts boot |

## Fleet shape (all defaults are small-and-cheap but HA-safe)

- **Writers**: on-demand ASG (`writer_max` ceiling, lease identity, graceful
  drain, AMI-roll convergence via instance refresh)
- **Readers**: Spot-diversified ASG (`reader_spot` fraction, on-demand base
  floor, spot-watch drains on the EC2 interruption notice)
- **Keepers**: 3 fixed EC2 with static IPs (+20..29 offsets) and persistent
  Raft EBS
- **Backup replica**: fixed EC2 (+30) running `clickhouse-backup` nightly
- **Storage**: S3-backed data bucket — a tenant is a prefix + policy
  (`tenants` variable), never a disk-resizing project. Merged parts TTL-move
  to S3 after `cold_move_ttl_days`; lifecycle transitions to Standard-IA.
- **Client entry**: private DNS only by default (`writer-0.<zone>` inside the
  VPC). The ecosystem [compute](../modules/compute) module can add NLBs
  later; compose it directly when you need them.

## Opt-in extras (one variable each)

| Variable | What it unlocks |
|---|---|
| `cas_enabled = true` | Shared content-addressed S3 cold tier (Altinity Antalya AMI): cold parts stored once, not per replica |
| `budget_monthly_amount` | Budget alerts on the cluster's cost tags |
| `log_shipping` | Remote log/metric/trace shipping (OTLP/Fluent Bit) — local-only by default |
| `tenants` | Additional isolated S3 prefixes + storage policies |
| `cache_on_nvme_instance_store` | Free NVMe instance-store cache instead of gp3 (NVMe-capable families) |
| `client_cidrs` | Client ingress to 8123/8443/9000/9440 on writers + readers |

## Prerequisites

1. **A golden AMI** built from [`images/aws`](../../images/aws), or pass
   nothing in `us-west-2` and the module auto-resolves the latest public
   stackgenhq/chinit golden AMI (`golden_ami_id = null`).
   ```bash
   cd images/aws
   packer init .
   packer build -var-file=defaults.pkrvars.hcl clickhouse.pkr.hcl
   ```
2. **AWS credentials** (any standard source) with permissions for EC2, VPC,
   S3, DynamoDB, Secrets Manager, Lambda, CloudWatch, Route 53, IAM,
   GuardDuty, and Security Hub.
3. **The ACME provider** declared once in your root module (the
   [dns_tls](../modules/dns_tls) module can issue Let's Encrypt certs; the
   provider schema requires `server_url` at config time even if you never
   use it):
   ```hcl
   provider "acme" {
     server_url = "https://acme-staging-v02.api.letsencrypt.org/directory"
   }
   ```

## Provenance

This module is the same wiring a large production fleet runs
(network/compute/storage/secrets/monitoring/security extracted verbatim and
hardened); the facade keeps the variable surface deliberately small. When
you outgrow it — existing VPCs, multi-AZ, NLBs, public DNS, mixed instance
pools, vertical autoscaling — compose the ecosystem modules directly
(start from [`examples/cluster`](./examples/cluster) and the module table
in [`../README.md`](../README.md)); the all-in-one module never stands in
the way.

## Key Outputs

- `node_endpoints` — private DNS hostnames per role
- `connect_writer` — a ready-to-run `clickhouse client` one-liner
- `secrets` — the Secrets Manager secret names (values resolve at boot)
- `writers_asg_name` / `readers_asg_name` — ASG names for scaling operations
- `alert_topic_arn` — the SNS topic carrying alarms + security findings
- `config_files_hash` — the SHA-256 gate digest over the config bundle

## Troubleshooting a Node

```bash
aws ssm start-session --target <instance-id>   # filters in the ssm_targets output
sudo cat /var/lib/chinit/state.json
journalctl -u chinit-boot.service -n 50
systemctl list-timers | grep chinit
```
