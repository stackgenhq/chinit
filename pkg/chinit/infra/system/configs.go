// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	chinitsvc "github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// ValidateXML checks a ClickHouse config fragment: non-empty, a root
// element the config-merge parser accepts (<clickhouse> / <yandex>), and
// well-formed (the Go XML decoder replaces xmllint --noout; no external
// binary needed).
//
// Why strict: a malformed fragment in config.d makes clickhouse-server
// refuse to start — the bash script validated before install for exactly
// this reason, and a broken downloaded bundle must fail the install
// step loudly instead of poisoning the config directory.
func ValidateXML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return chinitsvc.Fatal(fmt.Errorf("XML file is empty: %s", path))
	}

	// Well-formedness via a token walk (xml.Unmarshal would require a
	// concrete element; fragments have varying roots).
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		if _, err := dec.Token(); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return chinitsvc.Fatal(fmt.Errorf("XML validation failed for %s: %w", path, err))
		}
	}
	if err := ValidateConfigSemantics(path, data); err != nil {
		return err
	}
	return nil
}

// userLevelSettingsAtTopLevel are the settings ClickHouse 26.x rejects
// at the top level of a server config fragment with Code 137
// UNKNOWN_ELEMENT_IN_CONFIG ("user-level setting that should be located
// in users.xml inside <profiles>"). Refusing the class here catches a
// malformed bundle at the boot's install step instead of letting the
// server die in a startup loop.
var userLevelSettingsAtTopLevel = map[string]bool{
	"max_threads":            true,
	"max_concurrent_queries": true, // server-level COUNTERPART exists; the bare setting name is user-level at top level
	"max_memory_usage":       true,
	"max_execution_time":     true,
	"max_insert_block_size":  true,
	"readonly":               true,
	"profile":                true,
}

// serverLevelSettings are server-level settings that resemble merge
// tuning (the background pool family). At the fragment top level they
// are legal; inside <merge_tree> ClickHouse 26.x rejects them with
// Code 115 UNKNOWN_SETTING, so chinit refuses only the
// misplaced-inside-merge_tree form.
var serverLevelSettings = map[string]bool{
	"background_pool_size":                          true,
	"background_merges_mutations_concurrency_ratio": true,
	"background_move_pool_size":                     true,
	"background_fetches_pool_size":                  true,
	"background_schedule_pool_size":                 true,
}

// mergeTreeScopedSettings live in <merge_tree> ONLY. At the top level
// of a config.d fragment 26.x rejects them (Code 115 UNKNOWN_SETTING).
var mergeTreeScopedSettings = map[string]bool{
	"always_fetch_merged_part":                                            true,
	"replicated_can_become_leader":                                        true,
	"number_of_free_entries_in_pool_to_lower_max_size_of_merge":           true,
	"number_of_free_entries_in_pool_to_execute_mutation":                  true,
	"number_of_free_entries_in_pool_to_execute_optimize_entire_partition": true,
}

// ValidateConfigSemantics walks one config fragment's element tree and
// rejects the settings-placement patterns that crash ClickHouse at
// STARTUP — the three fatal classes the convergence audit
// found in shipped config templates (Code 137 user-level settings at
// top level; Code 115 merge_tree-scoped settings outside
// <merge_tree>). The server's own startup check is the last line of
// defense; this is the first — a broken template fails the boot's
// install step ONCE with a pointed error instead of a crash loop.
func ValidateConfigSemantics(path string, data []byte) error {
	var root xmlNode
	if err := xml.Unmarshal(data, &root); err != nil {
		// Token-valid but structurally unmarshalable (mixed content
		// etc.) — not this validator's class; ValidateXML's walk
		// already passed it.
		return nil
	}
	// Fragment roots vary (<clickhouse>, <yandex>) — validate the
	// element's CHILDREN whatever the root name is.
	if err := root.validateChildren(path); err != nil {
		return err
	}
	// Cross-element semantics: a merge_tree free-entry threshold must
	// stay under the background pool size or ClickHouse 26.x refuses to
	// start ("The value of 'number_of_free_entries_in_pool_to_execute_
	// mutation' setting (20) ... is greater than the background pool
	// size"). The values can sit in the SAME fragment or the thresholds
	// alone can ride one (the pool then lives in another fragment or
	// config.xml — the check only fires when BOTH are visible and
	// conflict).
	return root.validatePoolThresholds(path)
}

// poolSettings carry the cross-checked pair: the background pool size
// and the merge_tree free-entry thresholds that must stay under it.
type poolSettings struct {
	backgroundPoolSize int
	thresholds         map[string]int
}

// collectPoolSettings walks the fragment for the background pool size
// (fragment top level) and the free-entry thresholds (<merge_tree>).
// Absent values report -1 (unknown — the check needs both).
func (node *xmlNode) collectPoolSettings(ps *poolSettings) {
	inMergeTree := node.XMLName.Local == "merge_tree"
	for _, child := range node.Children {
		name := child.XMLName.Local
		switch {
		case name == "background_pool_size" && !inMergeTree:
			if v, ok := (&child).childInt(); ok {
				ps.backgroundPoolSize = v
			}
		case strings.HasPrefix(name, "number_of_free_entries_in_pool_to_") && inMergeTree:
			if v, ok := (&child).childInt(); ok {
				ps.thresholds[name] = v
			}
		}
		(&child).collectPoolSettings(ps)
	}
}

// childInt parses an element's text content as an integer.
func (node *xmlNode) childInt() (int, bool) {
	v, err := strconv.Atoi(strings.TrimSpace(node.Text))
	if err != nil {
		return 0, false
	}
	return v, true
}

// validatePoolThresholds enforces floor <= pool for every visible
// free-entry threshold against a fragment-visible pool size.
func (root *xmlNode) validatePoolThresholds(path string) error {
	ps := poolSettings{backgroundPoolSize: -1, thresholds: map[string]int{}}
	root.collectPoolSettings(&ps)
	if ps.backgroundPoolSize < 0 || len(ps.thresholds) == 0 {
		return nil // not both visible in this fragment — nothing to cross
	}
	for name, threshold := range ps.thresholds {
		if threshold >= ps.backgroundPoolSize {
			return chinitsvc.Fatal(fmt.Errorf(
				"%s: <%s> (%d) is at or above <background_pool_size> (%d) — ClickHouse 26.x refuses to start with Code 36 "+
					"\"is greater than the background pool size\" (a reduced pool against the default thresholds 8/20/25; "+
					"scale the thresholds under the pool)",
				path, name, threshold, ps.backgroundPoolSize))
		}
	}
	return nil
}

// xmlNode is a generic element: the fragment tree walk needs names,
// nesting, and (for the pool cross-check) leaf text.
type xmlNode struct {
	XMLName  xml.Name
	Text     string    `xml:",chardata"`
	Children []xmlNode `xml:",any"`
}

func (node *xmlNode) validateChildren(path string) error {
	inMergeTree := node.XMLName.Local == "merge_tree"
	for _, child := range node.Children {
		name := child.XMLName.Local
		// User-level settings belong in users.d (<profiles>), never
		// in a server config fragment (Code 137 at startup).
		if userLevelSettingsAtTopLevel[name] && !node.inProfileTree() {
			return chinitsvc.Fatal(fmt.Errorf(
				"%s: <%s> is a user-level setting in a server config fragment — "+
					"ClickHouse 26.x refuses to start with Code 137 UNKNOWN_ELEMENT_IN_CONFIG "+
					"(move it to the spec's profiles: section; it renders into users.d)",
				path, name))
		}
		// Server-level pool settings INSIDE <merge_tree>: the
		// backup-0 round-two shape — Code 115 at startup.
		if serverLevelSettings[name] && inMergeTree {
			return chinitsvc.Fatal(fmt.Errorf(
				"%s: <%s> is a SERVER-level setting — inside <merge_tree> ClickHouse 26.x "+
					"refuses to start with Code 115 UNKNOWN_SETTING (move it to the fragment top level)",
				path, name))
		}
		// Merge-tree-scoped settings at the fragment top level: the
		// mirror error — Code 115 the other way.
		if mergeTreeScopedSettings[name] && !inMergeTree && !node.inProfileTree() {
			return chinitsvc.Fatal(fmt.Errorf(
				"%s: <%s> belongs inside <merge_tree> — at the fragment top level "+
					"ClickHouse 26.x refuses to start with Code 115 UNKNOWN_SETTING",
				path, name))
		}
		if err := (&child).validateChildren(path); err != nil {
			return err
		}
	}
	return nil
}

// inProfileTree reports whether node is a profiles subtree (users.d
// renders carry these; a setting inside <profiles>/<default> is legal).
func (node *xmlNode) inProfileTree() bool {
	return node.XMLName.Local == "profiles" || node.XMLName.Local == "default" || node.XMLName.Local == "readonly"
}

// StaticConfigInstaller moves the downloaded bundle's static XMLs into
// /etc/clickhouse-server/config.d with validation and ownership, and
// retires the pre-chinit hand-written overlays whose duplicate sections
// would conflict at config-merge time with the rendered files.
type StaticConfigInstaller struct {
	runner Runner
	// Role gates the backup-replica-specific overlay installed from the
	// bundle (the settings file is fetched and hash-covered for every
	// role; only the backup node installs it).
	Role model.Role
}

// NewStaticConfigInstaller builds the installer.
func NewStaticConfigInstaller(runner Runner) *StaticConfigInstaller {
	return &StaticConfigInstaller{runner: runner}
}

// defaultBackupOverlayXML is the backup replica's built-in role
// overlay, installed when the config bundle carries no copy: it tunes
// the node for its sole job of archiving replicated data.
//
// Why: a backup replica neither executes merges nor serves queries, so
// the merge/move/schedule pools shrink to free CPU and memory for part
// fetching and backup uploads. The free-entry thresholds drop under
// the reduced pools because ClickHouse 26.x refuses to start when a
// threshold meets or exceeds its pool (Code 36), and the pools stay at
// the fragment top level because inside <merge_tree> they are rejected
// as unknown settings (Code 115). always_fetch_merged_part and
// replicated_can_become_leader keep the node a pure follower so it can
// never win an election it is not sized to serve.
//
// The fetch pool deliberately stays at the ClickHouse default: fetching
// parts from peers is the node's primary activity, and
// background_fetches_pool_size sets both the thread count and the max
// concurrent task count of the fetch executor — lowering it throttles
// replication convergence behind the fleet.
//
// Without it: the backup runs with writer-sized background pools it
// never uses, and a copy of the overlay must ship in every bundle.
func defaultBackupOverlayXML() string {
	return `<?xml version="1.0"?>
<clickhouse>
    <merge_tree>
        <always_fetch_merged_part>1</always_fetch_merged_part>
        <replicated_can_become_leader>0</replicated_can_become_leader>
        <number_of_free_entries_in_pool_to_lower_max_size_of_merge>1</number_of_free_entries_in_pool_to_lower_max_size_of_merge>
        <number_of_free_entries_in_pool_to_execute_mutation>1</number_of_free_entries_in_pool_to_execute_mutation>
        <number_of_free_entries_in_pool_to_execute_optimize_entire_partition>1</number_of_free_entries_in_pool_to_execute_optimize_entire_partition>
    </merge_tree>
    <background_pool_size>2</background_pool_size>
    <background_merges_mutations_concurrency_ratio>1</background_merges_mutations_concurrency_ratio>
    <background_move_pool_size>2</background_move_pool_size>
    <background_fetches_pool_size>16</background_fetches_pool_size>
    <background_schedule_pool_size>2</background_schedule_pool_size>
</clickhouse>
`
}

// HardwareSizingOverlayXML renders a server-level config.d overlay
// (z_hardware-sizing.xml) proportional to the host's actual vCPU count
// and physical RAM, plus optional dynamic cache <max_size> sizing when
// a dedicated cache disk is mounted at /var/lib/clickhouse/disks.
//
// Why: a single cluster CHI spec is shared across heterogeneous roles
// (e.g. 8-16 vCPU / 64 GiB writers alongside 2-4 vCPU / 8 GiB readers,
// or writers stepping across a vertical type ladder). Static writer-sized
// pools (background_pool_size=16) and caches (mark_cache_size=5 GiB) on a
// 2-vCPU / 4-8 GiB node peg CPU at 100% on background merges and trigger
// Code 241 OOMs, while clamping chi.yaml down to the smallest node wastes
// capacity on scaled-up writers.
//
// Every <merge_tree> free-entry threshold is scaled strictly below
// background_pool_size so ClickHouse 26.x never refuses startup with
// Code 36 when background_pool_size < 20 (the upstream default of
// number_of_free_entries_in_pool_to_execute_mutation).
func HardwareSizingOverlayXML(vcpus int, memBytes uint64, cacheDisks ...CacheDiskSizing) string {
	if vcpus < 1 {
		vcpus = 2
	}
	if memBytes == 0 {
		memBytes = 8 << 30
	}
	bgPool := clampInt(vcpus*2, 4, 32)
	concurrencyRatio := 4
	if vcpus <= 2 {
		concurrencyRatio = 2
	}
	movePool := clampInt(vcpus, 2, 8)
	schedulePool := clampInt(vcpus*2, 8, 16)
	freeEntries := clampInt(bgPool/2, 1, 20)

	markCache := clampUint64(memBytes*15/100, 512<<20, 8<<30)
	uncompressedCache := clampUint64(memBytes*5/100, 256<<20, 2<<30)

	storageBlock := CacheDiskSizings(cacheDisks).renderCacheStorageOverlay()

	return fmt.Sprintf(`<?xml version="1.0"?>
<clickhouse>
    <!-- Rendered by chinit for host hardware (%d vCPU, %d MiB RAM) -->
    <merge_tree>
        <number_of_free_entries_in_pool_to_lower_max_size_of_merge>%d</number_of_free_entries_in_pool_to_lower_max_size_of_merge>
        <number_of_free_entries_in_pool_to_execute_mutation>%d</number_of_free_entries_in_pool_to_execute_mutation>
        <number_of_free_entries_in_pool_to_execute_optimize_entire_partition>%d</number_of_free_entries_in_pool_to_execute_optimize_entire_partition>
    </merge_tree>
    <background_pool_size>%d</background_pool_size>
    <background_merges_mutations_concurrency_ratio>%d</background_merges_mutations_concurrency_ratio>
    <background_move_pool_size>%d</background_move_pool_size>
    <background_schedule_pool_size>%d</background_schedule_pool_size>
    <mark_cache_size>%d</mark_cache_size>
    <uncompressed_cache_size>%d</uncompressed_cache_size>%s
</clickhouse>
`, vcpus, memBytes>>20, freeEntries, freeEntries, freeEntries, bgPool, concurrencyRatio, movePool, schedulePool, markCache, uncompressedCache, storageBlock)
}

// CacheDiskSizing describes the mounted cache filesystem capacity and the
// cache wrapper disk names (e.g. s3_cache, cas_cache) present in storage.xml
// that share the mountpoint.
type CacheDiskSizing struct {
	// MountBytes is the total byte capacity of the mounted cache volume
	// (/var/lib/clickhouse/disks). Zero skips the storage_configuration
	// override so storage.xml's static <max_size> remains in effect.
	MountBytes uint64
	// DiskNames lists the cache wrapper disk names declared in
	// storage.xml (e.g. ["s3_cache"] or ["s3_cache", "cas_cache"]).
	// Defaults to ["s3_cache"] when empty.
	DiskNames []string
}

// CacheDiskSizings is a domain collection of CacheDiskSizing entries.
type CacheDiskSizings []CacheDiskSizing

// CacheMaxSizeBytes computes the per-wrapper <max_size> in bytes (90% of
// the mounted cache volume divided evenly across the active cache disk
// wrappers sharing the mountpoint).
func (c CacheDiskSizing) CacheMaxSizeBytes() uint64 {
	if c.MountBytes == 0 {
		return 0
	}
	count := uint64(len(c.DiskNames))
	if count == 0 {
		count = 1
	}
	return (c.MountBytes * 90 / 100) / count
}

func (sizings CacheDiskSizings) renderCacheStorageOverlay() string {
	if len(sizings) == 0 || sizings[0].MountBytes == 0 {
		return ""
	}
	s := sizings[0]
	maxSize := s.CacheMaxSizeBytes()
	if maxSize == 0 {
		return ""
	}
	names := s.DiskNames
	if len(names) == 0 {
		names = []string{"s3_cache"}
	}
	var b strings.Builder
	b.WriteString("\n    <storage_configuration>\n        <disks>\n")
	for _, name := range names {
		if name == "" {
			continue
		}
		fmt.Fprintf(&b, "            <%s>\n                <max_size>%d</max_size>\n            </%s>\n", name, maxSize, name)
	}
	b.WriteString("        </disks>\n    </storage_configuration>")
	return b.String()
}

// cacheDiskNames returns the <type>cache</type> disk names declared under
// <storage_configuration><disks> in this XML fragment.
func (root *xmlNode) cacheDiskNames() []string {
	var names []string
	for _, child := range root.Children {
		if child.XMLName.Local != "storage_configuration" {
			continue
		}
		for _, sc := range child.Children {
			if sc.XMLName.Local != "disks" {
				continue
			}
			for _, disk := range sc.Children {
				for _, prop := range disk.Children {
					if prop.XMLName.Local == "type" && strings.TrimSpace(prop.Text) == "cache" {
						names = append(names, disk.XMLName.Local)
						break
					}
				}
			}
		}
	}
	return names
}

// detectCacheDiskNames inspects storage.xml bytes for <type>cache</type>
// disks so z_hardware-sizing.xml overrides every cache wrapper sharing
// /var/lib/clickhouse/disks (e.g. s3_cache and cas_cache).
func detectCacheDiskNames(storageXML []byte) []string {
	var root xmlNode
	if err := xml.Unmarshal(storageXML, &root); err != nil {
		return []string{"s3_cache"}
	}
	names := root.cacheDiskNames()
	if len(names) == 0 {
		return []string{"s3_cache"}
	}
	return names
}

// probeCacheMountBytes returns the size in bytes of the dedicated cache
// filesystem mounted at /var/lib/clickhouse/disks, or 0 when unmounted
// or unavailable.
func (i *StaticConfigInstaller) probeCacheMountBytes(ctx context.Context) uint64 {
	const cacheMount = "/var/lib/clickhouse/disks"
	if _, err := i.runner.Run(ctx, "mountpoint", "-q", cacheMount); err != nil {
		return 0
	}
	out, err := i.runner.Run(ctx, "df", "-B1", "--output=size", cacheMount)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return 0
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	n, err := strconv.ParseUint(last, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// HardwareProfilesOverlayXML renders a users.d overlay
// (z_hardware-profiles.xml) sizing external aggregation and sort spill
// thresholds to the host's physical RAM so queries/backfills spill to
// disk before hitting max_server_memory_usage_to_ram_ratio (Code 241).
func HardwareProfilesOverlayXML(memBytes uint64) string {
	if memBytes == 0 {
		memBytes = 8 << 30
	}
	extGroupBy := clampUint64(memBytes*30/100, 512<<20, 16<<30)
	extSort := clampUint64(memBytes*20/100, 512<<20, 8<<30)
	return fmt.Sprintf(`<?xml version="1.0"?>
<clickhouse>
    <profiles>
        <default>
            <max_bytes_before_external_group_by>%d</max_bytes_before_external_group_by>
            <max_bytes_before_external_sort>%d</max_bytes_before_external_sort>
        </default>
    </profiles>
</clickhouse>
`, extGroupBy, extSort)
}

func readMemTotalBytes() uint64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 8 << 30
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if kb, err := strconv.ParseUint(fields[1], 10, 64); err == nil && kb > 0 {
					return kb * 1024
				}
			}
		}
	}
	return 8 << 30
}

func clampInt(v, minV, maxV int) int {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func clampUint64(v, minV, maxV uint64) uint64 {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

// fileExists reports whether the path names an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// WithRole pins the node's role for the role-conditional overlays.
func (i *StaticConfigInstaller) WithRole(role model.Role) *StaticConfigInstaller {
	i.Role = role
	return i
}

// retiredOverlays are files earlier stacks shipped by hand. A lingering
// copy is config drift: chinit renders these sections now, and the merge
// conflicts (duplicate <macros>, <remote_servers>, profiles) break the
// server config parse. Removed on every boot — idempotent by deletion.
var retiredOverlays = []string{
	"/etc/clickhouse-server/config.d/macros.xml",
	"/etc/clickhouse-server/config.d/cluster.xml",
	"/etc/clickhouse-server/users.d/z_users.xml",
	"/etc/clickhouse-server/config.d/settings.xml",
	"/etc/clickhouse-server/config.d/logger.xml",
	"/etc/clickhouse-server/config.d/query-log.xml",
	"/etc/clickhouse-server/config.d/prometheus.xml",
	"/etc/clickhouse-server/users.d/delete_default_user.xml",
}

// staticConfigs maps bundle filenames to their install destinations.
// The network/ssl/storage trio is the static remainder — everything
// else renders from the CHI spec via the render library.
var staticConfigs = map[string]string{
	"network.xml": "/etc/clickhouse-server/config.d/network.xml",
	"ssl.xml":     "/etc/clickhouse-server/config.d/ssl.xml",
	"storage.xml": "/etc/clickhouse-server/config.d/storage.xml",
}

// Install converges the static configs: validate each bundle XML, copy
// to its destination, fix ownership/perms. A missing or invalid bundle
// member is Fatal — the bundle SHA gate should have caught corruption,
// so reaching here with a bad file means the gate was skipped and the
// node must not boot half-configured.
func (i *StaticConfigInstaller) Install(ctx context.Context, bundleDir string) error {
	for _, gone := range retiredOverlays {
		if err := os.Remove(gone); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("retire %s: %w", gone, err)
		}
	}

	for name, dest := range staticConfigs {
		src := filepath.Join(bundleDir, name)
		if err := ValidateXML(src); err != nil {
			return err
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return chinitsvc.Fatal(fmt.Errorf("read bundle config %s: %w", name, err))
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return chinitsvc.Fatal(fmt.Errorf("create %s: %w", filepath.Dir(dest), err))
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return chinitsvc.Fatal(fmt.Errorf("install %s: %w", name, err))
		}
		if _, err := i.runner.Run(ctx, "chown", "clickhouse:clickhouse", dest); err != nil {
			return fmt.Errorf("chown %s: %w", dest, err)
		}
	}

	// backup-replica-settings.xml: the backup's role overlay. The
	// bundle contract NO LONGER carries it (the port-to-
	// chinit): defaultBackupOverlayXML is the single source, rendered
	// here — the installer owns the role gate because it is the only
	// place the node's role and the config installation meet. A legacy
	// bundle that still ships a copy keeps its content (the last
	// bundles pre-0.2.32), validated against the same placement rules.
	if i.Role == model.RoleBackup {
		const brsName = "backup-replica-settings.xml"
		var data []byte
		if src := filepath.Join(bundleDir, brsName); fileExists(src) {
			b, err := os.ReadFile(src)
			if err != nil {
				return chinitsvc.Fatal(fmt.Errorf("read bundle config %s: %w", brsName, err))
			}
			if verr := ValidateXML(src); verr != nil {
				return verr
			}
			data = b
		} else {
			data = []byte(defaultBackupOverlayXML())
		}
		const dest = "/etc/clickhouse-server/config.d/backup-replica-settings.xml"
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return chinitsvc.Fatal(fmt.Errorf("install %s: %w", brsName, err))
		}
		if _, err := i.runner.Run(ctx, "chown", "clickhouse:clickhouse", dest); err != nil {
			return fmt.Errorf("chown %s: %w", dest, err)
		}
	} else {
		// Non-backup roles MUST NOT carry the overlay — a lingering copy
		// on a writer would disable merges on a serving node. Removing
		// it here is the same converge-the-drift discipline as the
		// retiredOverlays block above.
		_ = os.Remove("/etc/clickhouse-server/config.d/backup-replica-settings.xml")
	}

	// Hardware-proportional sizing overlays for active serving roles
	// (writers + readers): lexicographically after chop-generated-*.xml
	// so each node tunes background merge pools, mark/uncompressed caches,
	// and external group-by/sort spill thresholds to its own vCPUs and RAM.
	const hwConfigDest = "/etc/clickhouse-server/config.d/z_hardware-sizing.xml"
	const hwProfileDest = "/etc/clickhouse-server/users.d/z_hardware-profiles.xml"
	if i.Role == model.RoleWriter || i.Role == model.RoleReader {
		vcpus := runtime.NumCPU()
		memBytes := readMemTotalBytes()
		var cacheSizing CacheDiskSizing
		if mountBytes := i.probeCacheMountBytes(ctx); mountBytes > 0 {
			storageBytes, _ := os.ReadFile(filepath.Join(bundleDir, "storage.xml"))
			cacheSizing = CacheDiskSizing{
				MountBytes: mountBytes,
				DiskNames:  detectCacheDiskNames(storageBytes),
			}
		}
		hwXML := []byte(HardwareSizingOverlayXML(vcpus, memBytes, cacheSizing))
		if err := ValidateConfigSemantics(hwConfigDest, hwXML); err != nil {
			return err
		}
		if err := os.WriteFile(hwConfigDest, hwXML, 0644); err != nil {
			return chinitsvc.Fatal(fmt.Errorf("install z_hardware-sizing.xml: %w", err))
		}
		if _, err := i.runner.Run(ctx, "chown", "clickhouse:clickhouse", hwConfigDest); err != nil {
			return fmt.Errorf("chown %s: %w", hwConfigDest, err)
		}

		profXML := []byte(HardwareProfilesOverlayXML(memBytes))
		if err := os.MkdirAll(filepath.Dir(hwProfileDest), 0755); err != nil {
			return chinitsvc.Fatal(fmt.Errorf("create %s: %w", filepath.Dir(hwProfileDest), err))
		}
		if err := ValidateConfigSemantics(hwProfileDest, profXML); err != nil {
			return err
		}
		if err := os.WriteFile(hwProfileDest, profXML, 0644); err != nil {
			return chinitsvc.Fatal(fmt.Errorf("install z_hardware-profiles.xml: %w", err))
		}
		if _, err := i.runner.Run(ctx, "chown", "clickhouse:clickhouse", hwProfileDest); err != nil {
			return fmt.Errorf("chown %s: %w", hwProfileDest, err)
		}
	} else {
		_ = os.Remove(hwConfigDest)
		_ = os.Remove(hwProfileDest)
	}
	return nil
}
