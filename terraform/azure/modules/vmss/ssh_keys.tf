# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# SSH Key Generation
# ============================================================================
# Why: Generates an SSH key pair for VM access if not provided. The private key
#      is saved to local disk for later use, and the public key is used for
#      VM authentication. This eliminates the need to manually generate and
#      provide SSH keys.
# Without it: Users must manually generate SSH keys and provide them via
#             variables, which is error-prone and requires additional setup steps.
# ============================================================================
resource "tls_private_key" "vm_ssh" {
  count     = var.vm_ssh_public_key == null ? 1 : 0
  algorithm = "RSA"
  rsa_bits  = 4096
}

# Save the private key to disk for later use
# Why: Saves the generated private key so users can use it to SSH into VMs.
#      The key is saved with proper permissions (600) for security.
# Without it: Users cannot retrieve the generated private key to SSH into VMs.
resource "local_file" "vm_ssh_private_key" {
  count           = var.vm_ssh_public_key == null && var.ssh_key_output_path != null ? 1 : 0
  content         = tls_private_key.vm_ssh[0].private_key_pem
  filename        = "${var.ssh_key_output_path}/id_rsa"
  file_permission = "0600"
}

# Save the public key to disk for reference
# Why: Saves the generated public key for reference and verification.
# Without it: Users cannot verify which public key was used for VM access.
resource "local_file" "vm_ssh_public_key" {
  count           = var.vm_ssh_public_key == null && var.ssh_key_output_path != null ? 1 : 0
  content         = tls_private_key.vm_ssh[0].public_key_openssh
  filename        = "${var.ssh_key_output_path}/id_rsa.pub"
  file_permission = "0644"
}

# Local for determining which SSH key to use
locals {
  ssh_public_key = var.vm_ssh_public_key != null ? var.vm_ssh_public_key : tls_private_key.vm_ssh[0].public_key_openssh
}
