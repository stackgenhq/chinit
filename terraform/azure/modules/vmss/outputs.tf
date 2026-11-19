# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# VMSS Module Outputs
# ============================================================================
# These outputs provide important information about the deployed VMSS
# infrastructure that may be needed by other modules or scripts.
# ============================================================================

output "cluster_name" {
  description = "Name of the ClickHouse cluster"
  value       = var.cluster_name
}

output "vnet_id" {
  description = "ID of the virtual network"
  value       = azurerm_virtual_network.main.id
}

output "vnet_name" {
  description = "Name of the virtual network"
  value       = azurerm_virtual_network.main.name
}

output "subnet_id" {
  description = "ID of the subnet"
  value       = azurerm_subnet.clickhouse.id
}

# ============================================================================
# Writers Load Balancer Outputs
# ============================================================================
# Why: Provides the public IP address and ID of the writer load balancer
#      for applications performing write operations. This is the endpoint
#      for INSERT operations using ch-go library.
# Without it: Applications cannot programmatically determine the writer
#             endpoint, requiring manual lookups or hardcoded IPs.
# ============================================================================
output "writers_public_ip" {
  description = "Public IP address of the writer load balancer (for INSERT operations)"
  value       = azurerm_public_ip.writers_lb.ip_address
}

output "writers_load_balancer_id" {
  description = "ID of the writer load balancer"
  value       = azurerm_lb.writers.id
}

output "writers_backend_pool_id" {
  description = "ID of the writer load balancer backend address pool"
  value       = azurerm_lb_backend_address_pool.writers.id
}

# ============================================================================
# Readers Load Balancer Outputs
# ============================================================================
# Why: Provides the public IP address and ID of the reader load balancer
#      for applications performing read operations. This is the endpoint
#      for SELECT queries using ch-go library.
# Without it: Applications cannot programmatically determine the reader
#             endpoint, requiring manual lookups or hardcoded IPs.
# ============================================================================
output "readers_public_ip" {
  description = "Public IP address of the reader load balancer (for SELECT queries)"
  value       = azurerm_public_ip.readers_lb.ip_address
}

output "readers_load_balancer_id" {
  description = "ID of the reader load balancer"
  value       = azurerm_lb.readers.id
}

output "readers_backend_pool_id" {
  description = "ID of the reader load balancer backend address pool"
  value       = azurerm_lb_backend_address_pool.readers.id
}

output "writers_scale_set_id" {
  description = "ID of the writer VMSS"
  value       = azurerm_orchestrated_virtual_machine_scale_set.writers.id
}

output "writers_scale_set_name" {
  description = "Name of the writer VMSS"
  value       = azurerm_orchestrated_virtual_machine_scale_set.writers.name
}

# Note: Individual IPs are not available for VMSS without per-instance public IPs.
# Use load balancer IP (writers_public_ip) for external access.
# For SSH to specific instances, use NAT rules or Azure Bastion.

output "readers_scale_set_id" {
  description = "ID of the reader VMSS"
  value       = azurerm_orchestrated_virtual_machine_scale_set.readers.id
}

output "readers_scale_set_name" {
  description = "Name of the reader VMSS"
  value       = azurerm_orchestrated_virtual_machine_scale_set.readers.name
}

output "keeper_vm_ids" {
  description = "IDs of the keeper VMs"
  value       = azurerm_linux_virtual_machine.keepers[*].id
}

output "keeper_vm_names" {
  description = "Names of the keeper VMs"
  value       = azurerm_linux_virtual_machine.keepers[*].name
}

# Legacy outputs for compatibility - returning first VM ID or list if possible? 
# Better to break compatibility here since it's a major refactor.

output "virtual_network_id" {
  description = "ID of the virtual network (for DNS zone linking)"
  value       = azurerm_virtual_network.main.id
}

output "all_vm_resource_ids" {
  description = "List of all VMSS resource IDs (writers, readers) and Keeper VM IDs for monitoring (deprecated - use vmss_resource_ids instead)"
  value = concat(
    [azurerm_orchestrated_virtual_machine_scale_set.writers.id],
    [azurerm_orchestrated_virtual_machine_scale_set.readers.id],
    azurerm_linux_virtual_machine.keepers[*].id
  )
}

output "vmss_resource_ids" {
  description = "List of VMSS resource IDs (writers, readers) for monitoring"
  value = concat(
    [azurerm_orchestrated_virtual_machine_scale_set.writers.id],
    [azurerm_orchestrated_virtual_machine_scale_set.readers.id]
  )
}

# ============================================================================
# Backup Replica Outputs
# ============================================================================
output "backup_replica_vm_id" {
  description = "ID of the backup replica VM (null if not enabled)"
  value       = var.backup_replica_config.enabled ? azurerm_linux_virtual_machine.backup_replica[0].id : null
}

output "backup_replica_private_ip" {
  description = "Private IP address of the backup replica (null if not enabled)"
  value       = var.backup_replica_config.enabled ? azurerm_network_interface.backup_replica[0].ip_configuration[0].private_ip_address : null
}

# ============================================================================
# Network Security Group Outputs
# ============================================================================
output "nsg_id" {
  description = "ID of the Network Security Group"
  value       = azurerm_network_security_group.clickhouse.id
}

output "nsg_name" {
  description = "Name of the Network Security Group"
  value       = azurerm_network_security_group.clickhouse.name
}

# ============================================================================
# Application Security Group Outputs
# ============================================================================
output "clickhouse_nodes_asg_id" {
  description = "ID of the ClickHouse nodes Application Security Group"
  value       = var.enable_application_security_groups ? azurerm_application_security_group.clickhouse_nodes[0].id : null
}

output "keeper_nodes_asg_id" {
  description = "ID of the Keeper nodes Application Security Group"
  value       = var.enable_application_security_groups ? azurerm_application_security_group.keeper_nodes[0].id : null
}

# ============================================================================
# Private Endpoint Outputs
# ============================================================================
output "storage_private_endpoint_id" {
  description = "ID of the Storage Private Endpoint"
  value       = var.security_config.enable_private_endpoints && var.storage_account_id != null ? azurerm_private_endpoint.storage_blob[0].id : null
}

output "keyvault_private_endpoint_id" {
  description = "ID of the Key Vault Private Endpoint"
  value       = var.security_config.enable_private_endpoints && var.key_vault_id != null ? azurerm_private_endpoint.key_vault[0].id : null
}

output "storage_private_dns_zone_id" {
  description = "ID of the Storage Blob Private DNS Zone"
  value       = var.security_config.enable_private_endpoints && var.security_config.create_private_dns_zones ? azurerm_private_dns_zone.storage_blob[0].id : var.security_config.storage_private_dns_zone_id
}

output "keyvault_private_dns_zone_id" {
  description = "ID of the Key Vault Private DNS Zone"
  value       = var.security_config.enable_private_endpoints && var.security_config.create_private_dns_zones ? azurerm_private_dns_zone.keyvault[0].id : var.security_config.keyvault_private_dns_zone_id
}

# ============================================================================
# Azure Bastion Outputs
# ============================================================================
output "bastion_name" {
  description = "Name of the Azure Bastion host (null if not enabled)"
  value       = var.network_config.enable_bastion ? (var.network_config.bastion_sku == "Developer" ? azurerm_bastion_host.developer[0].name : azurerm_bastion_host.main[0].name) : null
}

output "bastion_id" {
  description = "ID of the Azure Bastion host (null if not enabled)"
  value       = var.network_config.enable_bastion ? (var.network_config.bastion_sku == "Developer" ? azurerm_bastion_host.developer[0].id : azurerm_bastion_host.main[0].id) : null
}

# ============================================================================
# SSH Key Outputs
# ============================================================================
output "ssh_public_key_openssh" {
  description = "The public SSH key in OpenSSH format (either provided or generated)"
  value       = local.ssh_public_key
  sensitive   = true
}

output "ssh_private_key_pem" {
  description = "The private SSH key in PEM format (null if key was provided, not generated)"
  value       = var.vm_ssh_public_key == null ? tls_private_key.vm_ssh[0].private_key_pem : null
  sensitive   = true
}

output "ssh_key_generated" {
  description = "Whether the SSH key was generated (true) or provided (false)"
  value       = var.vm_ssh_public_key == null
}
