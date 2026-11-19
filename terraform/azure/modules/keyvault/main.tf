# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Local: Client Config (passed from root module)
# ============================================================================
# Why: Uses variables passed from root module instead of data sources to avoid
#      "(known after apply)" issues that cause resource replacement on every plan.
#      Data sources are evaluated fresh on each plan, making their values
#      "known after apply" which forces replacement of role assignments.
# Without it: Role assignments and other resources would be replaced on every apply.
# ============================================================================
locals {
  tenant_id       = var.client_config.tenant_id
  object_id       = var.client_config.object_id
  subscription_id = var.client_config.subscription_id
}

# ============================================================================
# Azure Key Vault (Native Resource)
# ============================================================================
# Why: Provides secure storage for ClickHouse passwords and other sensitive
#      configuration. Enables centralized password management, rotation, and
#      audit logging. Passwords are never stored in plain text on VMs.
#      Uses native azurerm resource for full control and to avoid deprecation
#      warnings from external modules.
# Without it: Passwords must be stored in plain text on VMs, creating security
#             risks and making password rotation difficult.
# ============================================================================
resource "azurerm_key_vault" "main" {
  name                = var.names.key_vault
  location            = var.location
  resource_group_name = var.resource_group_name
  tenant_id           = local.tenant_id
  tags                = var.tags

  # Key Vault configuration
  # SECURITY: Purge protection is enforced (not optional) per Azure best practices
  # Soft delete retention increased to 90 days per Microsoft recommendation
  sku_name                        = "standard"
  soft_delete_retention_days      = var.passwords_config.soft_delete_retention_days
  purge_protection_enabled        = true # Enforced per Azure Security Baseline (Feb 2025)
  enabled_for_deployment          = false
  enabled_for_template_deployment = false
  enabled_for_disk_encryption     = false

  # Use RBAC for authorization (modern approach, more secure than access policies)
  # Note: Using rbac_authorization_enabled instead of deprecated enable_rbac_authorization
  rbac_authorization_enabled = true

  # SECURITY: Explicit public network access control
  # When true with network_acls.default_action = "Deny", access is restricted to:
  # - Terraform client IP (auto-fetched, for managing secrets)
  # - Subnets in vnet_subnet_ids list (VNet service endpoints for VMs)
  # - Private endpoints (recommended for production)
  # - Azure services (via bypass = "AzureServices")
  public_network_access_enabled = true

  # Network ACLs - allow Terraform client IP + VNet access
  network_acls {
    default_action             = "Deny"
    bypass                     = "AzureServices"
    ip_rules                   = var.access_config.allowed_ip_ranges
    virtual_network_subnet_ids = var.access_config.vnet_subnet_ids
  }


  # Prevent errors from attempting to modify immutable settings
  lifecycle {
    ignore_changes = [
      soft_delete_retention_days, # Cannot be modified once set
      purge_protection_enabled,   # Cannot be disabled once enabled
    ]
  }
}

# ============================================================================
# Role Assignment: Terraform User -> Key Vault Administrator
# ============================================================================
# Why: Grants the current Terraform user permission to manage Key Vault secrets.
#      Using "Key Vault Administrator" instead of "Key Vault Secrets Officer" because:
#      - Secrets Officer doesn't include the "purge" permission
#      - Purge permission is required during terraform destroy to permanently delete
#        soft-deleted secrets when purge_protection is disabled
# Without it: Terraform cannot create or manage secrets in Key Vault.
# ============================================================================
resource "azurerm_role_assignment" "terraform_key_vault_admin" {
  scope                = azurerm_key_vault.main.id
  role_definition_name = "Key Vault Administrator"
  principal_id         = local.object_id
}

# ============================================================================
# Role Assignment: Managed Identity -> Key Vault Secrets User
# ============================================================================
# Why: Grants the Managed Identity permission to read secrets from Key Vault.
#      This is done outside the AVM module to avoid for_each issues with dynamic
#      keys (the principal_id is only known after the storage module creates
#      the managed identity).
# Without it: VMs cannot read secrets from Key Vault using managed identity.
# ============================================================================
resource "azurerm_role_assignment" "managed_identity_secrets_user" {
  count                = var.access_config.enable_managed_identity_access ? 1 : 0
  scope                = azurerm_key_vault.main.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = var.access_config.managed_identity_principal_id

  depends_on = [azurerm_key_vault.main]
}

# ============================================================================
# Time Sleep: Wait for RBAC Propagation
# ============================================================================
# Why: Azure RBAC role assignments take time to propagate (typically 30 seconds
#      to several minutes). Without this delay, Terraform may try to create
#      secrets before the "Key Vault Administrator" role assignment has
#      propagated, resulting in 403 Forbidden errors on subsequent applies.
# Without it: Repeated `terraform apply` commands may fail with 403 errors
#             because Terraform tries to read/write secrets before RBAC
#             permissions are active.
# ============================================================================
resource "time_sleep" "wait_for_rbac_propagation" {
  depends_on = [azurerm_key_vault.main, azurerm_role_assignment.terraform_key_vault_admin]

  create_duration = "30s"
}

# ============================================================================
# ClickHouse Default User Password Secret
# ============================================================================
# Why: Stores the default ClickHouse user password securely in Key Vault.
#      This password is used for read-only access to ClickHouse.
# Without it: Password must be generated and stored in plain text on VMs.
# ============================================================================
resource "azurerm_key_vault_secret" "clickhouse_default_password" {
  name            = "clickhouse-default-password"
  value           = var.passwords_config.default_password != null ? var.passwords_config.default_password : random_password.default_password.result
  key_vault_id    = azurerm_key_vault.main.id
  content_type    = "password"
  expiration_date = timeadd(timestamp(), "8760h") # 1 year from now
  tags            = var.tags

  # Wait for RBAC propagation before creating secrets
  depends_on = [time_sleep.wait_for_rbac_propagation]

  lifecycle {
    ignore_changes = [expiration_date]
  }
}

# ============================================================================
# ClickHouse Admin User Password Secret
# ============================================================================
# Why: Stores the admin ClickHouse user password securely in Key Vault.
#      This password is used for administrative access to ClickHouse.
# Without it: Password must be generated and stored in plain text on VMs.
# ============================================================================
resource "azurerm_key_vault_secret" "clickhouse_admin_password" {
  name            = "clickhouse-admin-password"
  value           = var.passwords_config.admin_password != null ? var.passwords_config.admin_password : random_password.admin_password.result
  key_vault_id    = azurerm_key_vault.main.id
  content_type    = "password"
  expiration_date = timeadd(timestamp(), "8760h") # 1 year from now
  tags            = var.tags

  # Wait for RBAC propagation before creating secrets
  depends_on = [time_sleep.wait_for_rbac_propagation]

  lifecycle {
    ignore_changes = [expiration_date]
  }
}

# ============================================================================
# ClickHouse Writer User Password Secret
# ============================================================================
# Why: Stores the writer ClickHouse user password securely in Key Vault.
#      This password is used for write operations to ClickHouse.
# Without it: Password must be generated and stored in plain text on VMs.
# ============================================================================
resource "azurerm_key_vault_secret" "clickhouse_writer_password" {
  name            = "clickhouse-writer-password"
  value           = var.passwords_config.writer_password != null ? var.passwords_config.writer_password : random_password.writer_password.result
  key_vault_id    = azurerm_key_vault.main.id
  content_type    = "password"
  expiration_date = timeadd(timestamp(), "8760h") # 1 year from now
  tags            = var.tags

  # Wait for RBAC propagation before creating secrets
  depends_on = [time_sleep.wait_for_rbac_propagation]

  lifecycle {
    ignore_changes = [expiration_date]
  }
}

# ============================================================================
# Random Password: Default User
# ============================================================================
# Why: Generates a secure random password for the default ClickHouse user
#      if not provided. Ensures strong passwords by default.
# Without it: Must manually provide passwords, which may be weak or reused.
# ============================================================================
resource "random_password" "default_password" {
  length  = 32
  special = true
  upper   = true
  lower   = true
  numeric = true

  # Only use safe special characters that won't cause issues in XML/config files,
  # connection strings, or shell escaping
  override_special = "#@^"
}

# ============================================================================
# Random Password: Admin User
# ============================================================================
# Why: Generates a secure random password for the admin ClickHouse user
#      if not provided. Ensures strong passwords by default.
# Without it: Must manually provide passwords, which may be weak or reused.
# ============================================================================
resource "random_password" "admin_password" {
  length  = 32
  special = true
  upper   = true
  lower   = true
  numeric = true

  # Only use safe special characters that won't cause issues in XML/config files,
  # connection strings, or shell escaping
  override_special = "#@^"
}

# ============================================================================
# Random Password: Writer User
# ============================================================================
# Why: Generates a secure random password for the writer ClickHouse user
#      if not provided. Ensures strong passwords by default.
# Without it: Must manually provide passwords, which may be weak or reused.
# ============================================================================
resource "random_password" "writer_password" {
  length  = 32
  special = true
  upper   = true
  lower   = true
  numeric = true

  # Only use safe special characters that won't cause issues in XML/config files,
  # connection strings, or shell escaping
  override_special = "#@^"
}











# ============================================================================
# Key Vault Diagnostic Settings (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per Azure Security Baseline and CIS Benchmark, Key Vault must have
#      diagnostic settings enabled to audit all secret access and operations.
#      This is required for compliance (PCI-DSS, HIPAA, SOC2) and security
#      incident investigation.
# Reference: Azure Policy "Resource logs in Key Vault should be enabled"
# Without it: Cannot audit secret access, detect unauthorized access attempts,
#             or investigate security incidents. Non-compliant with security
#             frameworks.
# ============================================================================
resource "azurerm_monitor_diagnostic_setting" "keyvault" {
  count                      = var.log_analytics_workspace_id != null ? 1 : 0
  name                       = "diag-${var.names.key_vault}"
  target_resource_id         = azurerm_key_vault.main.id
  log_analytics_workspace_id = var.log_analytics_workspace_id

  # Audit events - captures all secret/key/certificate operations
  enabled_log {
    category = "AuditEvent"
  }

  # Metrics for monitoring Key Vault performance
  enabled_metric {
    category = "AllMetrics"
  }
}

# ============================================================================
# Event Grid System Topic for Key Vault
# ============================================================================
# Why: Enables Event Grid integration with Key Vault to detect secret changes.
#      Required for automatic password synchronization to VMs.
# Without it: Cannot automatically detect when passwords are updated in Key Vault.
# ============================================================================
resource "azurerm_eventgrid_system_topic" "keyvault" {
  count               = var.auto_rotate_config.enabled ? 1 : 0
  name                = var.names.event_grid_topic
  location            = var.location
  resource_group_name = var.resource_group_name
  source_resource_id  = azurerm_key_vault.main.id
  topic_type          = "Microsoft.KeyVault.vaults"
  tags                = var.tags

  depends_on = [azurerm_key_vault.main]
}

# ============================================================================
# Azure Automation Account
# ============================================================================
# Why: Provides automation capabilities to run PowerShell scripts that update
#      VM passwords when Key Vault secrets change. Uses system-assigned managed
#      identity for authentication.
# Without it: Cannot automatically execute password update scripts on VMs.
# ============================================================================
resource "azurerm_automation_account" "password_sync" {
  count               = var.auto_rotate_config.enabled ? 1 : 0
  name                = var.names.automation_account
  location            = var.location
  resource_group_name = var.resource_group_name
  sku_name            = "Basic"
  tags                = var.tags

  identity {
    type = "SystemAssigned"
  }

  depends_on = [azurerm_key_vault.main]
}

# ============================================================================
# Key Vault Access Policy for Automation Account
# ============================================================================
# Why: Grants the Automation Account's managed identity permission to read
#      secrets from Key Vault. Required for the runbook to retrieve passwords.
# Without it: Runbook cannot access Key Vault secrets to update VMs.
# ============================================================================
# Use role assignment instead of access policy (more secure)
resource "azurerm_role_assignment" "automation_key_vault_secrets_user" {
  count                = var.auto_rotate_config.enabled ? 1 : 0
  scope                = azurerm_key_vault.main.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_automation_account.password_sync[0].identity[0].principal_id

  depends_on = [
    azurerm_key_vault.main,
    azurerm_automation_account.password_sync
  ]
}

# ============================================================================
# Automation Runbook: Update ClickHouse Passwords on VMs
# ============================================================================
# Why: PowerShell runbook that updates ClickHouse user passwords on all VMs
#      when Key Vault secrets change. Retrieves new password from Key Vault
#      and updates ClickHouse configuration on each VM.
# Without it: VMs cannot automatically receive password updates when Key Vault
#             secrets are changed.
# ============================================================================
resource "azurerm_automation_runbook" "update_passwords" {
  count                   = var.auto_rotate_config.enabled ? 1 : 0
  name                    = "UpdateClickHousePasswords"
  location                = var.location
  resource_group_name     = var.resource_group_name
  automation_account_name = azurerm_automation_account.password_sync[0].name
  runbook_type            = "PowerShell"
  log_verbose             = true
  log_progress            = true
  tags                    = var.tags

  content = <<-CONTENT
<#
.SYNOPSIS
    Updates ClickHouse user passwords on all VMs when Key Vault secrets change.

.DESCRIPTION
    This runbook is triggered by Event Grid when a Key Vault secret is updated.
    It retrieves the new password from Key Vault and updates ClickHouse user
    configuration on all ClickHouse VMs (writers and readers).

.PARAMETER EventData
    Event Grid event data containing information about the secret change.
#>
param(
    [Parameter(Mandatory=$false)]
    [object]$EventData
)

$ErrorActionPreference = "Stop"

# Handle webhook-triggered events (Event Grid sends array of events in request body)
# When called via webhook, Event Grid sends: [ { "id": "...", "data": {...}, "topic": "..." } ]
if ($EventData) {
    # If EventData is an array (webhook format), take first event
    if ($EventData -is [array] -and $EventData.Count -gt 0) {
        $event = $EventData[0]
    } elseif ($EventData -is [PSCustomObject] -or $EventData -is [Hashtable]) {
        $event = $EventData
    } else {
        # Try to parse as JSON string
        try {
            $parsed = $EventData | ConvertFrom-Json
            if ($parsed -is [array] -and $parsed.Count -gt 0) {
                $event = $parsed[0]
            } else {
                $event = $parsed
            }
        } catch {
            Write-Error "Unable to parse event data: $_"
            exit 1
        }
    }
} else {
    Write-Error "No event data provided"
    exit 1
}

# Extract secret name from event data (Event Grid format)
if ($event.data -and $event.data.objectName) {
    $secretName = $event.data.objectName
    # Extract Key Vault name from topic (format: /subscriptions/.../resourceGroups/.../providers/Microsoft.KeyVault/vaults/VAULT_NAME)
    $topicParts = $event.topic -split '/'
    $keyVaultName = $topicParts[-1]
} elseif ($event.ObjectName) {
    $secretName = $event.ObjectName
    $keyVaultName = $event.VaultName
} else {
    Write-Error "Unable to extract secret information from event data. Event structure: $($event | ConvertTo-Json -Depth 5)"
    exit 1
}

$resourceGroupName = "${var.auto_rotate_config.vm_resource_group_name != null ? var.auto_rotate_config.vm_resource_group_name : var.resource_group_name}"

Write-Output "Processing password update for secret: $secretName in Key Vault: $keyVaultName"

# Map secret names to ClickHouse user types
$userTypeMap = @{
    "clickhouse-default-password" = "default"
    "clickhouse-admin-password"   = "admin"
    "clickhouse-writer-password"  = "writer"
}

$userType = $userTypeMap[$secretName]
if (-not $userType) {
    Write-Output "Secret $secretName is not a ClickHouse password secret. Skipping."
    exit 0
}

Write-Output "Updating $userType user password on all ClickHouse VMs..."

# Authenticate using managed identity
Connect-AzAccount -Identity | Out-Null

# Retrieve new password from Key Vault
try {
    $secret = Get-AzKeyVaultSecret -VaultName $keyVaultName -Name $secretName
    $newPassword = $secret.SecretValue | ConvertFrom-SecureString -AsPlainText
    Write-Output "Retrieved new password from Key Vault"
} catch {
    Write-Error "Failed to retrieve password from Key Vault: $_"
    throw
}

# Find all ClickHouse VMs (writers and readers)
$vms = Get-AzVM -ResourceGroupName $resourceGroupName | Where-Object {
    $_.Name -like "*writer*" -or $_.Name -like "*reader*"
}

if ($vms.Count -eq 0) {
    Write-Output "No ClickHouse VMs found in resource group: $resourceGroupName"
    exit 0
}

Write-Output "Found $($vms.Count) ClickHouse VM(s) to update"

# Update password on each VM
foreach ($vm in $vms) {
    Write-Output "Updating password on VM: $($vm.Name)"
    
    # Create script to update ClickHouse user password using SHA256 hash (avoids XML escaping issues)
    $updateScript = @"
#!/bin/bash
set -e

USER_TYPE="$userType"
NEW_PASSWORD='$newPassword'

# Generate SHA256 hash of the password (avoids XML special character issues)
NEW_PASSWORD_HASH=`$(echo -n "`$NEW_PASSWORD" | sha256sum | cut -d' ' -f1)

# Update ClickHouse user configuration using password_sha256_hex
if [ "`$USER_TYPE" = "default" ]; then
    sed -i "s|<password_sha256_hex>.*</password_sha256_hex>|<password_sha256_hex>$${NEW_PASSWORD_HASH}</password_sha256_hex>|g" /etc/clickhouse-server/users.d/users.xml
    # Also handle legacy plain password tags if they exist
    sed -i "s|<password>.*</password>|<password_sha256_hex>$${NEW_PASSWORD_HASH}</password_sha256_hex>|g" /etc/clickhouse-server/users.d/users.xml
elif [ "`$USER_TYPE" = "admin" ]; then
    sed -i "/<admin>/,/<\/admin>/s|<password_sha256_hex>.*</password_sha256_hex>|<password_sha256_hex>$${NEW_PASSWORD_HASH}</password_sha256_hex>|g" /etc/clickhouse-server/users.d/users.xml
    # Also handle legacy plain password tags if they exist
    sed -i "/<admin>/,/<\/admin>/s|<password>.*</password>|<password_sha256_hex>$${NEW_PASSWORD_HASH}</password_sha256_hex>|g" /etc/clickhouse-server/users.d/users.xml
elif [ "`$USER_TYPE" = "writer" ]; then
    sed -i "/<writer>/,/<\/writer>/s|<password_sha256_hex>.*</password_sha256_hex>|<password_sha256_hex>$${NEW_PASSWORD_HASH}</password_sha256_hex>|g" /etc/clickhouse-server/users.d/users.xml
    # Also handle legacy plain password tags if they exist
    sed -i "/<writer>/,/<\/writer>/s|<password>.*</password>|<password_sha256_hex>$${NEW_PASSWORD_HASH}</password_sha256_hex>|g" /etc/clickhouse-server/users.d/users.xml
fi

# Validate XML syntax
xmllint --noout /etc/clickhouse-server/users.d/users.xml || {
    echo "ERROR: XML validation failed after password update"
    exit 1
}

# Reload ClickHouse configuration
clickhouse-client --query "SYSTEM RELOAD CONFIG" || true

echo "Password updated successfully for user: `$USER_TYPE"
"@

    try {
        # Execute script on VM using Run Command
        $result = Invoke-AzVMRunCommand `
            -ResourceGroupName $resourceGroupName `
            -VMName $vm.Name `
            -CommandId "RunShellScript" `
            -ScriptString $updateScript

        if ($result.Value[0].Code -eq "Success") {
            Write-Output "✓ Successfully updated password on VM: $($vm.Name)"
        } else {
            Write-Warning "⚠ Password update on VM $($vm.Name) completed with warnings: $($result.Value[0].Message)"
        }
    } catch {
        Write-Error "✗ Failed to update password on VM $($vm.Name): $_"
        # Continue with other VMs even if one fails
    }
}

Write-Output "Password synchronization completed for secret: $secretName"
CONTENT

  depends_on = [azurerm_automation_account.password_sync]
}

# ============================================================================
# Automation Webhook for Runbook
# ============================================================================
# Why: Creates a webhook URL that can be called by Event Grid to trigger
#      the password update runbook. Enables event-driven password synchronization.
# Without it: Cannot automatically trigger runbook when Key Vault secrets change.
# ============================================================================
resource "azurerm_automation_webhook" "update_passwords" {
  count                   = var.auto_rotate_config.enabled ? 1 : 0
  name                    = "UpdatePasswordsWebhook"
  resource_group_name     = var.resource_group_name
  automation_account_name = azurerm_automation_account.password_sync[0].name
  # SECURITY FIX: Reduced from 1 year to configurable short duration (default 90 days)
  # Per Azure security best practices, webhooks should have shorter expiry times
  # and be rotated regularly to reduce security exposure
  expiry_time  = timeadd(timestamp(), "${var.auto_rotate_config.webhook_expiry_hours}h")
  enabled      = true
  runbook_name = azurerm_automation_runbook.update_passwords[0].name

  # Event Grid will send event data directly to the webhook
  # The runbook will parse the event data from the webhook payload
  parameters = {}

  depends_on = [azurerm_automation_runbook.update_passwords]

  lifecycle {
    ignore_changes = [expiry_time] # Ignore expiry_time changes on subsequent applies for idempotency
  }
}

# ============================================================================
# Custom Role Definition: VM Run Command Only
# ============================================================================
# Why: Per principle of least privilege, the Automation Account should only have
#      permission to execute run commands on VMs, not full VM Contributor access.
#      This limits the blast radius if the automation account is compromised.
# Without it: Automation account has broad VM permissions including delete, stop,
#             and restart which are not needed for password sync operations.
# ============================================================================
resource "azurerm_role_definition" "vm_run_command" {
  count       = var.auto_rotate_config.enabled ? 1 : 0
  name        = "${var.cluster_name}-vm-run-command"
  scope       = "/subscriptions/${local.subscription_id}"
  description = "Custom role for executing run commands on VMs (password sync)"

  permissions {
    actions = [
      "Microsoft.Compute/virtualMachines/runCommand/action",
      "Microsoft.Compute/virtualMachines/read",
      "Microsoft.Compute/virtualMachineScaleSets/virtualMachines/runCommand/action",
      "Microsoft.Compute/virtualMachineScaleSets/virtualMachines/read",
      "Microsoft.Compute/virtualMachineScaleSets/read"
    ]
    not_actions = []
  }

  assignable_scopes = [
    "/subscriptions/${local.subscription_id}/resourceGroups/${var.auto_rotate_config.vm_resource_group_name != null ? var.auto_rotate_config.vm_resource_group_name : var.resource_group_name}"
  ]
}

# ============================================================================
# Role Assignment: Automation Account -> VM Run Command (Least Privilege)
# ============================================================================
# Why: Grants the Automation Account's managed identity permission to execute
#      run commands on VMs only. This follows the principle of least privilege.
# SECURITY FIX: Changed from "Virtual Machine Contributor" (overly broad) to
#               custom role with only run command permissions.
# Without it: Runbook cannot execute scripts on VMs to update ClickHouse passwords.
# ============================================================================
resource "azurerm_role_assignment" "automation_vm_run_command" {
  count              = var.auto_rotate_config.enabled ? 1 : 0
  scope              = "/subscriptions/${local.subscription_id}/resourceGroups/${var.auto_rotate_config.vm_resource_group_name != null ? var.auto_rotate_config.vm_resource_group_name : var.resource_group_name}"
  role_definition_id = azurerm_role_definition.vm_run_command[0].role_definition_resource_id
  principal_id       = azurerm_automation_account.password_sync[0].identity[0].principal_id

  depends_on = [
    azurerm_automation_account.password_sync,
    azurerm_role_definition.vm_run_command
  ]
}

# ============================================================================
# Event Grid Subscription: Key Vault Secret Updated
# ============================================================================
# Why: Subscribes to Key Vault secret update events via Event Grid system topic
#      and triggers the Automation runbook via webhook. This enables automatic
#      password synchronization when secrets are changed in Key Vault.
# Without it: Password changes in Key Vault do not automatically propagate to VMs.
# ============================================================================
resource "azurerm_eventgrid_system_topic_event_subscription" "keyvault_secret_updated" {
  count                 = var.auto_rotate_config.enabled ? 1 : 0
  name                  = var.names.event_grid_subscription
  system_topic          = azurerm_eventgrid_system_topic.keyvault[0].name
  resource_group_name   = var.resource_group_name
  event_delivery_schema = "EventGridSchema"

  # Filter for secret update events only
  included_event_types = [
    "Microsoft.KeyVault.SecretNewVersionCreated"
  ]

  # Advanced filter: Only trigger for ClickHouse password secrets
  advanced_filter {
    string_in {
      key = "data.objectName"
      values = [
        "clickhouse-default-password",
        "clickhouse-admin-password",
        "clickhouse-writer-password"
      ]
    }
  }

  # Trigger Automation runbook via webhook
  webhook_endpoint {
    url = azurerm_automation_webhook.update_passwords[0].uri
  }

  retry_policy {
    event_time_to_live    = 1440 # 24 hours
    max_delivery_attempts = 30
  }

  depends_on = [
    azurerm_eventgrid_system_topic.keyvault,
    azurerm_automation_webhook.update_passwords
  ]
}
