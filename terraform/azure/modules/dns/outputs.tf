# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# DNS Zone Outputs
# ============================================================================

output "dns_zone_name" {
  description = "Name of the private DNS zone"
  value       = azurerm_private_dns_zone.clickhouse.name
}

output "dns_zone_id" {
  description = "ID of the private DNS zone"
  value       = azurerm_private_dns_zone.clickhouse.id
}
