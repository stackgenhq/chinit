// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	chinitsvc "github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// DiskManager mounts the node's data disks by LABEL (never device path
// NVMe renaming on Graviton renames devices between boots; labels and
// fstab LABEL= entries survive).
//
// Convergence (what the bash script re-derived every boot, now probed):
// - label resolves via blkid → device already formatted, just mount
// - no label but a blank device → mkfs.ext4 -L, then mount
// - filesystem present with the wrong label → e2label in place
// - mountpoint already mounted → skip the mount
// - fstab already has LABEL= → skip the append
type DiskManager struct {
	runner Runner
	// Platform selects the first-boot device-candidate set ("azure"
	// mounts by SCSI LUN; empty/aws use the EBS names). The boot wires
	// it from the detected cloud.
	Platform string
	// fsPath exists for tests to simulate /proc/mounts; empty → the
	// mountpoint check shells out to mountpoint(1).
}

// NewDiskManager builds the manager over a runner.
func NewDiskManager(runner Runner) *DiskManager {
	return &DiskManager{runner: runner}
}

// deviceCandidates are the first-boot fallback paths per role, ordered
// per platform (the first existing device wins). On first boot the
// volume is blank — no filesystem, no label — so the label lookup finds
// nothing and the platform's stable device naming is the only hint:
// - AWS: the EBS device name (which the NVMe driver exposes, making
// the raw sd/xvd path unstable — Graviton renames)
// - Azure: the SCSI LUN symlink (stable: lunN follows the VMSS data
// disk's declared LUN, unlike /dev/sd* which reorders)
// The label check still wins when present; candidates only cover
// first-boot formatting.
//
// nvme0n1 joins the metadata/keeper lists (and nvme2n1 stays on cache):
// the NVMe enumeration order is not stable across instance families
// r8g.2xlarge roll enumerated the ROOT disk as
// nvme1n1 and the metadata volume as nvme0n1, so the metadata
// resolver (whose candidate list stopped at nvme1n1) never probed the
// actually-attached volume and aborted the boot with "not found" on
// every r8g node. The partition-table guard is the safety net that
// keeps the OS disk unadoptable: root's GPT signature is skipped, and
// only a BLANK or already-labeled data disk wins.
//
// nvme1n1 and nvme3n1 join the CACHE list (the 2026-11-17 writer-2
// live finding): the enumeration order is unstable per BOOT, not per
// family — writer-2's boot enumerated the ROOT disk as nvme2n1, the
// metadata volume as nvme0n1, and the 32G cache volume as nvme1n1,
// which the cache resolver (nvme2n1 only) never probed. The volume
// stayed raw forever, the cache fell back to a metadata-disk
// subdirectory, and 30G of s3_cache spill consumed the hot tier's
// headroom until Code 243/75/252 stalled the fleet. The same
// partition-table guard protects the root disk here: root carries a
// GPT signature and is skipped, so adding the remaining nvme names
// cannot adopt it.
const (
	awsMetadataCandidates = "/dev/sdf /dev/sdb /dev/nvme0n1 /dev/nvme1n1 /dev/xvdf /dev/xvdb"
	awsCacheCandidates    = "/dev/sdg /dev/sdc /dev/nvme2n1 /dev/nvme1n1 /dev/nvme3n1 /dev/xvdg /dev/xvdc"
	awsKeeperCandidates   = "/dev/sdf /dev/sdb /dev/nvme0n1 /dev/nvme1n1 /dev/xvdf /dev/xvdb"

	azureMetadataCandidates = "/dev/disk/azure/scsi1/lun0"
	azureCacheCandidates    = "/dev/disk/azure/scsi1/lun1"
	azureKeeperCandidates   = "/dev/disk/azure/scsi1/lun2"
)

// nvmePoolDevices enumerates the NVMe block devices probed for size
// comparison and ephemeral instance-store discovery.
var nvmePoolDevices = []string{
	"/dev/nvme0n1",
	"/dev/nvme1n1",
	"/dev/nvme2n1",
	"/dev/nvme3n1",
	"/dev/nvme4n1",
	"/dev/nvme5n1",
	"/dev/nvme6n1",
	"/dev/nvme7n1",
}

// MountedDisk describes what a successful mount produced.
type MountedDisk struct {
	Label  string
	Device string
	Mount  string
}

// ResolveByLabel finds the device carrying LABEL=label, or "" when none.
func (d *DiskManager) ResolveByLabel(ctx context.Context, label string) (string, error) {
	out, err := d.runner.Run(ctx, "blkid", "-l", "-o", "device", "-t", "LABEL="+label)
	return strings.TrimSpace(out), err
}

// resolveFirstExisting returns the first candidate device that exists as
// a block device. Probing via [ -b ] shells test(1); a missing candidate
// simply doesn't match. When the winning candidate is a blank NVMe disk
// whose byte size is known (lsblk > 0), also check the full NVMe pool
// (/dev/nvme0n1..nvme3n1) and pick the LARGEST blank unpartitioned
// non-ephemeral NVMe disk so a flipped Nitro enumeration order (e.g.
// /dev/sdc 32 GiB cache on nvme1n1 and /dev/sdb 64/256 GiB metadata on
// nvme2n1, observed on reader-2 i-091f087bd609b1877) never mounts the
// smaller cache disk as ch-metadata — while NEVER adopting an ephemeral
// local NVMe instance-store disk (Amazon EC2 NVMe Instance Storage /
// Microsoft NVMe Direct Disk) for persistent metadata or keeper roles.
func (d *DiskManager) resolveFirstExisting(ctx context.Context, candidates string, allowEphemeral bool) (string, error) {
	sawNVMe := false
	for _, dev := range strings.Fields(candidates) {
		if _, err := d.runner.Run(ctx, "test", "-b", dev); err != nil {
			continue
		}
		if strings.HasPrefix(dev, "/dev/nvme") {
			sawNVMe = true
		}
		// Never adopt a disk that carries a PARTITION TABLE: on
		// Graviton the NVMe enumeration order is not stable (the
		// t4g.xlarge the ROOT disk enumerated as
		// nvme1n1 and the metadata candidate list adopted it
		// e2label would have run against the root filesystem; only
		// the GPT-signature refusal in e2label saved the boot from
		// corrupting /). A partition table (blkid TYPE=dos/gpt) means
		// the disk belongs to the OS or a prior tenant — data disks
		// for this stack are whole-device filesystems, never
		// partitioned.
		if d.carriesPartitionTable(ctx, dev) {
			continue
		}
		// Ephemeral NVMe instance store (e.g. r8gd/c8gd/i4g 474 GiB+
		// local NVMe) must NEVER be adopted for metadata or keeper
		// volumes even when larger than the EBS metadata disk.
		if !allowEphemeral && d.isEphemeralNVMe(ctx, dev) {
			if dev == "/dev/nvme1n1" && sawNVMe {
				if poolDev := d.bestBlankNVMe(ctx, "", 0, allowEphemeral); poolDev != "" {
					return poolDev, nil
				}
			}
			continue
		}
		if strings.HasPrefix(dev, "/dev/nvme") && !d.hasFilesystem(ctx, dev) {
			if bestSize := d.deviceSizeBytes(ctx, dev); bestSize > 0 {
				if poolDev := d.bestBlankNVMe(ctx, dev, bestSize, allowEphemeral); poolDev != "" {
					return poolDev, nil
				}
			}
		}
		return dev, nil
	}
	return "", nil
}

// bestBlankNVMe scans the Nitro NVMe pool (/dev/nvme0n1..nvme3n1) for the
// largest blank, unpartitioned NVMe disk (excluding ephemeral NVMe when
// allowEphemeral is false).
func (d *DiskManager) bestBlankNVMe(ctx context.Context, currentBest string, currentSize uint64, allowEphemeral bool) string {
	bestDev := currentBest
	bestSize := currentSize
	for _, nvme := range nvmePoolDevices[:4] {
		if nvme == currentBest {
			continue
		}
		if _, err := d.runner.Run(ctx, "test", "-b", nvme); err != nil {
			continue
		}
		if d.carriesPartitionTable(ctx, nvme) || d.hasFilesystem(ctx, nvme) {
			continue
		}
		if !allowEphemeral && d.isEphemeralNVMe(ctx, nvme) {
			continue
		}
		sz := d.deviceSizeBytes(ctx, nvme)
		if sz > bestSize || bestDev == "" {
			bestSize = sz
			bestDev = nvme
		}
	}
	return bestDev
}

// deviceModel returns the hardware model reported by lsblk for a block
// device (e.g. "Amazon Elastic Block Store", "Amazon EC2 NVMe Instance
// Storage", "Microsoft NVMe Direct Disk"), or "" when unavailable.
func (d *DiskManager) deviceModel(ctx context.Context, device string) string {
	out, err := d.runner.Run(ctx, "lsblk", "-d", "-n", "-o", "MODEL", device)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// isEphemeralNVMe reports whether a block device is a local ephemeral
// NVMe instance-store disk (AWS Nitro Instance Storage or Azure NVMe
// Direct Disk). Ephemeral disks are ideal for S3/CAS read-write cache
// (cold cache on VM replacement) and must never back metadata or keeper
// state.
func (d *DiskManager) isEphemeralNVMe(ctx context.Context, device string) bool {
	if !strings.HasPrefix(device, "/dev/nvme") {
		return false
	}
	model := d.deviceModel(ctx, device)
	return strings.Contains(model, "Instance Storage") ||
		strings.Contains(model, "NVMe Direct Disk")
}

// resolveEphemeralCacheDevice discovers blank local ephemeral NVMe
// instance-store disks for the cache volume. When multiple blank
// ephemeral NVMe drives are present (e.g. r8gd.8xlarge, i4g.4xlarge,
// Azure L16s_v3) and mdadm is available, they are striped into a single
// RAID-0 array (/dev/md/ch-cache) to pool full local NVMe capacity and
// IOPS; when a single drive is present, it is used directly.
func (d *DiskManager) resolveEphemeralCacheDevice(ctx context.Context, excludeDevice string) string {
	var ephemerals []string
	var bestDev string
	var bestSize uint64
	for _, nvme := range nvmePoolDevices {
		if nvme == excludeDevice {
			continue
		}
		if _, err := d.runner.Run(ctx, "test", "-b", nvme); err != nil {
			continue
		}
		if d.carriesPartitionTable(ctx, nvme) || d.hasFilesystem(ctx, nvme) {
			continue
		}
		if !d.isEphemeralNVMe(ctx, nvme) {
			continue
		}
		ephemerals = append(ephemerals, nvme)
		if sz := d.deviceSizeBytes(ctx, nvme); sz >= bestSize {
			bestSize = sz
			bestDev = nvme
		}
	}
	if len(ephemerals) == 0 {
		return ""
	}
	if len(ephemerals) == 1 {
		return ephemerals[0]
	}
	// Multiple blank ephemeral NVMe drives: stripe with mdadm RAID-0 when
	// mdadm is installed; fall back to the largest single drive otherwise.
	raidDev := "/dev/md/ch-cache"
	args := append([]string{
		"--create", raidDev,
		"--level=0",
		fmt.Sprintf("--raid-devices=%d", len(ephemerals)),
		"--metadata=1.2",
		"--force",
		"--run",
	}, ephemerals...)
	if _, err := d.runner.Run(ctx, "mdadm", args...); err == nil {
		return raidDev
	}
	if bestDev != "" {
		return bestDev
	}
	return ephemerals[0]
}

// deviceSizeBytes returns the block device's size in bytes via lsblk, or 0
// when unavailable (e.g. unscripted unit-test fakes).
func (d *DiskManager) deviceSizeBytes(ctx context.Context, device string) uint64 {
	out, err := d.runner.Run(ctx, "lsblk", "-b", "-d", "-n", "-o", "SIZE", device)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// carriesPartitionTable reports whether the device's first signature is
// a partition table (dos/gpt) rather than a filesystem — the shape that
// marks root/OS disks and must never be relabeled or formatted.
func (d *DiskManager) carriesPartitionTable(ctx context.Context, device string) bool {
	out, err := d.runner.Run(ctx, "blkid", device)
	if err != nil {
		return false // blank device: safe to format (the first-boot case)
	}
	return strings.Contains(out, `TYPE="dos"`) || strings.Contains(out, `TYPE="gpt"`)
}

// ResolveDevice resolves a disk: label first (repeat boots), then the
// first-boot device-path fallback. Returns "" when neither exists yet
// (the boot step waits and retries — EBS volumes attach asynchronously).
func (d *DiskManager) ResolveDevice(ctx context.Context, label, fallbackCandidates string) (string, error) {
	if byLabel, err := d.ResolveByLabel(ctx, label); err == nil && byLabel != "" {
		return byLabel, nil
	}
	return d.resolveFirstExisting(ctx, fallbackCandidates, false)
}

// ResolveCacheDevice resolves the S3/CAS cache disk: label first (repeat
// boots), then any blank local ephemeral NVMe instance-store disk (or
// RAID-0 stripe across multiple ephemeral NVMe disks), falling back to
// the platform's attached cache volume candidates.
func (d *DiskManager) ResolveCacheDevice(ctx context.Context, label, fallbackCandidates, excludeDevice string) (string, error) {
	if byLabel, err := d.ResolveByLabel(ctx, label); err == nil && byLabel != "" && byLabel != excludeDevice {
		return byLabel, nil
	}
	if ephDev := d.resolveEphemeralCacheDevice(ctx, excludeDevice); ephDev != "" {
		return ephDev, nil
	}
	return d.resolveFirstExisting(ctx, fallbackCandidates, true)
}

// isMounted reports whether the mountpoint is an active mount.
func (d *DiskManager) isMounted(ctx context.Context, mountPoint string) bool {
	_, err := d.runner.Run(ctx, "mountpoint", "-q", mountPoint)
	return err == nil
}

// hasFilesystem reports whether the device carries any filesystem
// (blkid exits 0 when it recognizes a signature).
func (d *DiskManager) hasFilesystem(ctx context.Context, device string) bool {
	_, err := d.runner.Run(ctx, "blkid", device)
	return err == nil
}

// carriesLabel reports whether the device's filesystem has this label.
func (d *DiskManager) carriesLabel(ctx context.Context, device, label string) bool {
	out, err := d.runner.Run(ctx, "blkid", device)
	return err == nil && strings.Contains(out, `LABEL="`+label+`"`)
}

// MountDisk converges one disk to "mounted at mountPoint, in fstab by
// label". Idempotent: re-running on a mounted, fstab'd disk is a no-op.
func (d *DiskManager) MountDisk(ctx context.Context, label, mountPoint, device string) (MountedDisk, error) {
	res := MountedDisk{Label: label, Device: device, Mount: mountPoint}
	if device == "" {
		return res, chinitsvc.Fatal(fmt.Errorf("device not found for %s (label=%s)", mountPoint, label))
	}
	if _, err := d.runner.Run(ctx, "test", "-b", device); err != nil {
		return res, chinitsvc.Fatal(fmt.Errorf("device %s is not a block device (label=%s)", device, label))
	}

	// First-use format, or relabel an existing filesystem that lost its
	// label (both observed in the fleet).
	if !d.hasFilesystem(ctx, device) {
		if _, err := d.runner.Run(ctx, "mkfs.ext4", "-F", "-L", label, device); err != nil {
			return res, fmt.Errorf("format %s with label %s: %w", device, label, err)
		}
	} else if !d.carriesLabel(ctx, device, label) {
		if _, err := d.runner.Run(ctx, "e2label", device, label); err != nil {
			return res, fmt.Errorf("relabel %s to %s: %w", device, label, err)
		}
	}

	// Ensure the mountpoint directory exists.
	if _, err := d.runner.Run(ctx, "mkdir", "-p", mountPoint); err != nil {
		return res, fmt.Errorf("create %s: %w", mountPoint, err)
	}

	// Mount only when not already mounted; resolve by label post-format
	// so the mount does not depend on the unstable device path.
	if !d.isMounted(ctx, mountPoint) {
		resolved, err := d.ResolveByLabel(ctx, label)
		if err != nil || resolved == "" {
			resolved = device
		}
		if _, err := d.runner.Run(ctx, "mount", resolved, mountPoint); err != nil {
			return res, fmt.Errorf("mount %s at %s: %w", resolved, mountPoint, err)
		}
	}

	// fstab by label: append once, never duplicate.
	fstabHas, err := d.runner.Run(ctx, "grep", "-q", "LABEL="+label, "/etc/fstab")
	if err != nil && strings.TrimSpace(fstabHas) == "" {
		// grep -q exits 1 when absent: append the entry.
		if _, err := d.runner.Run(ctx, "sh", "-c",
			fmt.Sprintf("echo 'LABEL=%s %s ext4 defaults,nofail 0 2' >> /etc/fstab", label, mountPoint)); err != nil {
			return res, fmt.Errorf("append LABEL=%s to fstab: %w", label, err)
		}
	}

	return res, nil
}

// candidatesFor returns the platform's first-boot device candidates.
// Azure VMs (detected by the Lun0 device existing) mount by SCSI LUN;
// everything else uses the AWS-shaped EBS names. The LUN path is a
// topology convention, not a provider API — no port needed.
func (d *DiskManager) candidatesFor(platform string) (meta, cache, keeper string) {
	if platform == "azure" {
		return azureMetadataCandidates, azureCacheCandidates, azureKeeperCandidates
	}
	return awsMetadataCandidates, awsCacheCandidates, awsKeeperCandidates
}

// MountAll mounts the role's disks. The metadata disk is Critical (its
// loss makes S3 data an unreadable pile of parts); cache is BestEffort
// (falls back to a metadata-disk subdirectory); keeper is BestEffort
// (keeper runs on default storage when the dedicated disk is missing,
// with an IOPS-contention warning). The caller (boot step) supplies the
// retry policy; this function is one convergent pass.
//
// A metadata disk that is not YET VISIBLE returns a plain (retryable)
// error — never Fatal. EBS volumes attach asynchronously and the first
// boot pass can legitimately run before the attachment surfaces; the
// boot step's bootWait policy (30 attempts, ~1 min) is the wait that
// closes the race. Fatal here would misclassify a transient attach as
// permanent and abort the boot on a healthy instance: a scale-out
// can hit mount_data_disks while a volume is still attaching, and a
// fatal there wedges the ASG instance refresh at half capacity.
// Fatal stays reserved for the cases a retry cannot cure:
// the metadata-disk MOUNT itself failing (corrupt fs, device error).
func (d *DiskManager) MountAll(ctx context.Context, spec model.BootSpec, hostnameSet func()) ([]MountedDisk, error) {
	if d.Platform == "compose" {
		return d.mountComposeDirs(ctx, spec)
	}
	metaCands, cacheCands, keeperCands := d.candidatesFor(d.Platform)

	var mounted []MountedDisk

	// Metadata: must succeed.
	dev, err := d.ResolveDevice(ctx, spec.Disks.MetadataLabel, metaCands)
	if err != nil {
		return mounted, err
	}
	if dev == "" {
		return mounted, fmt.Errorf("metadata disk not found (label=%s): EBS volume not attached yet", spec.Disks.MetadataLabel)
	}
	md, err := d.MountDisk(ctx, spec.Disks.MetadataLabel, "/var/lib/clickhouse", dev)
	if err != nil {
		return mounted, chinitsvc.Fatal(fmt.Errorf("metadata disk: %w", err))
	}
	mounted = append(mounted, md)

	// Cache: optional; prefers ephemeral local NVMe instance store when
	// present, then attached cache volume, and falls back to a
	// subdirectory on the metadata disk.
	if spec.Disks.CacheLabel != "" {
		cacheDev, err := d.ResolveCacheDevice(ctx, spec.Disks.CacheLabel, cacheCands, dev)
		if err != nil {
			return mounted, err
		}
		if cacheDev != "" && cacheDev != dev {
			cd, err := d.MountDisk(ctx, spec.Disks.CacheLabel, "/var/lib/clickhouse/disks", cacheDev)
			if err != nil {
				// BestEffort: cache is re-derivable from S3.
				_ = err // logged by the step; mount falls back below.
				_, _ = d.runner.Run(ctx, "mkdir", "-p", "/var/lib/clickhouse/disks")
			} else {
				mounted = append(mounted, cd)
			}
		} else {
			_, _ = d.runner.Run(ctx, "mkdir", "-p", "/var/lib/clickhouse/disks")
		}
	} else {
		_, _ = d.runner.Run(ctx, "mkdir", "-p", "/var/lib/clickhouse/disks")
	}

	// Keeper: keepers only, dedicated disk for fsync latency isolation.
	if spec.Role == model.RoleKeeper && spec.Disks.KeeperLabel != "" {
		kDev, err := d.ResolveDevice(ctx, spec.Disks.KeeperLabel, keeperCands)
		if err != nil {
			return mounted, err
		}
		if kDev != "" {
			kd, err := d.MountDisk(ctx, spec.Disks.KeeperLabel, "/var/lib/clickhouse-keeper", kDev)
			if err == nil {
				mounted = append(mounted, kd)
			}
		}
		// Both paths need the keeper state directories to exist.
		_, _ = d.runner.Run(ctx, "mkdir", "-p", "/var/lib/clickhouse-keeper/log")
		_, _ = d.runner.Run(ctx, "mkdir", "-p", "/var/lib/clickhouse-keeper/snapshots")
	}

	// Ownership: everything ClickHouse writes belongs to clickhouse.
	if _, err := d.runner.Run(ctx, "chown", "-R", "clickhouse:clickhouse", "/var/lib/clickhouse"); err != nil {
		return mounted, err
	}

	// Final invariant: the metadata mount is active.
	if !d.isMounted(ctx, "/var/lib/clickhouse") {
		return mounted, chinitsvc.Fatal(fmt.Errorf("metadata disk is not mounted at /var/lib/clickhouse"))
	}

	return mounted, nil
}

// WithPlatform pins the first-boot device-candidate selection (the boot
// wires it from the detected cloud; empty = the AWS-shaped defaults).
func (d *DiskManager) WithPlatform(platform string) *DiskManager {
	d.Platform = platform
	return d
}

// mountComposeDirs prepares container-mounted directories (Docker volumes
// or bind mounts) without touching block devices or /etc/fstab.
func (d *DiskManager) mountComposeDirs(ctx context.Context, spec model.BootSpec) ([]MountedDisk, error) {
	dirs := []string{"/var/lib/clickhouse", "/var/lib/clickhouse/disks"}
	mounted := []MountedDisk{
		{Label: spec.Disks.MetadataLabel, Device: "compose-volume", Mount: "/var/lib/clickhouse"},
	}
	if spec.Role == model.RoleKeeper {
		dirs = append(dirs, "/var/lib/clickhouse-keeper/log", "/var/lib/clickhouse-keeper/snapshots")
		mounted = append(mounted, MountedDisk{
			Label:  spec.Disks.KeeperLabel,
			Device: "compose-volume",
			Mount:  "/var/lib/clickhouse-keeper",
		})
	}
	for _, dir := range dirs {
		if _, err := d.runner.Run(ctx, "mkdir", "-p", dir); err != nil {
			return mounted, chinitsvc.Fatal(fmt.Errorf("create %s: %w", dir, err))
		}
	}
	if _, err := d.runner.Run(ctx, "chown", "-R", "clickhouse:clickhouse", "/var/lib/clickhouse"); err != nil {
		return mounted, err
	}
	if spec.Role == model.RoleKeeper {
		if _, err := d.runner.Run(ctx, "chown", "-R", "clickhouse:clickhouse", "/var/lib/clickhouse-keeper"); err != nil {
			return mounted, err
		}
	}
	return mounted, nil
}
