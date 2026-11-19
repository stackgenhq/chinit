# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# DNS + TLS Module Outputs
# ============================================================================

# Private zone
output "private_zone_id" {
  description = "Route53 private hosted zone id (node discovery)"
  value       = aws_route53_zone.private.zone_id
}

output "private_zone_name" {
  description = "Private hosted zone name (used in ClickHouse hostnames)"
  value       = aws_route53_zone.private.name
}

# ACM (acm mode)
output "acm_certificate_arn" {
  description = "ACM certificate ARN for NLB termination (acm mode): the supplied existing ARN when provided, otherwise the newly issued cert"
  value       = var.nlb_tls_mode == "acm" ? coalesce(var.acm_certificate_arn, try(aws_acm_certificate.nlb[0].arn, null)) : null
}

# ACME (passthrough mode) — secret names for cloud-init
output "tls_certificate_secret_name" {
  description = "Secrets Manager secret name holding the wildcard cert chain"
  value       = local.is_passthrough ? aws_secretsmanager_secret.tls_certificate[0].name : null
}

output "tls_private_key_secret_name" {
  description = "Secrets Manager secret name holding the cert private key"
  value       = local.is_passthrough ? aws_secretsmanager_secret.tls_private_key[0].name : null
}

output "tls_ca_certificate_secret_name" {
  description = "Secrets Manager secret name holding the issuer CA"
  value       = local.is_passthrough ? aws_secretsmanager_secret.tls_ca_certificate[0].name : null
}

output "cert_expiration_date" {
  description = "Certificate expiry (feeds the cert-expiry alarm)"
  value       = local.is_passthrough ? acme_certificate.cluster[0].certificate_not_after : null
}

# Public records
output "writers_fqdn" {
  description = "Public FQDN of the writers endpoint (matches cert SAN)"
  value       = var.public_zone_name != null ? "${var.writers_record_name}.${var.public_zone_name}" : null
}

output "readers_fqdn" {
  description = "Public FQDN of the readers endpoint (matches cert SAN)"
  value       = var.public_zone_name != null ? "${var.readers_record_name}.${var.public_zone_name}" : null
}

output "https_writers_fqdn_endpoint" {
  description = "HTTPS writers endpoint with valid-SSL FQDN"
  value       = var.public_zone_name != null ? "https://${var.writers_record_name}.${var.public_zone_name}:8443" : null
}

output "https_readers_fqdn_endpoint" {
  description = "HTTPS readers endpoint with valid-SSL FQDN"
  value       = var.public_zone_name != null ? "https://${var.readers_record_name}.${var.public_zone_name}:8443" : null
}

output "acme_server_url" {
  description = "ACME directory URL for the root provider block (staging vs production)"
  value       = local.is_passthrough ? local.acme_server_url : null
}

output "client_private_zone_id" {
  description = "ID of the private client zone (null when not configured)."
  value       = local.client_zone_enabled ? local.client_zone_id : null
}

output "client_writers_fqdn" {
  description = "Private FQDN for the writers (ingest) endpoint."
  value       = var.client_private_zone_name != null ? "${var.client_writers_record_name}.${var.client_private_zone_name}" : null
}

output "client_readers_fqdn" {
  description = "Private FQDN for the readers (query) endpoint."
  value       = var.client_private_zone_name != null ? "${var.client_readers_record_name}.${var.client_private_zone_name}" : null
}
