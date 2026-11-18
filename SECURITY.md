<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Security Policy

## Reporting a Vulnerability

Please report security vulnerabilities privately via [GitHub Security Advisories](https://github.com/stackgenhq/chinit/security/advisories/new) ("Report a vulnerability" under the Security tab) rather than opening a public issue.

- We aim to acknowledge vulnerability reports within 2 business days.
- Confirmed security fixes are released as patch versions on the `v0.x` release line.
- All release binaries are published alongside `chinit-SHA256SUMS`, and golden-image builds verify SHA-256 digests before installation.

## Scope

- **In scope**: The `chinit` binary (`cmd/` and `internal/`), the golden-image Packer templates in `images/`, the Terraform modules in `terraform/`, and the release workflows in `.github/workflows/`.
- **Out of scope**: Upstream ClickHouse binaries, third-party dependencies, and external infrastructure configurations outside this repository.
