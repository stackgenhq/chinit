# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Locals — DNS + TLS Module
# ============================================================================
# All derived values for this module live here, in ONE locals block (open
# source Terraform convention): resources below reference local.* only.
# ============================================================================

locals {
  is_passthrough = var.nlb_tls_mode == "passthrough"

  # Why: The ACME provider's server_url lives on the provider block in the
  #      ROOT module (it is provider-wide). This local only documents the
  #      mapping so the root passes the right URL per acme_environment:
  #        staging    -> https://acme-staging-v02.api.letsencrypt.org/directory
  #        production -> https://acme-v02.api.letsencrypt.org/directory
  acme_server_url = var.acme_environment == "production" ? "https://acme-v02.api.letsencrypt.org/directory" : "https://acme-staging-v02.api.letsencrypt.org/directory"

  # Private client zone (optional) — Grafana/EKS reach ClickHouse over private DNS.
  # The zone resource is count-gated on client_private_zone_name — a null
  # name (zone disabled, the dev shape) leaves the resource empty, so the
  # index must never fire: only read .zone_id when the resource EXISTS,
  # else fall back to the passed-in zone id (or empty when disabled —
  # the consumers gate on client_zone_enabled).
  client_zone_enabled = var.client_private_zone_name != null
  client_zone_id      = (var.create_client_private_zone && var.client_private_zone_name != null) ? aws_route53_zone.client_private[0].zone_id : (var.client_private_zone_id != null ? var.client_private_zone_id : "")
  client_zone_vpc_ids = coalesce(var.client_zone_vpc_ids, [var.vpc_id])
}
