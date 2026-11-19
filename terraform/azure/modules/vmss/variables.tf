# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# VMSS Module Variables
# ============================================================================
# This file defines all input variables for the VMSS module.
# ============================================================================

variable "resource_group_name" {
  description = "Name of the Azure Resource Group"
  type        = string
}

variable "resource_group_id" {
  description = "ID of the Azure Resource Group (to avoid data source lookups that cause replacement)"
  type        = string
}

variable "location" {
  description = "Azure region for resources"
  type        = string
}

variable "cluster_name" {
  description = "Name of the ClickHouse cluster"
  type        = string
}

variable "network_config" {
  description = "Network configuration including VNet, Subnet, NAT Gateway, DDoS, and Flow Logs"
  type = object({
    vnet_address_space                 = optional(string, "10.0.0.0/16")
    subnet_address_prefix              = optional(string, "10.0.1.0/24")
    enable_nat_gateway                 = optional(bool, true)
    nat_gateway_idle_timeout_minutes   = optional(number, 10)
    enable_ddos_protection             = optional(bool, false)
    enable_vnet_flow_logs              = optional(bool, false)
    flow_log_storage_account_id        = optional(string)
    flow_log_retention_days            = optional(number, 91)
    enable_traffic_analytics           = optional(bool, false)
    traffic_analytics_workspace_guid   = optional(string)
    traffic_analytics_interval_minutes = optional(number, 60)
    enable_bastion                     = optional(bool, false)
    bastion_subnet_address_prefix      = optional(string, "10.0.2.0/26")
    bastion_sku                        = optional(string, "Basic")
  })
  default = {}
}

variable "security_config" {
  description = "Security configuration including trusted launch, encryption, and private endpoints"
  type = object({
    enable_trusted_launch        = optional(bool, true)
    enable_encryption_at_host    = optional(bool, true)
    enable_private_endpoints     = optional(bool, true)
    create_private_dns_zones     = optional(bool, true)
    storage_private_dns_zone_id  = optional(string)
    keyvault_private_dns_zone_id = optional(string)
  })
  default = {}
}

variable "vm_admin_username" {
  description = "Admin username for VMs"
  type        = string
  default     = "clickhouseadmin"
}

variable "vm_ssh_public_key" {
  description = "SSH public key for VM access. If null, a new key pair will be generated and saved to ssh_key_output_path."
  type        = string
  sensitive   = true
  default     = null
}

variable "ssh_key_output_path" {
  description = "Path where generated SSH keys will be saved. Only used when vm_ssh_public_key is null."
  type        = string
  default     = null
}

variable "instance_config" {
  description = "Instance configuration including counts, sizes, and spot settings"
  type = object({
    writer_count                = number
    writer_vm_size              = string
    reader_count                = number
    reader_vm_size              = string
    keeper_count                = number
    keeper_vm_size              = string
    enable_reader_spot_vms      = optional(bool, true)
    reader_spot_eviction_policy = optional(string, "Deallocate")
    reader_spot_max_bid_price   = optional(number, -1)
    enable_ephemeral_os_disk    = optional(bool, true)
    metadata_disk_size_gb       = optional(number, 512)
    cache_disk_size_gb          = optional(number, 500)
    disk_storage_account_type   = optional(string, "Premium_LRS")
  })
}

variable "writer_cloud_init" {
  description = "Cloud-init script for writer nodes"
  type        = string
}

variable "reader_cloud_init" {
  description = "Cloud-init script for reader nodes"
  type        = string
}

variable "keeper_cloud_init" {
  description = "Cloud-init script for keeper nodes"
  type        = string
}

variable "config_files_hash" {
  description = "Hash of XML config files - used to trigger VM replacement when configs change"
  type        = string
  default     = ""
}

variable "keeper_config_files_hash" {
  description = "Hash of Keeper-specific config files to trigger VM replacement on change"
  type        = string
  default     = ""
}

variable "tags" {
  description = "Tags to apply to resources"
  type        = map(string)
  default     = {}
}


variable "allowed_source_ips" {
  description = "List of source IP addresses/CIDR blocks allowed to access ClickHouse (empty = VNet subnet only)"
  type        = list(string)
  default     = []
}


variable "allowed_ssh_ips" {
  description = "List of source IP addresses/CIDR blocks allowed SSH access (empty = VNet subnet only, never exposed to public internet)"
  type        = list(string)
  default     = []
}

variable "managed_identity_id" {
  description = "ID of the user-assigned managed identity for VMSS instances"
  type        = string
  default     = null
}

variable "autoscaling_config" {
  description = "Autoscaling configuration"
  type = object({
    enabled                    = optional(bool, false)
    min_instances              = optional(number, 2)
    max_instances              = optional(number, 10)
    cpu_scale_out_threshold    = optional(number, 70)
    cpu_scale_in_threshold     = optional(number, 30)
    memory_scale_out_threshold = optional(number, 80)
    memory_scale_in_threshold  = optional(number, 50)
  })
  default = {}
}

# ============================================================================
# Naming Variables (from Azure Naming Module)
# ============================================================================
# Why: Provides centralized naming convention from the Azure Naming module.
#      All resource names are generated using Azure CAF-compliant patterns
#      and passed from the root module to ensure consistency.
# Without it: Resource names must be manually constructed in each module,
#             leading to inconsistent naming and potential conflicts.
# ============================================================================
variable "names" {
  description = "Naming outputs from the Azure Naming module"
  type = object({
    virtual_network    = string
    subnet             = string
    nsg                = string
    ddos_plan          = string
    lb_writers         = string
    lb_readers         = string
    lb_writers_pip     = string
    lb_readers_pip     = string
    lb_backend_writers = string
    lb_backend_readers = string
    vmss_writers       = string
    vmss_readers       = string
    vmss_keepers       = string
  })
}

# Note: writer_names variable removed - writers are now VMSS, names generated from vmss_writers

# ============================================================================
# DDoS Protection Configuration
# ============================================================================
# Why: Azure DDoS Protection Standard provides advanced DDoS mitigation for
#      public IPs. However, it costs ~$2,944/month which may not be justified
#      for development/staging environments.
# Without it: Public IPs rely on Azure's basic DDoS protection, which provides
#             less comprehensive protection but is included at no extra cost.
# ============================================================================

# ============================================================================
# Storage Account Configuration
# ============================================================================
# Why: Storage account ID is needed to grant system-assigned Managed Identity
#      permissions to read cloud-init scripts from blob storage. This enables
#      VMs to download updated scripts without requiring storage account keys.
# Without it: VMs cannot access blob storage to download cloud-init scripts,
#             preventing script updates and requiring manual intervention.
# ============================================================================
variable "storage_account_id" {
  description = "ID of the storage account for blob storage access"
  type        = string
  default     = null
}

# ============================================================================
# Backup Replica Configuration
# ============================================================================
# Why: A dedicated backup replica isolates backup operations from production
#      query traffic. Uses Azure B-series burstable VMs for cost optimization.
# Without it: Backups run on production nodes, consuming CPU resources and
#             potentially impacting query performance during backup windows.
# ============================================================================
variable "backup_replica_config" {
  description = "Backup replica configuration"
  type = object({
    enabled               = optional(bool, false)
    vm_size               = optional(string, "Standard_B4ms")
    metadata_disk_size_gb = optional(number, 256)
    cache_disk_size_gb    = optional(number, 128)
    names = optional(object({
      vm         = string
      nic        = string
      os_disk    = string
      meta_disk  = string
      cache_disk = string
    }), null)
  })
}

variable "backup_replica_cloud_init" {
  description = "Cloud-init script for backup replica node"
  type        = string
  default     = ""
}

variable "key_vault_id" {
  description = "ID of the Key Vault for granting backup replica access to secrets"
  type        = string
  default     = null
}

# ============================================================================
# DNS Zone Configuration
# ============================================================================
# Why: DNS Zone ID is needed to grant system-assigned Managed Identity
#      permissions to register and update A records in the Private DNS Zone.
#      This enables VMs to register their hostnames (keeper-0, writer-0, etc.)
#      for proper cluster communication.
# Without it: VMs cannot register their DNS records, causing cluster formation
#             failures as nodes cannot resolve each other's hostnames.
# ============================================================================
variable "dns_zone_id" {
  description = "ID of the Private DNS Zone for granting VMs permission to register DNS records"
  type        = string
  default     = null
}

# ============================================================================
# Availability Zone Configuration
# ============================================================================
# Why: Per Azure best practices, VMSS should be deployed across availability
#      zones for high availability. This protects against datacenter-level
#      failures and provides rack/power isolation.
# Reference: BLINDSPOTS.md items #8 and #9 - No Availability Zone Distribution
# Without it: Single zone failure takes down entire cluster. No rack/power
#             isolation. Higher blast radius for Azure platform issues.
# ============================================================================
variable "enable_availability_zones" {
  description = "Enable availability zone distribution for VMSS. Recommended for production."
  type        = bool
  default     = true
}

variable "availability_zones" {
  description = "List of availability zones to distribute VMSS instances across"
  type        = list(string)
  default     = ["1", "2", "3"]
}

# ============================================================================
# Application Health Extension Configuration
# ============================================================================
# Why: Per Azure VMSS security baseline, enable application health monitoring
#      to track instance health and automatic repair policies for high-impact
#      resilience. Health probes verify ClickHouse is actually responding,
#      not just that the VM is running.
# Reference: BLINDSPOTS.md item #11 - No Application Health Extension
# Without it: Azure cannot detect unhealthy ClickHouse instances. Failed nodes
#             remain in the load balancer pool, causing connection failures.
#             No automatic recovery from application-level failures.
# ============================================================================
variable "enable_health_extension" {
  description = "Enable Application Health Extension for VMSS. Enables health monitoring and auto-repair."
  type        = bool
  default     = true
}



variable "enable_automatic_instance_repair" {
  description = "Enable automatic instance repair when health probe fails"
  type        = bool
  default     = true
}

variable "automatic_repair_grace_period" {
  description = "Grace period before automatic repair starts (ISO 8601 duration format, minimum PT10M)"
  type        = string
  default     = "PT10M"
}

# ============================================================================
# NAT Gateway Configuration (BLINDSPOTS.md #13)
# ============================================================================
# Why: Per Azure best practices, production workloads requiring outbound
#      connectivity should use NAT Gateway to prevent SNAT port exhaustion.
#      VMs in VMSS rely on Load Balancer SNAT for outbound, which has limited
#      ports per instance and can exhaust during heavy blob storage uploads.
# Reference: BLINDSPOTS.md item #13 - Outbound Connectivity (SNAT Exhaustion Risk)
# Without it: SNAT port exhaustion during heavy outbound traffic (blob uploads,
#             Key Vault calls, Azure Monitor). Initialization failures for
#             az login, apt update. Telemetry loss to Azure Monitor/OTel.
# Cost: ~$32/month for NAT Gateway + ~$0.045/GB data processed
# ============================================================================

# ============================================================================
# Rolling Upgrade Policy Configuration (BLINDSPOTS.md #3)
# ============================================================================
# Why: Per Azure best practices, configure rolling upgrades on VMSS for safe
#      updates. Rolling upgrades apply changes in batches, ensuring high
#      availability during OS upgrades and image updates.
# Reference: BLINDSPOTS.md item #3 - No Rolling Upgrade Policy
# Without it: All instances upgrade simultaneously, causing full cluster
#             downtime during OS upgrades. No automatic rollback on failure.
# ============================================================================
variable "rolling_upgrade_config" {
  description = "Rolling upgrade configuration"
  type = object({
    enabled                               = optional(bool, true)
    max_batch_percent                     = optional(number, 20)
    max_unhealthy_percent                 = optional(number, 20)
    max_unhealthy_upgraded_percent        = optional(number, 5)
    pause_time                            = optional(string, "PT1M")
    enable_max_surge                      = optional(bool, true)
    enable_cross_zone_upgrades            = optional(bool, true)
    enable_prioritize_unhealthy_instances = optional(bool, true)
  })
  default = {}
}



# ============================================================================
# Keeper Dedicated Disk Configuration (BLINDSPOTS.md #4)
# ============================================================================
# Why: ClickHouse Keeper nodes are extremely sensitive to disk latency (fsync).
#      Sharing a single disk for both transaction logs and snapshots causes
#      IO contention, leading to leader election timeouts and RAFT quorum loss.
# Reference: BLINDSPOTS.md item #4 - Keeper IOPS Contention
# Without it: Heavy write loads on logs delay snapshots, causing leader
#             election timeouts, RAFT quorum loss, and cluster instability.
#             All ReplicatedMergeTree tables may become read-only.
# ============================================================================
variable "keeper_log_disk_size_gb" {
  description = "Size of dedicated Keeper log disk in GB (for transaction logs). Separate from snapshot storage to prevent IOPS contention."
  type        = number
  default     = 64
}

# ============================================================================
# Application Security Group Configuration (BLINDSPOTS.md #13)
# ============================================================================
# Why: ASGs provide micro-segmentation for Keeper ports. Instead of allowing
#      entire subnet access, restrict Keeper ports to only ClickHouse and
#      Keeper nodes using ASG membership.
# Without it: Keeper ports (9181, 9234) are accessible from any host in subnet.
# ============================================================================
variable "enable_application_security_groups" {
  description = "Enable Application Security Groups to restrict Keeper ports to ClickHouse nodes only"
  type        = bool
  default     = true
}

# ============================================================================
# Boot Diagnostics Configuration
# ============================================================================
# Why: Boot diagnostics capture console output and screenshots during VM boot,
#      essential for troubleshooting cloud-init failures and startup issues.
# Reference: BLINDSPOTS.md #10 - Cloud-Init Failure Not Detected
# Without it: No visibility into VM boot process, making cloud-init failures
#             extremely difficult to diagnose.
# ============================================================================
variable "enable_boot_diagnostics" {
  description = "Enable boot diagnostics for VMSS instances. Required for troubleshooting cloud-init failures."
  type        = bool
  default     = true
}

variable "boot_diagnostics_storage_account_uri" {
  description = "Storage account URI for boot diagnostics. If null, uses managed boot diagnostics."
  type        = string
  default     = null
}

# ============================================================================
# Spot VM Configuration for Production Safety
# ============================================================================
# Why: Spot VMs can be evicted at any time, which is risky for production.
#      Default changed to false for readers to ensure production stability.
# Reference: BLINDSPOTS.md #9 - Spot VMs Enabled by Default for Readers
# ============================================================================
# Note: enable_reader_spot_vms already exists but defaulted to true.
# Changing default would be a breaking change, so adding explicit warning.

# ============================================================================
# Private Endpoint Configuration (BLINDSPOTS.md #14)
# ============================================================================
# Why: Per Azure Security Baseline, use Private Endpoints for secure access
#      to Storage and Key Vault instead of service endpoints.
# Without it: Traffic to PaaS services traverses public internet.
# ============================================================================

# ============================================================================
# Encryption at Host Configuration
# ============================================================================
# Why: Per Azure Security Baseline, host-based encryption encrypts OS disk,
#      data disks, and temp disks at the host level before data reaches storage.
# Requirement: Subscription must have Microsoft.Compute/EncryptionAtHost feature.
# Without it: Disks rely on default Azure encryption.
# ============================================================================

# ============================================================================
# Azure Monitor Agent Configuration
# ============================================================================
# Why: Azure Monitor Agent collects logs and metrics from VMSS instances and
#      sends them to Log Analytics Workspace. Installing it as an inline
#      extension (part of the VMSS model) ensures it's installed during VM
#      creation, avoiding rolling upgrade failures on fresh deployments.
# Without it: If installed as a separate VMSS extension resource, it triggers
#             a rolling upgrade that fails when instances are still initializing
#             (100% unhealthy exceeds the 20% max_unhealthy_instance_percent).
# ============================================================================
variable "enable_azure_monitor_agent" {
  description = "Enable Azure Monitor Agent extension on VMSS instances. Installs agent during VM creation."
  type        = bool
  default     = false
}

variable "log_analytics_workspace_id" {
  description = "Log Analytics Workspace ID for Azure Monitor Agent. Required when enable_azure_monitor_agent is true."
  type        = string
  default     = null
}

# ============================================================================
# VNet Flow Logs Configuration (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per Azure Security Baseline, VNet Flow Logs are required for network
#      traffic auditing, anomaly detection, and security incident investigation.
#      VNet Flow Logs are the recommended replacement for NSG Flow Logs
#      (NSG Flow Logs retire September 2027).
# Without it: Cannot audit network traffic patterns, detect intrusions, or
#             investigate security incidents. Non-compliant with security frameworks.
# ============================================================================

# ============================================================================
# Trusted Launch Configuration (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per Azure Security Baseline, Trusted Launch protects against boot kits,
#      rootkits, and kernel-level malware. It includes Secure Boot, vTPM, and
#      integrity monitoring. This is now the default for new Azure Gen2 VMs.
# Reference: https://learn.microsoft.com/en-us/azure/virtual-machines/trusted-launch
# Without it: VMs are vulnerable to firmware rootkits, boot kits, and kernel
#             rootkits that can compromise the entire system before the OS loads.
# ============================================================================

# ============================================================================
# Azure Bastion Configuration
# ============================================================================
# Why: Azure Bastion provides secure SSH/RDP access to VMs without exposing
#      them to the public internet. This is the recommended approach for
#      production environments per Azure Security Baseline.
# Cost: ~$0.19/hour when running (~$140/month if always on).
#       Use the start-bastion.sh script to start on-demand and auto-shutdown.
# Without it: SSH requires either public IPs on VMs (security risk) or VPN.
# ============================================================================

# ============================================================================
# Ephemeral OS Disk Configuration (FINOPS OPTIMIZATION)
# ============================================================================
# Why: Ephemeral OS disks are free and faster than managed disks because they
#      use the VM's local temp disk or resource disk. Since ClickHouse is a
#      distributed system with data on separate data disks and object storage,
#      the OS disk is effectively stateless and can be treated as ephemeral.
# Trade-off: Data on the OS disk is lost when the VM is deallocated, resized,
#            or during platform maintenance. This is acceptable for VMSS nodes
#            that are designed to be replaceable.
# Savings: ~$10-12/month by eliminating E4 managed disk costs for OS disks.
# Monitoring: If experiencing issues, check these metrics in Azure Monitor:
#      - "OS Disk Queue Depth" > 2: Indicates disk I/O bottleneck
#      - "OS Disk IOPS Consumed Percentage" > 90%: Near IOPS limit
#      - "OS Disk Bandwidth Consumed Percentage" > 90%: Near throughput limit
#      - "Available Memory Bytes": Low memory can cause swap to OS disk
# Reference: https://learn.microsoft.com/en-us/azure/virtual-machines/ephemeral-os-disks
# ============================================================================

# ============================================================================
# Proximity Placement Group Configuration (PERFORMANCE OPTIMIZATION)
# ============================================================================
# Why: Azure Proximity Placement Groups ensure VMs are physically co-located
#      in the same data center, reducing network latency by 27-33%. Critical
#      for distributed databases requiring low-latency inter-node communication.
# Reference: Implementation Plan - Azure ClickHouse Infrastructure Optimizations
# Trade-off: PPGs can occasionally cause allocation failures if VM sizes are
#            unavailable in the selected data center. Does NOT span availability
#            zones (reduces HA scope slightly).
# Without it: VMs may be scattered across different data centers even within
#             the same availability zone, causing higher inter-node latency,
#             slower distributed queries, and increased replication lag.
# ============================================================================
variable "enable_proximity_placement_group" {
  description = "Enable Proximity Placement Group for low-latency inter-node communication. Trade-off: may cause allocation failures in constrained regions."
  type        = bool
  default     = true
}

# ============================================================================
# Accelerated Networking Configuration (PERFORMANCE OPTIMIZATION)
# ============================================================================
# Why: Accelerated Networking uses SR-IOV to bypass the VM host's virtual
#      switch, reducing latency, jitter, and CPU utilization. Essential for
#      high-throughput database replication and distributed query processing.
# Reference: Implementation Plan - Azure ClickHouse Infrastructure Optimizations
# Note: Azure enables this automatically for supported VM sizes, but explicit
#       configuration ensures it's never accidentally disabled and provides
#       visibility in Terraform state.
# Without it: Higher network latency, increased jitter during replication,
#             and higher CPU utilization on high-throughput workloads.
# ============================================================================
variable "enable_accelerated_networking" {
  description = "Explicitly enable Accelerated Networking on NICs for high-throughput database replication. Defaults to true for supported VM sizes."
  type        = bool
  default     = true
}

# ============================================================================
# Golden Image Configuration (BOOTSTRAP OPTIMIZATION)
# ============================================================================
# Why: Pre-baked images with ClickHouse, Azure CLI, and OS optimizations
#      pre-installed reduce VMSS bootstrap time from ~8-12 minutes to ~2-3
#      minutes. Images are built with Packer and stored in Azure Compute Gallery.
# Without it: Every new VMSS instance must download and install ClickHouse,
#             Azure CLI, configure OS settings, adding 5-10 minutes to bootstrap.
# ============================================================================

variable "golden_image_id" {
  description = "Resource ID of the golden image version from Azure Compute Gallery. Format: /subscriptions/.../galleries/.../images/.../versions/..."
  type        = string
  default     = null

  validation {
    condition     = var.golden_image_id == null || can(regex("^/subscriptions/.+/galleries/.+/images/.+/versions/.+$", var.golden_image_id))
    error_message = "golden_image_id must be a valid Azure Compute Gallery image version resource ID."
  }
}

