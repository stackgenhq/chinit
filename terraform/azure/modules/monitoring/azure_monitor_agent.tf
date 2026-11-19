# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Azure Monitor Agent Extension for Regular VMs (Writers)
# ============================================================================
# Why: Installs Azure Monitor Agent extension on each writer VM, enabling
#      the agent to collect logs and metrics. The extension provides the
#      Azure Monitor Agent service that sends data to Log Analytics Workspace
#      based on Data Collection Rule associations.
# Without it: Azure Monitor Agent is not installed on VMs. Even with DCR
#             associations, no agent exists to collect and send logs/metrics.
#             Logs remain only on individual VMs, making centralized
#             monitoring impossible.
# Automatic Application: Uses count to iterate over all VM resource IDs.
#                        When new writer VMs are added via Terraform (by
#                        increasing writer_count), they automatically get this
#                        extension installed. The DCR association below also
#                        uses count, ensuring new VMs are fully configured.
# ============================================================================
resource "azurerm_virtual_machine_extension" "azure_monitor_agent_vms" {
  count                      = length(var.vm_resource_ids)
  name                       = "AzureMonitorLinuxAgent"
  virtual_machine_id         = var.vm_resource_ids[count.index]
  publisher                  = "Microsoft.Azure.Monitor"
  type                       = "AzureMonitorLinuxAgent"
  type_handler_version       = "1.0"
  auto_upgrade_minor_version = true

  # With Data Collection Rules, we only need the workspace ID
  # The DCR association will configure what data to collect
  settings = jsonencode({
    workspaceId = module.log_analytics_workspace.resource.workspace_id
  })

  # Timeouts to prevent hanging during destroy operations
  # Why: Azure VM extensions can get stuck during destroy if the VM is in
  #      a transitional state. Timeouts ensure Terraform fails fast rather
  #      than hanging indefinitely.
  # Without it: Destroy operations can hang indefinitely, blocking
  #             infrastructure teardown and requiring manual intervention.
  timeouts {
    create = "30m"
    update = "30m"
    delete = "20m"
  }
}

# ============================================================================
# Azure Monitor Agent Extension for VMSS (Readers and Keepers)
# ============================================================================
# Why: Installs Azure Monitor Agent extension on each VMSS (readers and
#      keepers), enabling the agent to collect logs and metrics from all
#      instances in the scale set. The extension provides the Azure Monitor
#      Agent service that sends data to Log Analytics Workspace based on
#      Data Collection Rule associations.
# Without it: Azure Monitor Agent is not installed on VMSS instances. Even
#             with DCR associations, no agent exists to collect and send
#             logs/metrics from reader and keeper nodes. Logs remain only
#             on individual VM instances, making centralized monitoring
#             impossible for these nodes.
# Automatic Application: This extension becomes part of the VMSS template.
#                        When VMSS scales out (via autoscaling or manual scaling),
#                        all new instances automatically get this extension installed
#                        and configured. No manual intervention required.
# NOTE: When skip_vmss_extension_install is true (default), this resource is
#       not created because the Azure Monitor Agent is installed as an inline
#       extension in the VMSS module. This avoids rolling upgrade failures on
#       fresh deployments where instances are still initializing.
# ============================================================================
resource "azurerm_virtual_machine_scale_set_extension" "azure_monitor_agent_vmss" {
  count                        = var.skip_vmss_extension_install ? 0 : length(var.vmss_resource_ids)
  name                         = "AzureMonitorLinuxAgent"
  virtual_machine_scale_set_id = var.vmss_resource_ids[count.index]
  publisher                    = "Microsoft.Azure.Monitor"
  type                         = "AzureMonitorLinuxAgent"
  type_handler_version         = "1.0"
  auto_upgrade_minor_version   = true

  # With Data Collection Rules, we only need the workspace ID
  # The DCR association will configure what data to collect
  settings = jsonencode({
    workspaceId = module.log_analytics_workspace.resource.workspace_id
  })
}

# ============================================================================
# Data Collection Rule Associations for Regular VMs (Writers)
# ============================================================================
# Why: Associates each writer VM with the Data Collection Rule, enabling
#      Azure Monitor Agent to collect logs and metrics from the VMs and send
#      them to Log Analytics Workspace. Without these associations, the
#      Data Collection Rule exists but VMs are not configured to send data.
# Without it: VMs are not associated with the Data Collection Rule. Azure
#             Monitor Agent cannot send logs and metrics to Log Analytics,
#             making centralized monitoring and log analysis impossible.
# Automatic Application: Uses count to iterate over all VM resource IDs.
#                        When new writer VMs are added via Terraform (by
#                        increasing writer_count), they automatically get this
#                        DCR association created, ensuring logs flow immediately.
# ============================================================================
resource "azurerm_monitor_data_collection_rule_association" "vm_writers" {
  count                   = length(var.vm_resource_ids)
  name                    = "dcr-assoc-${split("/", var.vm_resource_ids[count.index])[length(split("/", var.vm_resource_ids[count.index])) - 1]}"
  target_resource_id      = var.vm_resource_ids[count.index]
  data_collection_rule_id = azurerm_monitor_data_collection_rule.clickhouse.id
  description             = "Associates writer VM with ClickHouse Data Collection Rule"

  depends_on = [
    azurerm_virtual_machine_extension.azure_monitor_agent_vms
  ]
}

# ============================================================================
# Data Collection Rule Associations for VMSS (Readers and Keepers)
# ============================================================================
# Why: Associates each VMSS (readers and keepers) with the Data Collection
#      Rule, enabling Azure Monitor Agent to collect logs and metrics from
#      all instances in the scale set and send them to Log Analytics
#      Workspace. Without these associations, VMSS instances are not
#      configured to send data.
# Without it: VMSS instances are not associated with the Data Collection
#             Rule. Azure Monitor Agent cannot send logs and metrics from
#             reader and keeper nodes to Log Analytics, making centralized
#             monitoring and log analysis impossible for these nodes.
# Automatic Application: DCR associations at the VMSS level apply to all
#                        instances in the scale set, including new instances
#                        that scale out via autoscaling. When VMSS scales out,
#                        new instances automatically inherit the DCR association
#                        and start sending logs immediately after the Azure Monitor
#                        Agent extension installs (which happens automatically
#                        as part of the VMSS template or inline extension).
# ============================================================================
resource "azurerm_monitor_data_collection_rule_association" "vmss" {
  count                   = length(var.vmss_resource_ids)
  name                    = "dcr-assoc-${split("/", var.vmss_resource_ids[count.index])[length(split("/", var.vmss_resource_ids[count.index])) - 1]}"
  target_resource_id      = var.vmss_resource_ids[count.index]
  data_collection_rule_id = azurerm_monitor_data_collection_rule.clickhouse.id
  description             = "Associates VMSS with ClickHouse Data Collection Rule"

  # Only depend on the extension resource when it's being created
  # When skip_vmss_extension_install is true, the agent is installed inline in VMSS
  depends_on = [
    azurerm_virtual_machine_scale_set_extension.azure_monitor_agent_vmss
  ]

  # RELIABILITY: Ensure DCR associations are recreated before being destroyed.
  # This prevents the scenario where VMSS is replaced (e.g., golden image migration)
  # but DCR associations are destroyed and not recreated, causing log collection
  # to silently stop working.
  lifecycle {
    create_before_destroy = true
  }
}
