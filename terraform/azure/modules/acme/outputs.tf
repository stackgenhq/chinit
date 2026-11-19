# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ACME Module Outputs
# ============================================================================

output "certificate_secret_name" {
  description = "Name of the TLS certificate secret in Key Vault"
  value       = azurerm_key_vault_secret.certificate.name
}

output "private_key_secret_name" {
  description = "Name of the TLS private key secret in Key Vault"
  value       = azurerm_key_vault_secret.private_key.name
}

output "ca_certificate_secret_name" {
  description = "Name of the CA certificate secret in Key Vault"
  value       = azurerm_key_vault_secret.ca_certificate.name
}

output "certificate_common_name" {
  description = "Common Name of the generated certificate"
  value       = acme_certificate.certificate.common_name
}

output "certificate_expiry" {
  description = "Expiration date of the certificate"
  value       = acme_certificate.certificate.certificate_not_after
}

output "service_principal_client_id" {
  description = "Client ID of the Service Principal used for DNS authentication"
  value       = azuread_application.acme_dns.client_id
}
