# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — Network Module
# ============================================================================
# All derived values for this module live here, in ONE locals block (open
# source Terraform convention): resources below reference local.* only.
# ============================================================================

locals {
  # Client-facing ClickHouse ports. The root module exposes both plain and
  # TLS variants; the security groups below open both so operators can flip
  # between them (e.g. ch-go native vs HTTP) without a networking change.
  client_ports = {
    http       = { port = 8123, description = "ClickHouse HTTP interface" }
    native     = { port = 9000, description = "ClickHouse native protocol" }
    https_tls  = { port = 8443, description = "ClickHouse HTTPS interface (TLS)" }
    native_tls = { port = 9440, description = "ClickHouse native protocol (TLS)" }
  }

  # ---- Public subnet CIDR (NLBs + NAT gateway) ----
  # Auto-carve the LAST /24 of the VPC when public_subnet_cidr is null.
  # Why the last block: keeps the low addresses free for the operator's
  #      private ranges; parity with the previous NAT-carve convention.
  vpc_prefix_bits       = tonumber(split("/", var.vpc_cidr)[1])
  public_subnet_newbits = 24 - local.vpc_prefix_bits
  public_subnet_cidr = var.public_subnet_cidr != null ? var.public_subnet_cidr : cidrsubnet(
    var.vpc_cidr,
    local.public_subnet_newbits,
    pow(2, local.public_subnet_newbits) - 1,
  )

  # ---- AZ span (the multi-AZ opt-in; default Single-AZ) ----
  # Single-AZ (default): one AZ, ONE /24 nodes subnet (az_count = 1) —
  # the cost posture (sub-ms peer RTT, $0 cross-AZ part traffic; an
  # AZ outage is an ASG re-launch). The opt-in
  # spans 3 AZs: the PRIMARY nodes subnet keeps its caller-supplied
  # CIDR + AZ (static nodes, their EBS volumes, the NAT, and the NLBs
  # stay pinned there), and one /26 per EXTRA AZ is carved from the
  # VPC's tail. aws-ia/vpc requires one cidr per AZ for every subnet
  # role, so both lists below grow to 3 under the opt-in.
  multi_az_az_count = var.multi_az ? 3 : 1

  # Extra-AZ nodes carve math: /26 slots within the VPC (newbits =
  # 26 - vpc_prefix_bits). The auto-carved public /24 always occupies
  # the LAST 4 /26 slots (the tail /24); the two extras take the last
  # two /26 slots of the /24 BEFORE it, i.e. 2 slots clear of the
  # public block. Worked example (10.0.0.0/16): public = 10.0.255.0/24
  # (slots 1020–1023), extras = 10.0.254.128/26 + 10.0.254.192/26
  # (slots 1018–1019). The cidr_guards check below fails the plan if a
  # caller's primary nodes CIDR or an explicitly-pinned public CIDR
  # collides with the carve.
  extra_nodes_newbits = 26 - local.vpc_prefix_bits
  nodes_subnet_cidrs = concat(
    [var.subnet_cidr],
    [
      for i in range(var.multi_az ? 2 : 0) : cidrsubnet(
        var.vpc_cidr,
        local.extra_nodes_newbits,
        pow(2, local.extra_nodes_newbits) - 6 + i
      )
    ]
  )
  # Public subnets: keep the primary public subnet at local.public_subnet_cidr
  # (e.g. 10.0.255.0/24 in us-west-2a) so enabling multi_az on a live Single-AZ
  # cluster preserves the primary public subnet, NAT gateway, and NLBs in-place
  # with zero replacement; carve two extra /26 public subnets for AZs 2 & 3 from
  # slots -8..-7 (e.g. 10.0.254.0/26 + 10.0.254.64/26 right beside the two extra
  # private /26s 10.0.254.128/26 + 10.0.254.192/26).
  public_prefix_bits = tonumber(split("/", local.public_subnet_cidr)[1])
  extra_public_cidrs = [
    for i in range(var.multi_az ? 2 : 0) : cidrsubnet(
      var.vpc_cidr,
      local.extra_nodes_newbits,
      pow(2, local.extra_nodes_newbits) - 8 + i
    )
  ]
  public_subnet_cidrs = concat([local.public_subnet_cidr], local.extra_public_cidrs)

  # ---- Flow-log bucket decision ----
  # A caller-provided bucket wins; otherwise the module creates a small
  # dedicated versioned bucket with lifecycle expiry.
  create_flow_log_bucket = var.enable_flow_logs && var.flow_log_bucket_arn == null

  # ---- public/private subnet overlap guards (Terraform 1.5-compatible) ----
  # Two IPv4 CIDR blocks never partially overlap (they are either disjoint
  # or nested), so comparing the top min(prefix_a, prefix_b) bits of their
  # network addresses decides overlap. IPv4 addresses are converted to
  # integers via octet math; every value stays below 2^32 so float64
  # arithmetic is exact. (cidrcontains is TF 1.6+ and the module pins 1.5.)
  # Integer-overlap guards reference public_prefix_bits (declared
  # beside the carve math above — one definition serves both the AZ
  # span and the guard math).
  nodes_prefix_bits = tonumber(split("/", var.subnet_cidr)[1])

  nodes_net_int = sum([
    for i, octet in split(".", cidrhost(var.subnet_cidr, 0)) :
    tonumber(octet) * pow(256, 3 - i)
  ])

  public_net_int = sum([
    for i, octet in split(".", cidrhost(local.public_subnet_cidr, 0)) :
    tonumber(octet) * pow(256, 3 - i)
  ])

  vpc_net_int = sum([
    for i, octet in split(".", cidrhost(var.vpc_cidr, 0)) :
    tonumber(octet) * pow(256, 3 - i)
  ])

  # Nodes (private) subnet must sit inside the VPC (AWS only reports at apply).
  nodes_subnet_inside_vpc = (
    floor(local.nodes_net_int / pow(2, 32 - local.vpc_prefix_bits))
    == floor(local.vpc_net_int / pow(2, 32 - local.vpc_prefix_bits))
  )

  # Public subnet vs nodes (private) subnet: overlap in either direction.
  subnet_overlap_top_bits = 32 - min(local.public_prefix_bits, local.nodes_prefix_bits)
  public_subnet_overlaps_nodes = (
    floor(local.nodes_net_int / pow(2, local.subnet_overlap_top_bits))
    == floor(local.public_net_int / pow(2, local.subnet_overlap_top_bits))
  )

  # Public subnet must also sit inside the VPC.
  public_subnet_inside_vpc = (
    floor(local.public_net_int / pow(2, 32 - local.vpc_prefix_bits))
    == floor(local.vpc_net_int / pow(2, 32 - local.vpc_prefix_bits))
  )

  # ---- Existing-VPC (BYO-VPC) mode ----
  # mode = "create" (default): nodes/public subnets come from aws-ia/vpc.
  # mode = "existing": deploy into pre-existing VPC; create only additive
  # resources. Everything below is a no-op-equivalent in create mode.
  is_create_mode = var.mode == "create"
  is_existing    = !local.is_create_mode

  vpc_id_effective = local.is_create_mode ? module.vpc[0].vpc_attributes.id : var.existing_vpc_id

  # Nodes subnet: created inside existing VPC, or reused as-is. The
  # PRIMARY subnet answers Single-AZ; the multi-AZ opt-in adds the
  # per-AZ extras (see nodes_subnet_cidrs above) for the ASG roles to
  # spread across — keepers/backup stay in the primary (its AZ/EBS
  # contract never moves). Multi-AZ is a create-mode affordance:
  # existing mode carries the caller's own single AZ as before.
  create_nodes_subnet_eff = local.is_existing && var.create_nodes_subnet
  nodes_subnet_id = (
    local.is_create_mode ? values(module.vpc[0].subnet_ids_by_role.nodes)[0] :
    local.create_nodes_subnet_eff ? aws_subnet.nodes[0].id :
    var.existing_nodes_subnet_id
  )

  # Per-AZ nodes subnet ids (create mode): S3 gateway routes cover every
  # nodes route table, so cross-AZ peer links keep the free path.
  nodes_subnet_ids = (
    local.is_create_mode && var.multi_az ? [for az in sort(keys(module.vpc[0].subnet_ids_by_role.nodes)) : module.vpc[0].subnet_ids_by_role.nodes[az]] :
    [local.nodes_subnet_id]
  )

  # CIDR/AZ of the nodes subnet. For a reused subnet the values come from a
  # data lookup (locals cannot see resource attributes before apply without it).
  nodes_subnet_cidr_eff = (
    local.is_create_mode ? var.subnet_cidr :
    local.create_nodes_subnet_eff ? var.nodes_subnet_cidr :
    data.aws_subnet.existing_nodes[0].cidr_block
  )
  nodes_az_eff = (
    local.is_create_mode ? module.vpc[0].azs[0] :
    local.create_nodes_subnet_eff ? var.nodes_subnet_az :
    data.aws_subnet.existing_nodes[0].availability_zone
  )

  # Route table that gets the S3 gateway endpoint + (optionally) the NAT default route.
  # own RT (created) > caller-provided RT > main RT of the VPC (via subnet lookup).
  nodes_route_table_id_eff = (
    local.create_nodes_subnet_eff && var.create_nodes_route_table ? aws_route_table.nodes[0].id :
    local.create_nodes_subnet_eff && var.nodes_route_table_id != null ? var.nodes_route_table_id :
    var.existing_nodes_route_table_id != null ? var.existing_nodes_route_table_id :
    local.is_existing ? data.aws_route_table.nodes[0].id :
    null
  )
  s3_gateway_route_tables = local.is_create_mode ? values(module.vpc[0].route_table_ids_by_type_by_az.private) : [local.nodes_route_table_id_eff]

  # NLB placement: existing mode defaults to the nodes subnet (internal NLB); create mode keeps the public subnet.
  # NLB subnets. In existing mode with a dedicated nodes subnet the nodes subnet
  # is created by this module, so subnet ids are unknown until apply; the sentinel
  # keeps for_each/count keys static while values resolve at apply time.
  # Multi-AZ (create mode): the public subnets span the AZs (one NAT per
  # AZ), so the internet-facing NLBs register one subnet per AZ — the
  # AWS-assigned static IPs then survive a single-AZ failure.
  nlb_subnet_ids_raw = local.is_create_mode ? [] : (var.nlb_subnet_ids != null ? var.nlb_subnet_ids : (local.create_nodes_subnet_eff ? ["__nodes_subnet__"] : [var.existing_nodes_subnet_id]))
  nlb_subnet_ids     = local.is_create_mode ? (var.multi_az ? [for az in sort(keys(module.vpc[0].subnet_ids_by_role.public)) : module.vpc[0].subnet_ids_by_role.public[az]] : [values(module.vpc[0].subnet_ids_by_role.public)[0]]) : [for s in local.nlb_subnet_ids_raw : s == "__nodes_subnet__" ? aws_subnet.nodes[0].id : s]
  nlb_subnet_cidrs   = local.is_create_mode ? (var.multi_az ? local.public_subnet_cidrs : [local.public_subnet_cidr]) : [for s in local.nlb_subnet_ids_raw : s == "__nodes_subnet__" ? local.nodes_subnet_cidr_eff : data.aws_subnet.nlb[s].cidr_block]

  # Client ingress enabled when either CIDRs or SG sources are configured.
  client_ingress_enabled  = length(var.allowed_source_ips) > 0 || length(var.client_security_group_ids) > 0
  client_sg_ingress_pairs = var.client_security_group_ids
  # Client-port CIDR rules include every nodes subnet (the Single-AZ
  # shape carries just the primary) and (create mode) the public subnet.
  node_cidrs = var.multi_az ? local.nodes_subnet_cidrs : [local.nodes_subnet_cidr_eff]
  client_cidrs = distinct(concat(
    var.allowed_source_ips,
    local.node_cidrs,
    local.nlb_subnet_cidrs,
  ))
  client_cidr_ingress_pairs = local.client_ingress_enabled ? {
    for pair in setproduct(keys(local.client_ports), local.client_cidrs) : "${pair[0]}|${pair[1]}" => {
      port        = local.client_ports[pair[0]].port
      description = local.client_ports[pair[0]].description
      cidr        = pair[1]
    }
  } : {}

  # NAT egress IPs: one per AZ under the multi-AZ opt-in (each nodes
  # subnet routes its own AZ's gateway); the Single-AZ shape carries
  # the single well-known EIP. nat_gateway_public_ips (the output)
  # surfaces the full list; the well-known single value consumers
  # allowlist rides nat_public_ip (first AZ under the opt-in).
  nat_public_ips_single = var.enable_nat_gateway ? values(module.vpc[0].nat_public_ips) : []
  nat_public_ip = local.is_create_mode ? (
    var.enable_nat_gateway && length(local.nat_public_ips_single) > 0 ? sort(local.nat_public_ips_single)[0] : null
  ) : (var.create_nat_gateway ? aws_eip.nat[0].public_ip : null)
}
