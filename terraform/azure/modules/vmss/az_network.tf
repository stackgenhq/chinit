# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Virtual Network - Native azurerm Resource
# ============================================================================
# Why: The virtual network provides network isolation and IP address space
#      for all ClickHouse VMs. Using native azurerm resource instead of AVM
#      module guarantees idempotent applies without azapi state drift issues.
# Without it: VMs cannot be created as they require a network. Network
#             isolation and security boundaries cannot be established.
# ============================================================================
resource "azurerm_virtual_network" "main" {
  name                = var.names.virtual_network
  address_space       = [var.network_config.vnet_address_space]
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# Subnet - Native azurerm Resource
# ============================================================================
# Why: Provides the subnet within the VNet for ClickHouse resources. Using
#      native azurerm_subnet ensures idempotent applies without azapi issues.
# Without it: VMs cannot have network interfaces without a subnet.
# ============================================================================
resource "azurerm_subnet" "clickhouse" {
  name                 = var.names.subnet
  resource_group_name  = var.resource_group_name
  virtual_network_name = azurerm_virtual_network.main.name
  address_prefixes     = [var.network_config.subnet_address_prefix]

  # Service endpoints for secure Azure service access (SECURITY AUDIT FIX)
  # Microsoft.KeyVault - Required for Key Vault network rules
  # Microsoft.Storage - Required for Storage Account VNet rules (Azure Policy compliance)
  service_endpoints = ["Microsoft.KeyVault", "Microsoft.Storage"]
}

# ============================================================================
# Application Security Groups (ASGs)
# ============================================================================
# Why: Per Azure security best practices, use ASGs to restrict Keeper ports
#      to only ClickHouse nodes instead of the entire subnet. This implements
#      micro-segmentation and reduces the attack surface.
# Reference: BLINDSPOTS.md #13 - Keeper Ports Open to Entire Subnet
# Without it: Keeper ports (9181, 9234) are accessible from any host in the
#             subnet, increasing the attack surface if another VM is compromised.
# ============================================================================
resource "azurerm_application_security_group" "clickhouse_nodes" {
  count               = var.enable_application_security_groups ? 1 : 0
  name                = "${var.cluster_name}-clickhouse-nodes"
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

resource "azurerm_application_security_group" "keeper_nodes" {
  count               = var.enable_application_security_groups ? 1 : 0
  name                = "${var.cluster_name}-keeper-nodes"
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# Network Security Group
# ============================================================================
# Why: The NSG provides firewall rules to control network traffic to and
#      from ClickHouse VMs. Only TLS-encrypted ports are allowed per Azure
#      Security Baseline and CIS Benchmark requirements.
# SECURITY: Non-TLS ports (8123, 9000, 9009) are intentionally NOT allowed.
#           All client connections must use TLS-encrypted ports:
#           - HTTPS: 8443 (instead of HTTP 8123)
#           - Native TLS: 9440 (instead of native 9000)
#           - Interserver TLS: 9010 (instead of interserver 9009)
# Without it: All network traffic is allowed by default, creating security
#             risks. Unauthorized access attempts cannot be blocked at the
#             network level.
# ============================================================================
resource "azurerm_network_security_group" "clickhouse" {
  name                = var.names.nsg
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags

  # ============================================================================
  # NOTE: Non-TLS ports (8123, 9000, 9009) are intentionally NOT allowed.
  # Per CIS Benchmark and Azure Security Baseline, all traffic must be encrypted.
  # Clients must use TLS ports: 8443 (HTTPS), 9440 (Native TLS), 9010 (Interserver TLS)
  # ============================================================================

  # Allow SSH port (22) for VM management
  # Why: Enables SSH access for troubleshooting, configuration updates,
  #      and administrative tasks.
  # Without it: SSH access is blocked, making VM management and
  #             troubleshooting difficult or impossible.
  # Security: Only allow from specified IPs or VNet subnet (no public internet access)
  #           SSH should never be exposed to the public internet.
  security_rule {
    name                       = "AllowSSH"
    priority                   = 1003
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "22"
    source_address_prefix      = length(var.allowed_ssh_ips) > 0 ? null : var.network_config.subnet_address_prefix
    source_address_prefixes    = length(var.allowed_ssh_ips) > 0 ? var.allowed_ssh_ips : null
    destination_address_prefix = "*"
  }

  # Allow Keeper client port (9181) for ClickHouse Keeper coordination
  # Why: Enables ClickHouse nodes (writers, readers) to connect to Keeper for
  #      coordination and metadata management. Port 9181 is the recommended
  #      ClickHouse Keeper client port (not 2181 which is legacy ZooKeeper).
  # Reference: https://clickhouse.com/docs/guides/sre/network-ports
  # SECURITY FIX: When ASGs are enabled, restrict to ClickHouse nodes only
  #               instead of entire subnet (BLINDSPOTS.md #13)
  # Without it: Keeper nodes cannot receive client connections, breaking cluster
  #             coordination and ReplicatedMergeTree functionality.
  security_rule {
    name                                       = "AllowKeeperClient"
    priority                                   = 1004
    direction                                  = "Inbound"
    access                                     = "Allow"
    protocol                                   = "Tcp"
    source_port_range                          = "*"
    destination_port_range                     = "9181"
    source_address_prefix                      = var.enable_application_security_groups ? null : var.network_config.subnet_address_prefix
    source_application_security_group_ids      = var.enable_application_security_groups ? [azurerm_application_security_group.clickhouse_nodes[0].id] : null
    destination_address_prefix                 = var.enable_application_security_groups ? null : "*"
    destination_application_security_group_ids = var.enable_application_security_groups ? [azurerm_application_security_group.keeper_nodes[0].id] : null
  }

  # Allow Keeper Raft port (9234) for Keeper inter-server communication
  # Why: Enables Keeper nodes to communicate with each other for Raft consensus.
  #      This is required for leader election, log replication, and quorum.
  # Reference: https://clickhouse.com/docs/guides/sre/network-ports
  # SECURITY FIX: When ASGs are enabled, restrict to Keeper nodes only
  #               (Keeper-to-Keeper communication) (BLINDSPOTS.md #13)
  # Without it: Keeper nodes cannot form a quorum or elect a leader, breaking
  #             the entire Keeper cluster and all dependent ClickHouse operations.
  security_rule {
    name                                       = "AllowKeeperRaft"
    priority                                   = 1009
    direction                                  = "Inbound"
    access                                     = "Allow"
    protocol                                   = "Tcp"
    source_port_range                          = "*"
    destination_port_range                     = "9234"
    source_address_prefix                      = var.enable_application_security_groups ? null : var.network_config.subnet_address_prefix
    source_application_security_group_ids      = var.enable_application_security_groups ? [azurerm_application_security_group.keeper_nodes[0].id] : null
    destination_address_prefix                 = var.enable_application_security_groups ? null : "*"
    destination_application_security_group_ids = var.enable_application_security_groups ? [azurerm_application_security_group.keeper_nodes[0].id] : null
  }

  # ============================================================================
  # TLS/SSL Secure Ports
  # ============================================================================
  # The following rules enable secure (TLS-encrypted) connections to ClickHouse.
  # TLS is mandatory and always enabled - these ports are always available.
  # ============================================================================

  # Allow HTTPS port (8443) for secure HTTP interface
  # Why: Enables TLS-encrypted HTTP connections. Required for secure web
  #      interfaces and HTTPS API access.
  # Without it: HTTPS connections blocked when TLS is enabled.
  security_rule {
    name                       = "AllowClickHouseHTTPS"
    priority                   = 1005
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "8443"
    source_address_prefix      = length(var.allowed_source_ips) > 0 ? null : var.network_config.subnet_address_prefix
    source_address_prefixes    = length(var.allowed_source_ips) > 0 ? concat(var.allowed_source_ips, [var.network_config.subnet_address_prefix]) : null
    destination_address_prefix = "*"
  }

  # Allow secure native protocol port (9440) for TLS-encrypted native client
  # Why: Enables TLS-encrypted native ClickHouse connections. Required for
  #      secure ch-go and other native client connections.
  # Without it: Secure native protocol connections blocked when TLS is enabled.
  security_rule {
    name                       = "AllowClickHouseNativeSecure"
    priority                   = 1006
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "9440"
    source_address_prefix      = length(var.allowed_source_ips) > 0 ? null : var.network_config.subnet_address_prefix
    source_address_prefixes    = length(var.allowed_source_ips) > 0 ? concat(var.allowed_source_ips, [var.network_config.subnet_address_prefix]) : null
    destination_address_prefix = "*"
  }

  # Allow secure interserver port (9010) for TLS-encrypted cluster communication
  # Why: Enables TLS-encrypted communication between ClickHouse nodes for
  #      secure replication and distributed queries.
  # Without it: Secure interserver communication blocked when TLS is enabled.
  security_rule {
    name                       = "AllowClickHouseInterserverSecure"
    priority                   = 1007
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "9010"
    source_address_prefix      = var.network_config.subnet_address_prefix
    destination_address_prefix = "*"
  }

  # ============================================================================
  # Lifecycle - Ignore Security Rule Ordering/Casing Drift
  # ============================================================================
  # Why: Azure API returns ASG IDs with different casing than Terraform/OpenTofu
  #      generates, causing perpetual diffs even when nothing has changed.
  #      The rules themselves are functionally identical (Azure is case-insensitive).
  # This ensures idempotent applies regardless of Azure API casing behavior.
  # ============================================================================
  lifecycle {
    ignore_changes = [security_rule]
  }
}

# ============================================================================
# Associate NSG with Subnet
# ============================================================================
# Why: Applies the network security group rules to all VMs in the subnet.
#      This ensures all ClickHouse VMs inherit the firewall rules.
# Without it: NSG rules are not applied to VMs, leaving them unprotected
#             by the security rules defined above.
# ============================================================================
resource "azurerm_subnet_network_security_group_association" "clickhouse" {
  subnet_id                 = azurerm_subnet.clickhouse.id
  network_security_group_id = azurerm_network_security_group.clickhouse.id
}

# ============================================================================
# NAT Gateway (BLINDSPOTS.md #13 - SNAT Exhaustion Prevention)
# ============================================================================
# Why: Provides reliable outbound connectivity for all VMs in the subnet.
#      Prevents SNAT port exhaustion that can occur when VMs make many
#      outbound connections (Azure Blob uploads, Key Vault calls, apt updates).
#      Each NAT Gateway provides 64,000+ SNAT ports vs ~1,024 per LB backend.
# Cost: ~$32/month + ~$0.045/GB data processed
# Without it: SNAT port exhaustion during heavy outbound traffic causes:
#             - Failed blob storage uploads (ClickHouse data writes)
#             - Failed Key Vault calls (password retrieval)
#             - Failed apt update during cloud-init
#             - Lost telemetry to Azure Monitor/OTel endpoints
# ============================================================================
resource "azurerm_public_ip" "nat_gateway" {
  count               = var.network_config.enable_nat_gateway ? 1 : 0
  name                = "${var.cluster_name}-nat-pip"
  location            = var.location
  resource_group_name = var.resource_group_name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = var.tags
}

resource "azurerm_nat_gateway" "main" {
  count                   = var.network_config.enable_nat_gateway ? 1 : 0
  name                    = "${var.cluster_name}-nat-gateway"
  location                = var.location
  resource_group_name     = var.resource_group_name
  sku_name                = "Standard"
  idle_timeout_in_minutes = var.network_config.nat_gateway_idle_timeout_minutes
  tags                    = var.tags
}

resource "azurerm_nat_gateway_public_ip_association" "main" {
  count                = var.network_config.enable_nat_gateway ? 1 : 0
  nat_gateway_id       = azurerm_nat_gateway.main[0].id
  public_ip_address_id = azurerm_public_ip.nat_gateway[0].id
}

resource "azurerm_subnet_nat_gateway_association" "clickhouse" {
  count          = var.network_config.enable_nat_gateway ? 1 : 0
  subnet_id      = azurerm_subnet.clickhouse.id
  nat_gateway_id = azurerm_nat_gateway.main[0].id
}

# ============================================================================
# DDoS Protection Plan (Optional)
# ============================================================================
# Why: Provides DDoS protection for public IPs to prevent distributed denial
#      of service attacks. Azure DDoS Protection Standard automatically
#      mitigates DDoS attacks and provides attack analytics.
# Cost: ~$2,944/month - only enable for production environments.
# Without it: Public IPs rely on Azure's basic DDoS protection (free),
#             which provides less comprehensive protection but is sufficient
#             for development and staging environments.
# ============================================================================
resource "azurerm_network_ddos_protection_plan" "clickhouse" {
  count               = var.network_config.enable_ddos_protection ? 1 : 0
  name                = var.names.ddos_plan
  location            = var.location
  resource_group_name = var.resource_group_name
  tags                = var.tags
}

# ============================================================================
# Virtual Network Flow Logs (SECURITY AUDIT FIX)
# ============================================================================
# Why: Per Azure Security Baseline, network flow logs are required for:
#      - Auditing network traffic patterns
#      - Detecting anomalies and potential intrusions
#      - Investigating security incidents
#      - Compliance (PCI-DSS, HIPAA, SOC2)
# Note: VNet Flow Logs are the recommended replacement for NSG Flow Logs
#       (NSG Flow Logs will be retired September 2027).
# Cost: Storage costs for flow log data (~$0.05/GB)
# Without it: Cannot audit network traffic, detect anomalies, or investigate
#             security incidents. Non-compliant with security frameworks.
# ============================================================================

# Network Watcher (required for flow logs)
# Azure auto-creates Network Watcher in NetworkWatcherRG, but we need to
# reference it or create one in our RG for flow log management
data "azurerm_network_watcher" "main" {
  count               = var.network_config.enable_vnet_flow_logs ? 1 : 0
  name                = "NetworkWatcher_${var.location}"
  resource_group_name = "NetworkWatcherRG"
}

# VNet Flow Logs (replaces NSG Flow Logs)
resource "azurerm_network_watcher_flow_log" "vnet" {
  count                = var.network_config.enable_vnet_flow_logs ? 1 : 0
  name                 = "${var.cluster_name}-vnet-flow-log"
  network_watcher_name = data.azurerm_network_watcher.main[0].name
  resource_group_name  = data.azurerm_network_watcher.main[0].resource_group_name
  target_resource_id   = azurerm_virtual_network.main.id
  storage_account_id   = var.network_config.flow_log_storage_account_id
  enabled              = true
  version              = 2
  tags                 = var.tags

  retention_policy {
    enabled = true
    days    = var.network_config.flow_log_retention_days
  }

  # Traffic Analytics (optional, provides insights dashboard)
  dynamic "traffic_analytics" {
    for_each = var.network_config.enable_traffic_analytics && var.log_analytics_workspace_id != null ? [1] : []
    content {
      enabled               = true
      workspace_id          = var.network_config.traffic_analytics_workspace_guid
      workspace_region      = var.location
      workspace_resource_id = var.log_analytics_workspace_id
      interval_in_minutes   = var.network_config.traffic_analytics_interval_minutes
    }
  }
}
