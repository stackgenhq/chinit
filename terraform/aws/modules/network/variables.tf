# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Network Module Variables
# ============================================================================
# Inputs mirror the root module's network_config object (aws/variables.tf),
# flattened to plain module inputs per the module contract. Defaults match
# the root's optional() defaults so the module validates standalone.
# ============================================================================

variable "aws_region" {
  description = "AWS region. Used for the S3 gateway endpoint service name and the flow-log delivery policy source ARN. Must match the region configured on the AWS provider by the root module."
  type        = string
  default     = "us-east-1"
}

variable "cluster_name" {
  description = "Name of the ClickHouse cluster. Used to derive every resource name and the Name tag on every resource. Must be lowercase/DNS-safe (also becomes part of the flow-log S3 bucket name, which is globally unique)."
  type        = string
}

variable "vpc_cidr" {
  description = "IPv4 CIDR block for the VPC. When the public subnet is auto-carved (public_subnet_cidr null), the last /24 of this range is used for the public (NLB + NAT) subnet."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    condition     = can(cidrnetmask(var.vpc_cidr))
    error_message = "vpc_cidr must be a valid IPv4 CIDR block (e.g. 10.0.0.0/16)."
  }

  # Standalone prefix-bits check only (cross-variable validation referencing
  # enable_nat_gateway requires TF 1.9+; the NAT-vs-VPC-size interaction is
  # enforced by a lifecycle precondition in main.tf instead).
  validation {
    condition     = tonumber(split("/", var.vpc_cidr)[1]) <= 24
    error_message = "vpc_cidr prefix must be /24 or larger (e.g. 10.0.0.0/16) so a /24 public subnet can be carved from it when needed."
  }
}

variable "subnet_cidr" {
  description = "IPv4 CIDR block for the PRIVATE nodes subnet (single subnet, single AZ by DEFAULT; the multi-AZ opt-in extends the AZ span while this primary subnet and all static nodes stay in its AZ). All ClickHouse nodes live here; nothing in this subnet is internet-reachable except via the NLB."
  type        = string
  default     = "10.0.1.0/24"

  validation {
    condition     = can(cidrnetmask(var.subnet_cidr))
    error_message = "subnet_cidr must be a valid IPv4 CIDR block (e.g. 10.0.1.0/24)."
  }
}

variable "public_subnet_cidr" {
  description = "IPv4 CIDR block for the PUBLIC subnet hosting the internet-facing NLBs and the NAT gateway. null = auto-carve the LAST /24 of vpc_cidr. This subnet has an internet-gateway default route; NO ClickHouse nodes are ever placed in it."
  type        = string
  default     = null

  validation {
    condition     = var.public_subnet_cidr == null || can(cidrnetmask(var.public_subnet_cidr))
    error_message = "public_subnet_cidr must be a valid IPv4 CIDR block (e.g. 10.0.2.0/24) or null for auto-carve."
  }
}

variable "enable_nat_gateway" {
  description = "Provision a NAT gateway for bootstrap/apt egress only. ClickHouse<->S3 traffic always rides the free S3 gateway endpoint regardless of this setting. false = nodes are air-gapped except for in-VPC S3 access."
  type        = bool
  default     = true
}

variable "enable_flow_logs" {
  description = "Publish VPC flow logs to S3 for auditing, anomaly detection and security investigations (parity with the Azure module's enable_vnet_flow_logs; metered storage makes it opt-in)."
  type        = bool
  default     = false
}

variable "flow_log_retention_days" {
  description = "Days to retain VPC flow log objects, enforced via S3 lifecycle expiration on the module-created bucket. Only applies when the module creates its own bucket (flow_log_bucket_arn = null)."
  type        = number
  default     = 30

  validation {
    condition     = var.flow_log_retention_days >= 1
    error_message = "flow_log_retention_days must be at least 1 (S3 lifecycle expiration floor)."
  }
}

variable "flow_log_bucket_arn" {
  description = "Existing S3 bucket ARN to receive flow logs. null (default) = the module creates a dedicated versioned bucket with lifecycle expiration when enable_flow_logs is true. When set, lifecycle/retention are the caller's responsibility."
  type        = string
  default     = null
}

variable "allowed_source_ips" {
  description = "Client CIDRs allowed to reach ClickHouse ports 8123/9000/8443/9440 on writers and readers. May be empty = no client ingress rules are created yet (the root auto-detects the operator IP and passes the resolved list)."
  type        = list(string)
  default     = []
}

variable "allowed_ssh_ips" {
  description = "CIDRs allowed to reach port 22 on node security groups. May be empty (default) = SSH is disabled in favor of SSM Session Manager; the port-22 rule is created ONLY when this list is non-empty."
  type        = list(string)
  default     = []
}

variable "tags" {
  description = "Tags merged into every resource (the module adds the Name tag on top)."
  type        = map(string)
  default     = {}
}

# ----------------------------------------------------------------------------
# Existing-VPC (BYO-VPC) mode — deploy into a pre-existing VPC instead of
# creating one. All defaults preserve the create-mode behavior.
# ----------------------------------------------------------------------------

variable "mode" {
  description = "Network topology mode: 'create' (default) builds a new VPC/subnets/NAT via aws-ia/vpc as before; 'existing' attaches to a pre-existing VPC (shared/managed VPC, e.g. one hosting an EKS cluster) and creates ONLY additive resources (nodes subnet optionally, security groups, route table for the nodes subnet, endpoints). Additive-only: the module never modifies or deletes pre-existing VPC resources."
  type        = string
  default     = "create"

  validation {
    condition     = contains(["create", "existing"], var.mode)
    error_message = "mode must be 'create' or 'existing'."
  }
}

variable "existing_vpc_id" {
  description = "(existing mode) ID of the pre-existing VPC to deploy into. Required when mode = 'existing'."
  type        = string
  default     = null
}

variable "existing_vpc_cidr" {
  description = "(existing mode) Primary CIDR block of the pre-existing VPC. Used for CIDR containment validation only. Required when mode = 'existing'."
  type        = string
  default     = null
}

variable "existing_nodes_subnet_id" {
  description = "(existing mode) Pre-existing subnet to place ClickHouse nodes in. Mutually exclusive with create_nodes_subnet = true. The subnet must sit in existing_vpc_id and have room for the node IPs. Its route table is NOT modified by this module."
  type        = string
  default     = null
}

variable "create_nodes_subnet" {
  description = "(existing mode) Create a NEW dedicated subnet inside the existing VPC for ClickHouse nodes (recommended for shared VPCs: isolates route table, NACLs and future peering from the rest of the VPC). The module creates the subnet, its own route table (optionally with a default route via create_nat_gateway / existing NAT/IGW), and the S3 gateway endpoint attachment. Defaults to false."
  type        = bool
  default     = false
}

variable "nodes_subnet_cidr" {
  description = "(existing mode, create_nodes_subnet = true) CIDR for the new nodes subnet. Must be inside existing_vpc_cidr and disjoint from all existing subnets."
  type        = string
  default     = null
}

variable "nodes_subnet_az" {
  description = "(existing mode, create_nodes_subnet = true) Availability zone for the new nodes subnet, e.g. 'us-east-1a'. Single-AZ by design (cluster placement group)."
  type        = string
  default     = null
}

variable "existing_nodes_route_table_id" {
  description = "(existing mode, create_nodes_subnet = false) Route table to attach the S3 gateway endpoint to. Defaults to the main route table of the existing VPC when null. Ignored when create_nodes_subnet = true (the module creates and uses its own route table)."
  type        = string
  default     = null
}

variable "create_nodes_route_table" {
  description = "(existing mode, create_nodes_subnet = true) Create a dedicated route table for the new nodes subnet (default true). Disable only to attach the new subnet to an existing route table you manage."
  type        = bool
  default     = true
}

variable "nodes_route_table_id" {
  description = "(existing mode, create_nodes_subnet = true, create_nodes_route_table = false) Existing route table to associate the new nodes subnet with, and to attach the S3 gateway endpoint to."
  type        = string
  default     = null
}

variable "create_nat_gateway" {
  description = "(existing mode, create_nodes_subnet = true) Create a single NAT gateway (in an existing public subnet) and route 0.0.0.0/0 of the nodes route table through it. False (default) leaves the nodes subnet without a default route (S3-only egress via the gateway endpoint; everything else must go via interface endpoints supplied in vpc_endpoint_services)."
  type        = bool
  default     = false
}

variable "nat_subnet_id" {
  description = "(existing mode, create_nat_gateway = true) Existing public subnet (IGW-routed) to place the NAT gateway in. Required when create_nat_gateway = true."
  type        = string
  default     = null
}

variable "vpc_endpoint_services" {
  description = "(existing mode) Interface VPC endpoint service short names to create for the nodes subnet, e.g. [\"ssm\", \"ssmmessages\", \"ec2messages\", \"logs\", \"monitoring\", \"secretsmanager\"]. Each costs ~$7/mo per AZ. The S3 and DynamoDB gateway endpoints are always managed separately (see create_s3_gateway_endpoint). Empty = none."
  type        = list(string)
  default     = []
}

variable "create_s3_gateway_endpoint" {
  description = "Create the (free) S3 gateway endpoint for the nodes route table. Always true in create mode; disable only if the target VPC already has an S3 gateway endpoint attached to the same route table."
  type        = bool
  default     = true
}

variable "create_dynamodb_gateway_endpoint" {
  description = "Create the (free) DynamoDB gateway endpoint (used by the node-claims table on GetItem/PutItem/Scan). Defaults to false in existing mode (the account may already have one); in create mode it is not created either (claims table access goes through the NAT path today)."
  type        = bool
  default     = false
}

variable "multi_az" {
  description = "OPT-IN multi-AZ topology (default false — Single-AZ compute). true = the VPC/topology spans 3 AZs: the nodes' private subnets extend to one per AZ, the NLBs subnets span the AZs, and NAT follows nat_gateway_mode. Keepers and the backup replica stay in the PRIMARY nodes subnet's AZ by design (raft/metadata EBS volumes are AZ-pinned; quorum already spans racks via the ASG's placement)."
  type        = bool
  default     = false
}

variable "nat_gateway_mode" {
  description = "NAT gateway topology when enable_nat_gateway = true and multi_az = true: 'single_az' (default — one NAT gateway in the primary AZ shared across all 3 private nodes route tables; ClickHouse<->S3 always rides the free S3 gateway endpoint) or 'all_azs' (one NAT gateway per AZ)."
  type        = string
  default     = "single_az"

  validation {
    condition     = contains(["single_az", "all_azs"], var.nat_gateway_mode)
    error_message = "nat_gateway_mode must be 'single_az' or 'all_azs'."
  }
}

variable "nlb_subnet_ids" {
  description = "Subnets for the ClickHouse NLBs. In create mode this is derived from public_subnet_cidr and ignored. In existing mode: subnets for the internal NLB — typically the nodes subnet itself or dedicated private subnets in each AZ. Defaults to [nodes subnet] when null."
  type        = list(string)
  default     = null
}

variable "client_security_group_ids" {
  description = "(existing mode) Security group IDs allowed client access on the ClickHouse client ports (8123/9000/8443/9440) on the writers/readers SGs — the preferred ingress mechanism on shared VPCs (e.g. the EKS node SG, which covers all pods). Combined with allowed_source_ips; if both are empty NO client ingress rule is created (fail closed)."
  type        = list(string)
  default     = []
}

variable "endpoint_security_group_ids" {
  description = "(existing mode) Extra security group IDs to attach to the interface VPC endpoints created by this module (the nodes SG is always attached). Typically the calling services' SGs (e.g. EKS nodes) need no rule because endpoint SG ingress is from the nodes SG only."
  type        = list(string)
  default     = []
}
