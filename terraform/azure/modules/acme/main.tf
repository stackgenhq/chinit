# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# ACME (Let's Encrypt) Module - Main Configuration
# ============================================================================
# This module handles:
# - Service Principal creation for DNS authentication
# - ACME account registration with Let's Encrypt
# - Wildcard certificate generation via DNS-01 challenge
# - Storage of certificates in Azure Key Vault
# ============================================================================

# ============================================================================
# Service Principal for ACME DNS Authentication
# ============================================================================
# Why: The ACME provider (lego) cannot use Azure CLI credentials when running
#      locally. MSI is only available on Azure VMs. Creating a dedicated SP
#      with DNS Zone Contributor permissions allows the ACME provider to
#      authenticate and create the required DNS TXT records for validation.
# Without it: "MSI not available" error when running from local machines.
# ============================================================================
resource "azuread_application" "acme_dns" {
  display_name = "${var.acme_config.cluster_name}-acme-dns-automation"
}

resource "azuread_service_principal" "acme_dns" {
  client_id = azuread_application.acme_dns.client_id
}

resource "time_offset" "password_expiry" {
  offset_years = 1
}

resource "azuread_service_principal_password" "acme_dns" {
  service_principal_id = azuread_service_principal.acme_dns.id
  end_date             = time_offset.password_expiry.rfc3339
}

# Get the DNS Zone to assign permissions
data "azurerm_dns_zone" "main" {
  name                = var.acme_config.base_domain
  resource_group_name = var.acme_config.dns_resource_group_name
}

# Grant DNS Zone Contributor role to the Service Principal
resource "azurerm_role_assignment" "acme_dns_contributor" {
  scope                = data.azurerm_dns_zone.main.id
  role_definition_name = "DNS Zone Contributor"
  principal_id         = azuread_service_principal.acme_dns.object_id

  lifecycle {
    # Prevent replacement when the Azure-generated name differs from state
    ignore_changes = all
  }
}

# Wait for role assignment to propagate
resource "time_sleep" "acme_role_propagation" {
  depends_on      = [azurerm_role_assignment.acme_dns_contributor]
  create_duration = "30s"
}

# ============================================================================
# ACME Registration (Let's Encrypt Account)
# ============================================================================
# Why: Registers an account with Let's Encrypt to request certificates.
#      Requires an email address for important notifications (expiry, revocation).
# Without it: Cannot authenticate with ACME server to request certificates.
# ============================================================================
resource "tls_private_key" "acme_account" {
  algorithm = "RSA"
  rsa_bits  = 4096
}

resource "acme_registration" "main" {
  account_key_pem = tls_private_key.acme_account.private_key_pem
  email_address   = var.acme_config.email
}

# ============================================================================
# ACME Certificate (Let's Encrypt SSL)
# ============================================================================
# Why: Requests a wildcard SSL certificate (*.cluster.domain) using DNS validation.
#      Wildcard allows all nodes (writer-0, reader-0, etc.) to use the same cert.
#      Uses Azure DNS to prove domain ownership via DNS-01 challenge.
# Without it: Must use self-signed certificates (untrusted) or manually purchase
#             and install commercial certificates.
# ============================================================================
resource "acme_certificate" "certificate" {
  account_key_pem = acme_registration.main.account_key_pem

  # Use wildcard domain for the cluster
  common_name = "*.${var.acme_config.cluster_name}.${var.acme_config.base_domain}"

  # Also include the base cluster name
  subject_alternative_names = [
    "${var.acme_config.cluster_name}.${var.acme_config.base_domain}"
  ]

  dns_challenge {
    provider = "azure"

    config = {
      AZURE_RESOURCE_GROUP  = var.acme_config.dns_resource_group_name
      AZURE_SUBSCRIPTION_ID = var.client_config.subscription_id
      AZURE_TENANT_ID       = var.client_config.tenant_id
      AZURE_CLIENT_ID       = azuread_application.acme_dns.client_id
      AZURE_CLIENT_SECRET   = azuread_service_principal_password.acme_dns.value
    }
  }

  depends_on = [acme_registration.main, time_sleep.acme_role_propagation]
}

# ============================================================================
# Key Vault Secrets for ACME Certificates
# ============================================================================
# Why: Stores the generated Let's Encrypt certificates in Key Vault.
#      ClickHouse nodes download these secrets during bootstrap.
# Without it: Generated certificates are lost or not accessible to VMs.
# ============================================================================
resource "azurerm_key_vault_secret" "certificate" {
  name  = "clickhouse-tls-certificate"
  value = "${acme_certificate.certificate.certificate_pem}${acme_certificate.certificate.issuer_pem}"

  key_vault_id = var.key_vault_id
  content_type = "application/x-pem-file"

  # Set expiration based on certificate validity
  expiration_date = "${replace(acme_certificate.certificate.certificate_not_after, "Z", "")}Z"
  tags            = var.tags
}

resource "azurerm_key_vault_secret" "private_key" {
  name         = "clickhouse-tls-private-key"
  value        = acme_certificate.certificate.private_key_pem
  key_vault_id = var.key_vault_id
  content_type = "application/x-pem-file"

  # Set expiration based on certificate validity
  expiration_date = "${replace(acme_certificate.certificate.certificate_not_after, "Z", "")}Z"
  tags            = var.tags
}

resource "azurerm_key_vault_secret" "ca_certificate" {
  name         = "clickhouse-tls-ca-certificate"
  value        = acme_certificate.certificate.issuer_pem
  key_vault_id = var.key_vault_id
  content_type = "application/x-pem-file"

  # Set expiration based on certificate validity
  expiration_date = "${replace(acme_certificate.certificate.certificate_not_after, "Z", "")}Z"
  tags            = var.tags
}
