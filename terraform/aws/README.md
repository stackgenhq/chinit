<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# `chinit` Cluster Module — AWS

Deploy a ClickHouse cluster on AWS EC2 VMs, bootstrapped and managed by [chinit](https://github.com/stackgenhq/chinit):

```hcl
module "clickhouse" {
  source = "github.com/stackgenhq/chinit//terraform/aws"

  cluster_name  = "demo"
  region        = "us-west-2"
  golden_ami_id = "ami-…" # built from images/aws with Packer

  dns_zone_name = "clickhouse.internal"
  subnet_cidr   = "10.42.0.0/24"
}
```

## Provisioned Resources

| Component | Resources Created |
|---|---|
| Writers (write path) | On-demand Auto Scaling Group, lease identity (`writer-N`), metadata EBS volume + S3 cache volume |
| Readers (query path) | Spot-capable Auto Scaling Group, lease identity (`reader-N`) |
| Keepers (Raft coordination) | 3 fixed EC2 instances with static private IPs and persistent Raft EBS volumes |
| Backup replica | Optional fixed EC2 instance at the `+30` subnet offset running `clickhouse-backup` |
| Configuration bundle | S3 bucket storing per-role specs, `chi.yaml`, `keeper.yaml`, static XMLs, and bootstrap DDL (SHA-256 verified) |
| Identity & DNS | DynamoDB ordinal-lease table (TTL enabled) and Route 53 private hosted zone |
| Secrets | AWS Secrets Manager secrets (randomly generated; specs reference secret names only) |
| IAM | Instance profile granting access to the bundle bucket, secrets, lease table, private DNS zone, S3 data/backup buckets, and online volume expansion |

External load balancers, custom scaling policies, secret rotation Lambdas, and bastion hosts are intentionally left to your root module.

## Prerequisites

1. **A golden AMI** built from [`images/aws`](../../images/aws) (or copied from the prebuilt public AMI):
   ```bash
   cd images/aws
   packer init .
   packer build -var-file=defaults.pkrvars.hcl clickhouse.pkr.hcl
   ```
2. **AWS credentials** with permissions to manage EC2, VPC, S3, DynamoDB, Secrets Manager, Route 53, and IAM resources.

## How This Module Works with `chinit`

- **Declarative spec**: Instance `user_data` is a small (~1.2 KB) wrapper that fetches `chinit-<role>.yaml` from S3 and starts `chinit-boot.service`.
- **SHA-256 verified bundle**: Each spec pins `config_bundle.sha256` to `sha256(jsonencode(merge(config_files, {"chi.yaml": chi_spec})))`, which `chinit` verifies at boot.
- **Lease identity for ASGs**: Writers and readers claim `writer-N` and `reader-N` ordinals from DynamoDB. `writer_max` and `reader_max` define the Auto Scaling Group ceilings.
- **Static identity for fixed nodes**: Keepers (`+20..+29`) and the backup replica (`+30`) derive their hostname from their fixed private IP offset.
- **Secrets by name**: Terraform creates the secrets in Secrets Manager; `chinit` resolves their values at boot into `0600` files using the EC2 instance role.

## Disk Layout

| Device | Filesystem Label | Mount Point | Role |
|---|---|---|---|
| `/dev/sdb` | `ch-metadata` | `/var/lib/clickhouse` | Hot data and metadata (`writer`, `reader`, `backup`) |
| `/dev/sdc` | `ch-cache` | `/var/lib/clickhouse/disks` | Local S3 read cache (`writer`, `reader`) |
| `/dev/sdf` | `ch-keeper` | `/var/lib/clickhouse-keeper` | Persistent Raft log (`keeper`) |

`chinit` mounts volumes by filesystem `LABEL` so NVMe device-node numbering differences across instance types do not affect mounts.

## Sizing Notes

- `writer_max` and `reader_max` must match the maximum capacity of their respective Auto Scaling Groups.
- `keeper_count` must be an odd number (typically 3) to maintain Raft quorum.
- `cache_disk_size_gb` automatically sets `<max_size>` in `storage.xml` to 90% of the cache volume size.
- `metadata_disk_size_gb` can grow online via `chinit disk-autoscale` when free space drops below the threshold.

## Key Outputs

- `node_endpoints` — Private DNS hostnames for writers, readers, keepers, and the backup replica
- `secrets` — AWS Secrets Manager secret names
- `writers_asg_name` / `readers_asg_name` — Auto Scaling Group names
- `config_files_hash` — SHA-256 digest of the configuration bundle

## Troubleshooting a Node

```bash
aws ssm start-session --target <instance-id>
sudo cat /var/lib/chinit/state.json
journalctl -u chinit-boot.service -n 50
systemctl list-timers | grep chinit
```

## Using an Existing VPC

Set `create_network = false` and pass `vpc_id`, `subnet_id`, and `subnet_cidr`. Ensure the `.20+` and `.30` IP offsets in the subnet are available for the keeper and backup instances.
