# Copyright (C) 2026 StackGen, Inc. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# ============================================================================
# Packer Template for ClickHouse Golden AMI (AWS port)
# ============================================================================
# Builds a pre-baked AMI that cuts EC2 bootstrap from ~8-12 minutes to
# ~2-3 minutes (parity with the Azure Compute Gallery golden image):
# - Ubuntu 24.04 LTS arm64 (Graviton) base
# - ClickHouse server/client/keeper (pinned version); build_flavor selects
#   the upstream (ClickHouse Inc.) or antalya (Altinity, Apache-2.0, adds the
#   CAS disk used by the optional cold tier) apt repo
# - clickhouse-backup (pinned version)
# - AWS CLI v2 (S3 bootstrap downloads via instance role)
# - CloudWatch agent (node metrics + log shipping)
# - otelcol-contrib + Fluent Bit (optional remote log shipping to OTLP/HTTP)
# - OS optimizations (THP disabled, ulimits) baked in
#
# Usage:
#   packer init .
#   packer build -var-file=prod.pkrvars.hcl clickhouse.pkr.hcl
# Then pass the AMI id to terraform:
#   golden_ami_id = "ami-..."
# ============================================================================

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = ">= 1.0.0"
    }
  }
}

# ============================================================================
# Variables
# ============================================================================
variable "region" {
  type        = string
  description = "AWS region for building the AMI"
  default     = "us-east-1"
}

variable "instance_type" {
  type        = string
  description = "Builder instance type (arm64 Graviton)"
  default     = "c8g.large"
}

variable "ami_name" {
  type        = string
  description = "Base AMI name (leave null to auto-resolve Ubuntu 24.04 arm64)"
  default     = null
}

variable "ami_owners" {
  type        = list(string)
  description = "Owners allowed for the source AMI filter (Canonical)"
  default     = ["099720109477"]
}

variable "chinit_version" {
  type        = string
  description = "chinit release version to pre-install (from stackgenhq/chinit releases)"
  # Version history lives in CHANGELOG.md next to this template.
  # Keep `default = "<semver>"` on a single line for the release workflow.
  default = "0.1.2"
}

variable "clickhouse_version" {
  type        = string
  description = "ClickHouse version to pre-install (full version string). Must exist in the repo selected by build_flavor — Altinity Antalya ships its own version line (e.g. 26.6.8.20004.altinityantalya), upstream ships e.g. 26.8.36.3."
  # 26.8 S3Queue legacy-prefix caveat (fleet finding, 2026-11-15): a
  # format setting declared with the legacy `s3queue_` prefix in the
  # SETTINGS clause (e.g. s3queue_input_format_csv_skip_first_lines)
  # passes CREATE validation but NEVER reaches the parser — the drain
  # silently parses with defaults (ClickHouse/ClickHouse#125459). All
  # fleet DDL must use canonical spellings (input_format_*). Upstream
  # fix ClickHouse/ClickHouse#131835 (draft) — re-check when it lands.
  default     = "26.8.36.3"
}

variable "build_flavor" {
  type        = string
  description = "Which ClickHouse apt repo to install from: 'upstream' (packages.clickhouse.com, ClickHouse Inc.) or 'antalya' (builds.altinity.cloud, Altinity Antalya — Apache-2.0, adds the ContentAddressed (metadata_type=cas) disk used by the CAS cold tier)."
  default     = "upstream"

  validation {
    condition     = contains(["upstream", "antalya"], var.build_flavor)
    error_message = "Build flavor must be either 'upstream' or 'antalya'."
  }
}

variable "clickhouse_backup_version" {
  type        = string
  description = "clickhouse-backup version to pre-install"
  default     = "2.5.0"
}

variable "otel_collector_version" {
  type        = string
  description = "OpenTelemetry Collector (otelcol-contrib) version to pre-install for remote log shipping"
  default     = "0.118.0"
}

variable "fluentbit_version" {
  type        = string
  description = "Fluent Bit version to pre-install (remote log shipping alternative to otel)"
  default     = "3.2.10"
}

variable "build_id" {
  type        = string
  description = "Unique id for this build (CI passes <run_id>-<run_attempt>). Stamped as a BuildId tag on every temporary resource the builder creates, so a CI cleanup step can sweep them even when packer is killed mid-build (cancelled runs). Defaults to a timestamp for local builds."
  default     = ""
}

variable "ami_tags" {
  type        = map(string)
  description = "Tags on the produced AMI + its snapshot. The Name tag pattern identifies golden generations for cleanup sweeps and the leak guard's exclude logic."
  default = {
    Name    = "clickhouse-golden"
    Purpose = "clickhouse-golden-ami"
    Project = "clickhouse"
  }
}

variable "public_ami" {
  type        = bool
  description = "Build the public shareable flavor: unencrypted root snapshot + public launch permission (AWS refuses public encrypted AMIs). The default build stays encrypted."
  default     = false
}

variable "ami_author" {
  type        = string
  description = "Author stamped as a tag on the AMI (visible to anyone who can see the image)."
  default     = "stackgen.com"
}

variable "chinit_download_token" {
  type        = string
  description = "Bearer token for chinit release downloads (private repo: GITHUB_TOKEN / gh auth token; empty on a public repo). Never logged."
  default     = ""
  sensitive   = true
}

variable "ami_description" {
  type        = string
  description = "Description shown in DescribeImages and the console launch dialog for the public AMI."
  default     = "ClickHouse golden image (Ubuntu 24.04 arm64) with chinit - https://github.com/stackgenhq/chinit"
}

# ============================================================================
# Source
# ============================================================================
source "amazon-ebs" "clickhouse" {
  region        = var.region
  instance_type = var.instance_type

  # Plain Ubuntu Server 24.04 LTS, arm64. Avoids the metered Ubuntu Pro
  # image when Pro-only features (FIPS, livepatch) are not required.
  source_ami_filter {
    filters = {
      name                = "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-arm64-server-*"
      virtualization-type = "hvm"
      root-device-type    = "ebs"
    }
    owners      = var.ami_owners
    most_recent = true
  }

  ssh_username = "ubuntu"

  # Build in a temporary subnet — packer creates its own VPC by default.
  associate_public_ip_address = true

  launch_block_device_mappings {
    device_name           = "/dev/sda1"
    volume_size           = 30
    volume_type           = "gp3"
    # NOTE: a public (or cross-account) AMI cannot be EBS-encrypted —
    # AWS refuses encrypted public images. The public_ami flavor builds
    # an unencrypted snapshot; the default build stays encrypted.
    encrypted             = var.public_ami ? false : true
    delete_on_termination = true
  }

  # ami_name carries the ClickHouse version + git sha so Terraform can pin
  # `golden_ami_id` exactly, and the cleanup step can recognize generations.
  ami_name      = "clickhouse-golden-${var.clickhouse_version}-{{timestamp}}"
  ami_regions   = [var.region]
  # Author/usage metadata for the public listing: every consumer can see
  # these in DescribeImages / the console launch dialog.
  ami_description = var.ami_description

  # The public flavor carries its own Purpose tag so the private
  # generation-prune step never sweeps published images (one published
  # generation, retired by its own workflow job). Flavor stamps upstream
  # vs antalya so parallel matrix builds prune only their own flavor track.
  snapshot_tags = merge(
    var.public_ami ? merge(var.ami_tags, { Purpose = "clickhouse-golden-ami-public" }) : var.ami_tags,
    { Flavor = var.build_flavor }
  )
  tags = merge(
    var.public_ami ? merge(var.ami_tags, { Purpose = "clickhouse-golden-ami-public" }) : var.ami_tags,
    {
      Author = var.ami_author
      Flavor = var.build_flavor
    }
  )

  # Public launch permission when building the share flavor:
  # ami_groups=["all"] makes the image launchable by any AWS account.
  # An encrypted AMI cannot be public — the public_ami variable gates
  # both the encryption flag and this group.
  ami_users   = []
  ami_groups  = var.public_ami ? ["all"] : []

  # run_tags / run_volume_tags: applied via TagSpecification AT LAUNCH so
  # the CI role's IAM conditions (aws:RequestTag/aws:ResourceTag
  # Purpose=packer) can scope RunInstances/Stop/Terminate/CreateTags to
  # ONLY the builder - CI cannot touch any other instance in the account.
  run_tags = merge({
    Name    = "clickhouse-golden-builder"
    Purpose = "packer"
    }, var.build_id != "" ? { BuildId = var.build_id } : {})
  run_volume_tags = merge({
    Name    = "clickhouse-golden-builder"
    Purpose = "packer"
    }, var.build_id != "" ? { BuildId = var.build_id } : {})
}

# ============================================================================
# Build
# ============================================================================
build {
  sources = ["source.amazon-ebs.clickhouse"]

  # chinit is downloaded by the LAST install step below, NOT staged by
  # a file provisioner: a packer/** push races the chinit tag's
  # GoReleaser publish (three builds 404'd before this — 0.2.19/
  # 0.2.21/0.2.22), and the builder spends ~10 minutes on apt/
  # ClickHouse/backup/AWS-CLI/OTEL installs first. By the last step
  # the release has minutes of publish window; the step polls with
  # backoff for the remainder (the builder has internet — every other
  # component downloads at build time too).

  provisioner "shell" {
    # CHINIT_DOWNLOAD_TOKEN (in the inline below, from the sensitive
    # var): releases of a PRIVATE repo are not anonymously fetchable —
    # CI passes the workflow GITHUB_TOKEN; local builds pass `gh auth
    # token`. Empty on a public repo (the anonymous URL just works).
    inline = [
      "set -e",
      # ------------------------------------------------------------------
      # ClickHouse (official apt repo, pinned version)
      # Why pre-installed: apt install of ClickHouse alone takes 3-5 minutes;
      #      golden images get nodes query-ready in ~2-3 minutes after boot.
      # ------------------------------------------------------------------
      // Upstream: ClickHouse Inc. apt repo. Antalya: Altinity's repo
      // (Apache-2.0 build of the same source + CAS). Only the repo handler
      // differs; the package names (clickhouse-server/-client/-common-static)
      // and the apt install line are identical in both.
      # Build flavor selects the apt repo. Both publish arm64 (Graviton) and
      # amd64; both sign with a repo key we dearmor into the same keyring path.
      # Antalya's repo is a single flat `stable` suite (see builds.altinity.cloud).
      var.build_flavor == "antalya" ? "sudo curl -fsSL 'https://builds.altinity.cloud/antalya-apt-repo/pubkey.gpg' | sudo gpg --dearmor -o /usr/share/keyrings/clickhouse-keyring.gpg" : "sudo curl -fsSL 'https://packages.clickhouse.com/rpm/lts/repodata/repomd.xml.key' | sudo gpg --dearmor -o /usr/share/keyrings/clickhouse-keyring.gpg",
      var.build_flavor == "antalya" ? "echo 'deb [signed-by=/usr/share/keyrings/clickhouse-keyring.gpg arch=arm64] https://builds.altinity.cloud/antalya-apt-repo stable main' | sudo tee /etc/apt/sources.list.d/clickhouse.list" : "echo 'deb [signed-by=/usr/share/keyrings/clickhouse-keyring.gpg arch=arm64] https://packages.clickhouse.com/deb stable main' | sudo tee /etc/apt/sources.list.d/clickhouse.list",
      "sudo apt-get update -qq",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq clickhouse-server=${var.clickhouse_version} clickhouse-client=${var.clickhouse_version} clickhouse-common-static=${var.clickhouse_version}",
      # ------------------------------------------------------------------
      # clickhouse-backup
      # ------------------------------------------------------------------
      "sudo curl -sL 'https://github.com/Altinity/clickhouse-backup/releases/download/v${var.clickhouse_backup_version}/clickhouse-backup-linux-arm64.tar.gz' -o /tmp/chb.tar.gz",
      # v2.5+ release tarballs extract to a clickhouse-backup/ directory
      # (not a bare binary - the bare-binary assumption broke the build).
      "sudo tar xzf /tmp/chb.tar.gz -C /tmp",
      "sudo mv /tmp/build/linux/arm64/clickhouse-backup /usr/local/bin/clickhouse-backup",
      "sudo chmod +x /usr/local/bin/clickhouse-backup",
      "sudo rm -rf /tmp/chb.tar.gz /tmp/build",
      # ------------------------------------------------------------------
      # AWS CLI v2 (bootstrap S3 downloads via instance role)
      # ------------------------------------------------------------------
      "sudo curl -sL 'https://awscli.amazonaws.com/awscli-exe-linux-aarch64.zip' -o /tmp/awscliv2.zip",
      "sudo apt-get install -y -qq unzip",
      "sudo unzip -q -o /tmp/awscliv2.zip -d /tmp/",
      "sudo /tmp/aws/install",
      "sudo rm -rf /tmp/awscliv2.zip /tmp/aws/",
      # ------------------------------------------------------------------
      # CloudWatch agent (node metrics + log shipping)
      # ------------------------------------------------------------------
      "sudo curl -sL 'https://amazoncloudwatch-agent.s3.amazonaws.com/ubuntu/arm64/latest/amazon-cloudwatch-agent.deb' -o /tmp/cw-agent.deb",
      "sudo dpkg -i /tmp/cw-agent.deb",
      "sudo rm /tmp/cw-agent.deb",
      # ------------------------------------------------------------------
      # OpenTelemetry Collector + Fluent Bit (optional remote log shipping;
      # cloud-init only CONFIGURES these when telemetry_config sets an
      # endpoint — pre-installing keeps golden-image boots download-free)
      # ------------------------------------------------------------------
      # -f so an HTTP error (GitHub rate-limit page) FAILS curl instead of
      # piping an HTML error into tar ("not in gzip format" - the exact CI
      # failure); --retry for GitHub asset-download flakiness from runners.
      "sudo curl -fsSL --retry 3 --retry-delay 5 'https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v${var.otel_collector_version}/otelcol-contrib_${var.otel_collector_version}_linux_arm64.tar.gz' -o /tmp/otel.tar.gz",
      "file /tmp/otel.tar.gz | grep -q gzip",
      "sudo tar xzf /tmp/otel.tar.gz -C /tmp",
      "sudo mv /tmp/otelcol-contrib /usr/local/bin/otelcol",
      "sudo rm -f /tmp/otel.tar.gz",
      "sudo chmod +x /usr/local/bin/otelcol",
      # Fluent Bit: GitHub releases carry NO Linux tarballs (v3.2.10 has
      # zero assets - the old tar.gz URL 404'd and broke every build) and
      # the packages.fluentbit.io apt repo currently serves no noble
      # dist. Fluent Bit is the OPTIONAL alternative to otel (the
      # default); a missing binary only disables that mode - cloud-init's
      # install_fluentbit() has its own guarded fallback and simply skips
      # remote shipping when absent. So: try the official one-line
      # installer, and DO NOT fail the build when it is unavailable.
      "if curl -fsSL https://raw.githubusercontent.com/fluent/fluent-bit-docs/master/install/unix/debian-install.sh | sudo sh; then cp -L /opt/fluent-bit/bin/fluent-bit /usr/local/bin/fluent-bit 2>/dev/null || true; else echo 'fluent-bit unavailable - remote shipping via fluentbit mode disabled on this AMI'; fi",
      "command -v fluent-bit >/dev/null 2>&1 || ls /opt/fluent-bit/bin/fluent-bit >/dev/null 2>&1 || true",
      # ------------------------------------------------------------------
      # OS optimizations (THP off + limits) — carried from cloud-init so
      # nodes are tuned even before cloud-init runs; cloud-init re-applies.
      # ------------------------------------------------------------------
      "echo never | sudo tee /sys/kernel/mm/transparent_hugepage/enabled",
      "echo never | sudo tee /sys/kernel/mm/transparent_hugepage/defrag",
      "printf '#!/bin/bash\\necho never > /sys/kernel/mm/transparent_hugepage/enabled\\necho never > /sys/kernel/mm/transparent_hugepage/defrag\\nexit 0\\n' | sudo tee /etc/rc.local",
      "sudo chmod +x /etc/rc.local",
      "echo 'fs.file-max = 2097152' | sudo tee /etc/sysctl.d/99-clickhouse.conf",
      "printf '\\nclickhouse soft nofile 262144\\nclickhouse hard nofile 262144\\nroot soft nofile 262144\\nroot hard nofile 262144\\n' | sudo tee -a /etc/security/limits.conf",
      "sudo mkdir -p /etc/systemd/system/clickhouse-server.service.d",
      "printf '[Service]\\nLimitNOFILE=262144\\n' | sudo tee /etc/systemd/system/clickhouse-server.service.d/limits.conf",
      # ------------------------------------------------------------------
      # chinit — cloud-adaptive node bootstrapper (the cloud-init.sh Go
      # port): replaces the stage-2 bash script AND bootstrap.sh, and —
      # since chinit-v0.2.0 — carries EVERY cluster operation in-process
      # (render, schema/replica sync + gc, keeper mntr/quorum gate,
      # health): the chctl binary is dissolved, one binary serves the
      # node. DOWNLOADED HERE — the LAST install step — so the GoReleaser
      # publish race (every packer/** push fires this build immediately)
      # is absorbed by the ~10 minutes of package installs above; this
      # step polls for whatever publish window remains. Checksum-verified
      # against the release's own SHA256SUMS before install.
      #
      # PRIVATE REPO NOTE: release assets are not anonymously fetchable
      # while the repo is private — CURL_AUTH carries the bearer token
      # from CHINIT_DOWNLOAD_TOKEN (CI: the workflow GITHUB_TOKEN; local
      # builds: `gh auth token`). The token is empty on a public repo.
      # ------------------------------------------------------------------
      "CHINIT_DOWNLOAD_TOKEN=${var.chinit_download_token}",
      "API=\"https://api.github.com/repos/stackgenhq/chinit/releases/tags/v${var.chinit_version}\"",
      "DL=\"https://github.com/stackgenhq/chinit/releases/download/v${var.chinit_version}\"",
      "ok=\"\"; for i in $(seq 1 40); do",
      "  if [ -n \"$CHINIT_DOWNLOAD_TOKEN\" ]; then",
      "    printf 'Authorization: Bearer %s\\n' \"$CHINIT_DOWNLOAD_TOKEN\" > /var/tmp/.ghauth",
      "    chmod 600 /var/tmp/.ghauth",
      "    curl -fsSL --retry 2 -H @/var/tmp/.ghauth \"$API\" -o /var/tmp/rel.json || { echo \"release not visible yet (attempt $i)\"; sleep 30; continue; }",
      "    BIN_ID=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print([a[sys.argv[2]] for a in d[sys.argv[3]] if a[sys.argv[4]]==sys.argv[5]][0])' /var/tmp/rel.json id assets name chinit-linux-arm64)",
      "    SUMS_ID=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print([a[sys.argv[2]] for a in d[sys.argv[3]] if a[sys.argv[4]]==sys.argv[5]][0])' /var/tmp/rel.json id assets name chinit-SHA256SUMS)",
      "    [ -n \"$BIN_ID\" ] && sudo curl -fsSL --retry 2 -H @/var/tmp/.ghauth -H 'Accept: application/octet-stream' -o /var/tmp/chinit \"https://api.github.com/repos/stackgenhq/chinit/releases/assets/$BIN_ID\" && sudo curl -fsSL --retry 2 -H @/var/tmp/.ghauth -H 'Accept: application/octet-stream' -o /var/tmp/chinit-SHA256SUMS \"https://api.github.com/repos/stackgenhq/chinit/releases/assets/$SUMS_ID\" && ok=1",
      "  else",
      "    sudo curl -fsSL --retry 2 -o /var/tmp/chinit \"$DL/chinit-linux-arm64\" && sudo curl -fsSL --retry 2 -o /var/tmp/chinit-SHA256SUMS \"$DL/chinit-SHA256SUMS\" && ok=1",
      "  fi",
      "  [ -n \"$ok\" ] && break",
      "  echo \"chinit v${var.chinit_version} download failed (attempt $i) — release not yet published, or (private repo) no CHINIT_DOWNLOAD_TOKEN; waiting 30s\"; sleep 30",
      "done",
      "[ -n \"$ok\" ] || { echo \"chinit v${var.chinit_version} download never succeeded — check the release exists and CHINIT_DOWNLOAD_TOKEN is set (private repo)\"; exit 1; }",
      "cd /var/tmp && grep linux-arm64 chinit-SHA256SUMS | sed 's|chinit-linux-arm64|chinit|' | sha256sum -c -",
      "sudo install -m 0755 /var/tmp/chinit /usr/local/bin/chinit && sudo rm -f /var/tmp/chinit /var/tmp/chinit-SHA256SUMS",
      # ------------------------------------------------------------------
      # Verification — the image must actually work
      # ------------------------------------------------------------------
      "clickhouse-server --version",
      "clickhouse-client --version",
      "clickhouse-backup --version",
      "aws --version",
      "/usr/local/bin/otelcol --version",
      "ls /opt/fluent-bit/bin/fluent-bit >/dev/null 2>&1 && /opt/fluent-bit/bin/fluent-bit --version || echo 'fluent-bit not present (optional component)'",
      "/usr/local/bin/chinit version | grep -q ${var.chinit_version} || { echo 'chinit version mismatch'; exit 1; }",
    ]
  }

  # Keeper binary is included in the clickhouse-server package.
  post-processor "manifest" {
    output = "manifest.json"
  }
}
