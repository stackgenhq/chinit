<!--
  Copyright (C) 2026 StackGen, Inc. All rights reserved.
  SPDX-License-Identifier: Apache-2.0
-->

# Golden Images (Packer)

Pre-baked VM images reduce node bootstrap time from 8–12 minutes to 2–3 minutes. ClickHouse, the pinned `chinit` binary, backup tooling, monitoring agents, and OS optimizations are installed at image build time so nodes require no outbound internet access during boot.

| Cloud | Definition | Output |
|---|---|---|
| AWS | [`aws/`](./aws) | Golden AMI (`clickhouse-golden-…`) |
| Azure | [`azure/`](./azure) | Azure Compute Gallery image version |

Both image definitions can be built locally with Packer or via GitHub Actions workflows in `.github/workflows/`.

- **Included packages, version policy, and build flavors**: [`aws/README.md`](./aws/README.md) and [`azure/README.md`](./azure/README.md)
- **Node boot procedure**: [`BOOT.md`](./BOOT.md) covers identity modes, the convergent boot pipeline, spot draining, and recurring maintenance jobs across AWS and Azure.
- **Why golden images on VMs**: See the [project README](../README.md#where-it-fits).

During the Packer build, the pinned `chinit` release binary is downloaded from GitHub Releases and verified against `chinit-SHA256SUMS` before being installed at `/usr/local/bin/chinit`.

**Prebuilt public AMIs**: Public AWS golden AMIs are available in `us-west-2` for both upstream ClickHouse 26.8 LTS (`ami-0075eb79e447956ca`) and Altinity Antalya 26.8 LTS with CAS (`ami-04292dd0d9281579e`) — see [`aws/README.md`](./aws/README.md) for details.
