# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Public IP for Writers Load Balancer
# ============================================================================
# Why: Provides a dedicated public IP address for writer load balancer,
#      enabling external access to ClickHouse writer nodes. Applications
#      performing write operations connect via this IP.
# Without it: Writer load balancer cannot have a public endpoint. External
#             write operations require VPN or private network connection.
# ============================================================================
resource "azurerm_public_ip" "writers_lb" {
  name                = var.names.lb_writers_pip
  location            = var.location
  resource_group_name = var.resource_group_name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = var.tags

  # DDoS protection is managed separately - ignore changes from state
  lifecycle {
    ignore_changes = [ddos_protection_mode, ddos_protection_plan_id]
  }
}

# ============================================================================
# Public IP for Readers Load Balancer
# ============================================================================
# Why: Provides a dedicated public IP address for reader load balancer,
#      enabling external access to ClickHouse reader nodes. Applications
#      performing read operations connect via this IP.
# Without it: Reader load balancer cannot have a public endpoint. External
#             read operations require VPN or private network connection.
# ============================================================================
resource "azurerm_public_ip" "readers_lb" {
  name                = var.names.lb_readers_pip
  location            = var.location
  resource_group_name = var.resource_group_name
  allocation_method   = "Static"
  sku                 = "Standard"
  tags                = var.tags

  # DDoS protection is managed separately - ignore changes from state
  lifecycle {
    ignore_changes = [ddos_protection_mode, ddos_protection_plan_id]
  }
}

# ============================================================================
# Load Balancer for Writers
# ============================================================================
# Why: Distributes incoming ClickHouse write connections across writer VMs,
#      providing high availability and load distribution for write operations.
#      Applications performing writes connect to a single endpoint that
#      routes to healthy writer VMs.
# Without it: Applications must connect directly to individual writer VM
#             IPs, requiring manual failover and no load distribution. Single
#             point of failure if a writer VM becomes unavailable.
# ============================================================================
resource "azurerm_lb" "writers" {
  name                = var.names.lb_writers
  location            = var.location
  resource_group_name = var.resource_group_name
  sku                 = "Standard"
  tags                = var.tags

  frontend_ip_configuration {
    name                 = "PublicIPAddress"
    public_ip_address_id = azurerm_public_ip.writers_lb.id
  }
}

# ============================================================================
# Load Balancer for Readers
# ============================================================================
# Why: Distributes incoming ClickHouse read connections across reader VMs,
#      providing high availability and load distribution for read operations.
#      Applications performing reads connect to a single endpoint that routes
#      to healthy reader VMs.
# Without it: Applications must connect directly to individual reader VM
#             IPs, requiring manual failover and no load distribution. Single
#             point of failure if a reader VM becomes unavailable.
# ============================================================================
resource "azurerm_lb" "readers" {
  name                = var.names.lb_readers
  location            = var.location
  resource_group_name = var.resource_group_name
  sku                 = "Standard"
  tags                = var.tags

  frontend_ip_configuration {
    name                 = "PublicIPAddress"
    public_ip_address_id = azurerm_public_ip.readers_lb.id
  }
}

# ============================================================================
# Load Balancer Backend Address Pool for Writers
# ============================================================================
# Why: Defines the pool of writer VMs that will receive traffic from the
#      writer load balancer. All writer VMs are added to
#      this pool for load distribution of write operations.
# Without it: Writer load balancer has no backend targets. Write traffic
#             cannot be routed to any VMs, making the load balancer
#             non-functional.
# ============================================================================
resource "azurerm_lb_backend_address_pool" "writers" {
  loadbalancer_id = azurerm_lb.writers.id
  name            = var.names.lb_backend_writers
}

# ============================================================================
# Load Balancer Backend Address Pool for Readers
# ============================================================================
# Why: Defines the pool of reader VMs that will receive traffic from the
#      reader load balancer. All reader VMs are added to this pool for load
#      distribution of read operations.
# Without it: Reader load balancer has no backend targets. Read traffic
#             cannot be routed to any VMs, making the load balancer
#             non-functional.
# ============================================================================
resource "azurerm_lb_backend_address_pool" "readers" {
  loadbalancer_id = azurerm_lb.readers.id
  name            = var.names.lb_backend_readers
}

# ============================================================================
# Load Balancer Health Probe (HTTPS) for Writers
# ============================================================================
# Why: Monitors the health of ClickHouse writer VMs by checking the HTTPS
#      /ping endpoint on port 8443 (TLS-enabled). Unhealthy writer VMs are
#      removed from the load balancer pool automatically. Uses HTTPS protocol
#      to verify ClickHouse is responding with TLS encryption.
# Security: Uses TLS port 8443 instead of plain HTTP 8123 to ensure all
#           public-facing traffic is encrypted.
# Without it: Load balancer cannot detect unhealthy writer VMs. Write traffic
#             may be routed to failed VMs, causing connection failures and
#             poor user experience.
# ============================================================================
resource "azurerm_lb_probe" "writers_https" {
  loadbalancer_id     = azurerm_lb.writers.id
  name                = "${var.names.lb_writers}-https-probe"
  port                = local.health_probe_port
  protocol            = title(local.health_probe_protocol)
  request_path        = local.health_probe_path
  interval_in_seconds = 10
  number_of_probes    = 3
}

# ============================================================================
# Load Balancer Health Probe (Native Secure Protocol) for Writers
# ============================================================================
# Why: Monitors the health of ClickHouse writer VMs via native secure protocol
#      on port 9440 (TLS-enabled). Provides health check for TLS-encrypted
#      native protocol connections.
# Security: Uses TLS port 9440 instead of plain native 9000 to ensure all
#           public-facing traffic is encrypted.
# Without it: Only HTTPS health check exists, native protocol health may not
#             be accurately reflected.
# ============================================================================
resource "azurerm_lb_probe" "writers_native_secure" {
  loadbalancer_id     = azurerm_lb.writers.id
  name                = "${var.names.lb_writers}-native-secure-probe"
  port                = 9440
  protocol            = "Tcp"
  interval_in_seconds = 10
  number_of_probes    = 3
}

# ============================================================================
# Load Balancer Health Probe (HTTPS) for Readers
# ============================================================================
# Why: Monitors the health of ClickHouse reader VMs by checking the HTTPS
#      /ping endpoint on port 8443 (TLS-enabled). Unhealthy reader VMs are
#      removed from the load balancer pool automatically. Uses HTTPS protocol
#      to verify ClickHouse is responding with TLS encryption.
# Security: Uses TLS port 8443 instead of plain HTTP 8123 to ensure all
#           public-facing traffic is encrypted.
# Without it: Load balancer cannot detect unhealthy reader VMs. Read traffic
#             may be routed to failed VMs, causing connection failures and
#             poor user experience.
# ============================================================================
resource "azurerm_lb_probe" "readers_https" {
  loadbalancer_id     = azurerm_lb.readers.id
  name                = "${var.names.lb_readers}-https-probe"
  port                = local.health_probe_port
  protocol            = title(local.health_probe_protocol)
  request_path        = local.health_probe_path
  interval_in_seconds = 10
  number_of_probes    = 3
}

# ============================================================================
# Load Balancer Health Probe (Native Secure Protocol) for Readers
# ============================================================================
# Why: Monitors the health of ClickHouse reader VMs via native secure protocol
#      on port 9440 (TLS-enabled). Provides health check for TLS-encrypted
#      native protocol connections.
# Security: Uses TLS port 9440 instead of plain native 9000 to ensure all
#           public-facing traffic is encrypted.
# Without it: Only HTTPS health check exists, native protocol health may not
#             be accurately reflected.
# ============================================================================
resource "azurerm_lb_probe" "readers_native_secure" {
  loadbalancer_id     = azurerm_lb.readers.id
  name                = "${var.names.lb_readers}-native-secure-probe"
  port                = 9440
  protocol            = "Tcp"
  interval_in_seconds = 10
  number_of_probes    = 3
}

# ============================================================================
# Load Balancer Rule (HTTPS) for Writers
# ============================================================================
# Why: Routes HTTPS traffic (port 8443) from the writer load balancer to
#      healthy ClickHouse writer VMs. Enables TLS-encrypted HTTP-based write
#      access to ClickHouse.
# Security: Uses TLS port 8443 instead of plain HTTP 8123 to ensure all
#           public-facing traffic is encrypted per Azure Security Baseline.
# Without it: HTTPS write traffic cannot reach ClickHouse writer VMs through
#             the load balancer. Applications cannot perform secure writes via
#             HTTPS protocol.
# ============================================================================
resource "azurerm_lb_rule" "writers_https" {
  loadbalancer_id                = azurerm_lb.writers.id
  name                           = "${var.names.lb_writers}-https-rule"
  protocol                       = "Tcp"
  frontend_port                  = 443
  backend_port                   = local.health_probe_port
  frontend_ip_configuration_name = "PublicIPAddress"
  backend_address_pool_ids       = [azurerm_lb_backend_address_pool.writers.id]
  probe_id                       = azurerm_lb_probe.writers_https.id
}

# ============================================================================
# Load Balancer Rule (HTTPS) for Readers
# ============================================================================
# Why: Routes HTTPS traffic (port 8443) from the reader load balancer to
#      healthy ClickHouse reader VMs. Enables TLS-encrypted HTTP-based read
#      access to ClickHouse.
# Security: Uses TLS port 8443 instead of plain HTTP 8123 to ensure all
#           public-facing traffic is encrypted per Azure Security Baseline.
# Without it: HTTPS read traffic cannot reach ClickHouse reader VMs through
#             the load balancer. Applications cannot perform secure reads via
#             HTTPS protocol.
# ============================================================================
resource "azurerm_lb_rule" "readers_https" {
  loadbalancer_id                = azurerm_lb.readers.id
  name                           = "${var.names.lb_readers}-https-rule"
  protocol                       = "Tcp"
  frontend_port                  = 443
  backend_port                   = local.health_probe_port
  frontend_ip_configuration_name = "PublicIPAddress"
  backend_address_pool_ids       = [azurerm_lb_backend_address_pool.readers.id]
  probe_id                       = azurerm_lb_probe.readers_https.id
}

# ============================================================================
# Load Balancer Health Probe (TCP) for Keepers
# ============================================================================
# Why: Monitors the health of ClickHouse Keeper VMs via TCP port 9181.
#      Ensures that only healthy Keeper nodes are considered active.
# ============================================================================
resource "azurerm_lb_probe" "keepers_tcp" {
  loadbalancer_id     = azurerm_lb.writers.id
  name                = "${var.names.lb_writers}-keeper-tcp-probe"
  port                = 9181
  protocol            = "Tcp"
  interval_in_seconds = 10
  number_of_probes    = 3
}

# ============================================================================
# Load Balancer Backend Address Pool for Keepers
# ============================================================================
# Why: Defines the pool of Keeper VMs for health monitoring.
# ============================================================================
resource "azurerm_lb_backend_address_pool" "keepers" {
  loadbalancer_id = azurerm_lb.writers.id
  name            = "${var.names.lb_writers}-backend-keepers"
}

