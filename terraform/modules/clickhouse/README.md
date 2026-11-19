<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# ClickHouse Module — a storage-tier EXTENSION of the chinit contract module

This module does not render the chinit node specs, the SHA-gated
bundle, or the user-data wrappers itself. It consumes the
**open-source contract module**
([`modules/chinit/aws`](../chinit/aws))
and extends it with a multi-tenant storage tier:

| Owned here (the extension) | Owned by the contract module |
|---|---|
| The tenant/CAS tiered `storage.xml` | The role specs (`chinit-<role>.yaml`) |
| The tuned CHI/CHK specs (operator-parity settings, profiles, query logs) | The static XML trio + the default DDL |
| | The SHA-gated bundle digest + uploads |
| | The in-place config convergence (`chinit spec-sync`/`config-check`, the module's timer jobs) |
| | The user-data wrappers |

The chinit interface has exactly one implementation, in this repo;
this module feeds it through the `chi_spec`, `keeper_spec`, and
`config_files` extension hooks (and receives the wrappers + the
`config_files_hash` back, re-exported under this module's original
output names so the root wiring is unchanged).

## The extension hooks in use

```hcl
module "chinit" {
  source = "./../chinit/aws"

  # ... the cluster identity, ceilings, resource ids, secret names ...

  # THIS stack's overrides (the digest covers exactly these members):
  config_files = {
    "configs/storage.xml" = local.config_files_all["configs/storage.xml"]
  }
  chi_spec     = local.chi_spec    # the tuned cluster spec
  keeper_spec  = local.keeper_spec # the tuned keeper spec
}
```

Stacks with their own bootstrap DDL pass it as an additional
`config_files` member — the bundle digest covers whatever set of files
the caller supplies.

A chinit spec/bundle/wrapper fix lands in the contract module and flows
here on the next `tofu init` — consumers extend the ecosystem; they
never fork it.
