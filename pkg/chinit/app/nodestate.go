// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// nodeState holds the persisted identity and boot fingerprint loaded by
// maintenance subcommands from state.json.
type nodeState struct {
	Identity model.NodeIdentity `json:"identity"`
	// SpecHash is the recorded boot fingerprint (the engine's resume
	// key): the config-check job compares it against the fingerprint
	// over freshly-fetched spec+bundle to decide a refresh.
	SpecHash string `json:"spec_hash,omitempty"`
}

// clusterName returns the cluster the node serves (identity snapshot).
func (s *nodeState) clusterName() string { return s.Identity.ClusterName }

// inventorySpec is the topology subset of chinit.yaml specInventory
// needs. It stays a local raw struct (not model.BootSpec) on purpose:
// the subcommand re-reads the spec from disk, and a partial parse keeps
// a role-field rename in the model from breaking the job inventory.
type inventorySpec struct {
	WriterNodes []string `yaml:"writer_nodes"`
	WriterMax   int      `yaml:"writer_max"`
	// WriterCount is parsed-but-DELIBERATELY-IGNORED: the deprecated
	// compat field must never leak into the slot arithmetic (the specs
	// assert a drifted WriterCount cannot move a reader slot).
	WriterCount      int    `yaml:"writer_count"`
	WriterHostPrefix string `yaml:"writer_hostname_prefix"`
	ReaderMax        int    `yaml:"reader_max"`
	ReaderCount      int    `yaml:"reader_count"`
	ReaderHostPrefix string `yaml:"reader_hostname_prefix"`
	DNSZoneName      string `yaml:"dns_zone_name"`
	// ClaimerTableName is the lease table for nodes that hold no lease
	// of their own (the maintenance jobs list it for the writer-GC
	// liveness guard; the state's own LeaseTable wins when present).
	ClaimerTableName string `yaml:"claimer_table_name"`
	// BackupReplicaHostname rides the hosts-file convergence: the
	// backup is a CR member and part-fetch source but holds NO lease,
	// so the claim scan never lists it (the Code 198
	//). Its ADDRESS is derived at convergence time by resolving
	// the FQDN through the private zone (Route53's Terraform-managed
	// record) — never rendered into the spec, so a re-provisioned
	// backup converges without a bundle re-render.
	//
	// NESTED under `backup:` in the rendered spec — the flat field
	// never parsed (the live 2026-11-17 finding: three consecutive
	// chinit releases shipped inventory fixes that no-opped on the
	// node; the binary had the code, the parsed spec stayed empty,
	// and queue-heal on backup-0 kept failing "not in inventory"
	// while its default.events sat on 138 broken parts).
	Backup inventoryBackupSection `yaml:"backup,omitempty"`
	// KeeperNodes is the static keeper identity list (the autoscale
	// coordinator derives keeper-0 from it; keepers hold no leases).
	KeeperNodes []string `yaml:"keeper_nodes,omitempty"`
	// Autoscaling is the vertical-scaling section (the type ladders +
	// the writers' ASG/launch-template identity the autoscale job
	// needs — the fields the vertical-autoscaler Lambda took from its
	// apply-time env, now one spec source of truth).
	Autoscaling inventoryAutoscaling `yaml:"autoscaling,omitempty"`
	// MultiAZ is the compute-placement section (single-AZ shard
	// pinning vs the cross-AZ opt-in — the fleet-wide policy every
	// role's spec carries, not this node's own AZ, which the state's
	// identity snapshot answers).
	MultiAZ inventoryMultiAZ `yaml:"multi_az,omitempty"`
	// ConfigBundle is the spec's own bundle location (the
	// config-drift convergence job fetches the role spec from the
	// same bucket/prefix the boot fetched the bundle from).
	ConfigBundle inventoryBundle `yaml:"config_bundle,omitempty"`
}

// inventoryBackupSection is the spec's `backup:` projection (the
// subset the node-state consumers need — the hostname, schedule, and
// on-demand lifecycle switch; the bucket and retention ride their own
// consumers).
type inventoryBackupSection struct {
	BackupReplicaHostname string `yaml:"backup_replica_hostname,omitempty"`
	Schedule              string `yaml:"schedule,omitempty"`
	OnDemand              bool   `yaml:"on_demand,omitempty"`
}

// backupReplicaHostname answers the backup's hostname wherever the
// rendered spec carries it — the `backup:` section in the current
// bundle shape (nil-receiver-safe: a spec without the section answers
// "" and every consumer no-ops, the static-node shape).
func (s *inventorySpec) backupReplicaHostname() string {
	if s == nil {
		return ""
	}
	return s.Backup.BackupReplicaHostname
}

// backupOnDemand answers whether the dedicated backup replica runs in
// on-demand (StatefulSet 0<->1) mode.
func (s *inventorySpec) backupOnDemand() bool {
	if s == nil {
		return false
	}
	return s.Backup.OnDemand
}

// inventoryBundle is the config_bundle projection (the location, not
// the SHA — the spec-sync job compares the spec BYTES, and the boot's
// gate owns the hash).
type inventoryBundle struct {
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
}

// inventoryAutoscaling is the spec's autoscaling projection.
type inventoryAutoscaling struct {
	// KeeperLadder/WriterLadder are the ascending instance types.
	KeeperLadder []string `yaml:"keeper_type_ladder,omitempty"`
	KeeperMin    string   `yaml:"keeper_min_type,omitempty"`
	KeeperMax    string   `yaml:"keeper_max_type,omitempty"`
	WriterLadder []string `yaml:"writer_type_ladder,omitempty"`
	WriterMin    string   `yaml:"writer_min_type,omitempty"`
	WriterMax    string   `yaml:"writer_max_type,omitempty"`
	// WritersASG is the fleet identity for the vertical roll (the
	// launch template is resolved live from the ASG at decision time —
	// a template change between spec renders must not wedge the roll).
	WritersASG string `yaml:"writers_asg_name,omitempty"`
}

// inventoryMultiAZ is the spec's `multi_az:` projection — the fleet's
// compute-placement policy (NOT this node's own AZ, which lives in the
// state's identity snapshot). Absent section = single-AZ shard
// placement, the pre-placement bundle shape (nil-receiver-safe).
type inventoryMultiAZ struct {
	Enabled  bool   `yaml:"enabled,omitempty"`
	ColdTier string `yaml:"cold_tier,omitempty"`
}

// ladder answers the role's type ladder + bounds.
func (a inventoryAutoscaling) ladder(role string) (ladder []string, minT, maxT string) {
	switch role {
	case "keeper":
		return a.KeeperLadder, a.KeeperMin, a.KeeperMax
	case "writer":
		return a.WriterLadder, a.WriterMin, a.WriterMax
	}
	return nil, "", ""
}

// hostInventory renders the CR-name → address pairs for the chctl
// cluster operations. Pure over the parsed spec subset so the slot
// arithmetic has its own tests and A3's live-claims discovery can
// reuse the exact same arithmetic from a different enumeration source.
//
// CR-name arithmetic (the contract CRName lands): writers 01-0..
// 01-(writerCeiling-1), readers 01-{writerCeiling+i} — the writer slot
// CEILING (writer_max = the writers ASG max), never writer
// count, so scaling writers never re-keys reader CR slots. The static
// fallback (writer_max unset → the WriterNodes list length) covers the
// interim static-writer path; A3's live-claims discovery replaces the
// enumeration SOURCE with active leases, not this slot arithmetic.
func hostInventory(spec inventorySpec) []string {
	if spec.WriterHostPrefix == "" {
		spec.WriterHostPrefix = "writer-"
	}
	if spec.ReaderHostPrefix == "" {
		spec.ReaderHostPrefix = "reader-"
	}
	// writerCeiling: the writer slot ceiling — writer_max when the
	// writers ASG owns the role, else the legacy WriterNodes list
	// length (static writer specs have no writer_max).
	writerCeiling := spec.WriterMax
	if writerCeiling == 0 {
		writerCeiling = len(spec.WriterNodes)
	}
	var out []string
	// Writers: leased specs enumerate the reserved ordinals
	// (writer-i.zone — the spec's writer_hostname_prefix); static specs
	// keep the explicit WriterNodes list.
	if spec.WriterMax > 0 {
		for i := 0; i < writerCeiling; i++ {
			name := fmt.Sprintf("%s%d", spec.WriterHostPrefix, i)
			out = append(out, fmt.Sprintf("01-%d=%s.%s", i, name, spec.DNSZoneName))
		}
	} else {
		for i, name := range spec.WriterNodes {
			out = append(out, fmt.Sprintf("01-%d=%s.%s", i, name, spec.DNSZoneName))
		}
	}
	// Readers: the ASG's reserved ordinals, offset by the writer
	// ceiling (the same slot arithmetic CRName lands; reader_max is
	// the ceiling since the ASG can scale past the baseline count).
	readerCeiling := spec.ReaderMax
	if readerCeiling == 0 {
		readerCeiling = spec.ReaderCount
	}
	for i := 0; i < readerCeiling; i++ {
		name := fmt.Sprintf("%s%d", spec.ReaderHostPrefix, i)
		out = append(out, fmt.Sprintf("01-%d=%s.%s", writerCeiling+i, name, spec.DNSZoneName))
	}
	// The backup replica: a CR member at the FIRST slot after the
	// readers' ceiling — 01-{writerCeiling + readerCeiling} — the
	// CRName arithmetic the boot settles for the role (the 0.2.26
	// contract: an empty CRName meant no host-group render, and the
	// server crash-looped). Its own queue-heal needs the inventory to
	// carry the CR slot — chctl's WalkHosts registers hosts by their
	// CR name, so a hostname-keyed entry never matches (the live
	// 2026-11-17 finding: the 0.2.41 attempt appended "backup-0" and
	// queue-heal still failed "host backup-0 not in inventory" —
	// the runner's host() resolves by CR name OR short FQDN label,
	// and the CR-walk only registers the CR-named form).
	if hostname := spec.backupReplicaHostname(); hostname != "" {
		slot := writerCeiling + readerCeiling
		out = append(out, fmt.Sprintf("01-%d=%s.%s", slot, hostname, spec.DNSZoneName))
	}
	return out
}

// loadInventorySpec reads the bundle's chinit.yaml into the topology
// subset (the legacy path's enumeration source). A missing or
// unparsable file yields an error: the callers treat it as "no
// inventory" (chctl schema sync fails with its own "host inventory
// is empty" error — never a panic here).
//
// inventorySpecPath is the seam the tests redirect (a package-level
// func variable, not a flag: the unit files' ExecStart contract owns
// the CLI surface and the tests run without /var/lib privileges).
var inventorySpecPath = func() string { return service.BootstrapDir + "/chinit.yaml" }

func loadInventorySpec() (*inventorySpec, error) {
	data, err := os.ReadFile(inventorySpecPath())
	if err != nil {
		return nil, err
	}
	var spec inventorySpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// writerCeiling is the writer slot ceiling (writer_max, falling back
// to the static WriterNodes list length — pre-max specs). The live
// path needs the offset but not the full enumeration; WriterCount is
// the last-resort legacy fallback. Nil-receiver-safe: never called
// that way today, but the nil contract is the caller's.
func (is *inventorySpec) writerCeiling() int {
	if is == nil {
		return -1
	}
	if is.WriterMax > 0 {
		return is.WriterMax
	}
	if n := len(is.WriterNodes); n > 0 {
		return n
	}
	return is.WriterCount
}

// readerCeiling is the reader slot ceiling (reader_max, falling back
// to reader_count). Nil-receiver-safe.
func (is *inventorySpec) readerCeiling() int {
	if is == nil {
		return -1
	}
	if is.ReaderMax > 0 {
		return is.ReaderMax
	}
	return is.ReaderCount
}

// writerPrefix defaults the leased-writer hostname prefix.
// Nil-receiver-safe (see writerCeiling).
func (is *inventorySpec) writerPrefix() string {
	if is == nil || is.WriterHostPrefix == "" {
		return model.DefaultWriterHostPrefix
	}
	return is.WriterHostPrefix
}

// readerPrefix defaults the leased-reader hostname prefix.
// Nil-receiver-safe (see writerCeiling).
func (is *inventorySpec) readerPrefix() string {
	if is == nil || is.ReaderHostPrefix == "" {
		return model.DefaultReaderHostPrefix
	}
	return is.ReaderHostPrefix
}

// claimRoleOrdinal classifies a claim hostname into its ASG role and
// the ordinal it holds (writer-2 → writer, 2) — the shared input to
// every CR-slot mapping (claimsHostFlags, the drain's peer selection).
// Foreign rows (backup-0, keeper-N) and malformed ordinals answer
// false: a reserved slot they never had means no CR name either.
// Writer prefix wins, mirroring the claim keys' no-overlap contract.
func claimRoleOrdinal(hostname, writerPrefix, readerPrefix string) (model.Role, int, bool) {
	switch {
	case strings.HasPrefix(hostname, writerPrefix):
		if n, err := strconv.Atoi(strings.TrimPrefix(hostname, writerPrefix)); err == nil && n >= 0 {
			return model.RoleWriter, n, true
		}
	case strings.HasPrefix(hostname, readerPrefix):
		if n, err := strconv.Atoi(strings.TrimPrefix(hostname, readerPrefix)); err == nil && n >= 0 {
			return model.RoleReader, n, true
		}
	}
	return "", -1, false
}

// claimOrdinalFromHostname extracts the trailing ordinal from a leased
// hostname (writer-3 -> 3) without knowing the prefix — the lease-renew
// self-heal re-claims the node's OWN ordinal and only needs the number.
// -1 answers "no ordinal" (backup-0, keeper-N, malformed).
func claimOrdinalFromHostname(hostname string) int {
	i := strings.LastIndexAny(hostname, "-")
	if i < 0 || i == len(hostname)-1 {
		return -1
	}
	n, err := strconv.Atoi(hostname[i+1:])
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// claimCRSlot maps one live claim to its CR slot ordinal — THE slot
// arithmetic hostInventory, claimsHostFlags and the drain's peer
// selection all key on: writers occupy 01-<ordinal> bounded by the
// writer slot CEILING (writer_max — a stale claim from a shrunk max
// reserves no slot), readers offset by that ceiling
// (01-<ceiling+ordinal>). Why one helper: the inventory flags and the
// drain's peer mapping must never disagree about which CR slot a
// hostname lives in. -1 = no slot (a negative writerCeiling means the
// topology is unknown, so reader slots cannot be computed — never
// guessed).
func claimCRSlot(role model.Role, ordinal, writerCeiling int) int {
	switch role {
	case model.RoleWriter:
		if writerCeiling <= 0 || ordinal < writerCeiling {
			return ordinal
		}
	case model.RoleReader:
		if writerCeiling >= 0 {
			return writerCeiling + ordinal
		}
	}
	return -1
}

// claimsHostFlags renders ACTIVE lease rows as chctl -host flags with
// the same CR-name arithmetic hostInventory uses (writers 01-{ordinal};
// readers 01-{writerCeiling+ordinal} — the contract CRName lands),
// so schema-sync's replica set and remote_servers never disagree.
// Sorted by CR slot (the lease-table Scan order is unordered) and
// includes THIS node (self is a member). Claims at or beyond a role's
// ceiling are stale rows from a shrunk max — the CR reserves no slot
// for them, so they are dropped. A negative writerCeiling (-1: the
// spec lacks writer topology) skips the writer ordinal bound check
// (no writer claims can exist in that stack anyway).
func claimsHostFlags(claims []port.Claim, dnsZone string, writerCeiling int, writerPrefix, readerPrefix string) []string {
	type flag struct {
		slot string
		host string
	}
	var out []flag
	for _, claim := range claims {
		role, ordinal, ok := claimRoleOrdinal(claim.Hostname, writerPrefix, readerPrefix)
		if !ok {
			continue // foreign row (backup-0, expired other-role key)
		}
		slotOrdinal := claimCRSlot(role, ordinal, writerCeiling)
		if slotOrdinal < 0 {
			continue // stale row from a shrunk ceiling — no CR slot
		}
		out = append(out, flag{slot: fmt.Sprintf("01-%d", slotOrdinal), host: claim.Hostname})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].slot < out[j].slot })
	flags := make([]string, 0, len(out))
	for _, f := range out {
		flags = append(flags, f.slot+"="+f.host+"."+dnsZone)
	}
	return flags
}

// specInventory loads the node's chinit.yaml and renders the CR-name →
// address pairs the chctl cluster operations need as -host flags.
//
// Two sources, in precedence order:
// 1. LIVE claims: when this node holds a lease (identity.LeaseTable
// the same table the boot claimed from) and the cloud can list it,
// the inventory is the ACTIVE claims — the same contract
// remote_servers renders, so schema-sync never targets a
// reserved-but-vacant slot (an unreachable host fails every
// fan-out).
// 2. Static spec lists (legacy path): the parsed chinit.yaml
// (never the state: the state carries identity, not topology)
// keepers, backup nodes, clouds without the listing capability and
// a lease-table outage all land here, with the outage degraded
// loudly in the log.
//
// The provider is built from the registry (the subcommand's own entry
// into the factory — the boot's provider instance died with its
// process). If the cloud cannot be named from the state the static
// path answers rather than erroring: a missing provider name means a
// very old state file, and the job still has a spec to enumerate.
func (s *nodeState) specInventory() []string {
	return s.specInventoryWithSource(func(cloud string) (port.LeaseLister, string) {
		provider, err := registry().Build(cloud, port.Config{Region: s.Identity.Region})
		if err != nil {
			return nil, "unknown provider " + cloud
		}
		return port.AsLeaseLister(provider), provider.Name()
	})
}

// specInventoryWithSource is specInventory with the lister factory
// injected (tests substitute a fake; production builds the registry
// provider). The factory returns the capability plus the provider's
// display name for the warning paths.
//
// The bundle spec is read FIRST in both paths: path needs the
// writer ceiling from it (reader slots are ceiling-offset), and the
// legacy path enumerates it outright — so a node whose spec cannot be
// read gets the same loud empty inventory either way (chctl's own
// "host inventory is empty" is the visible error, never a guessed
// slot layout).
func (s *nodeState) specInventoryWithSource(listerFor func(cloud string) (port.LeaseLister, string)) []string {
	spec, _ := loadInventorySpec()

	// The live path: the SETTLED identity's LeaseTable is the table the
	// boot claimed from (empty for static nodes — the static lists are
	// their source).
	if table := s.Identity.LeaseTable; table != "" && listerFor != nil {
		if lister, cloudName := listerFor(providerNameOf(s)); lister != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if spec == nil {
				// The lease is live but the spec (the writer ceiling)
				// is not readable: reader slots cannot be computed, and
				// the legacy enumeration below is equally empty
				// degrade loudly instead of guessing slot offsets.
				logger.GetLogger(ctx).Warn("WARNING: this node holds a lease but the bundle spec is unreadable; -host inventory is empty (re-run chinit boot)",
					"hostname", s.Identity.Hostname)
				return nil
			}
			claims, err := lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: table})
			if err == nil {
				return claimsHostFlags(claims, s.Identity.DNSZone, spec.writerCeiling(), spec.writerPrefix(), spec.readerPrefix())
			}
			// A lease-table outage falls back to static — the hourly
			// job must still run (a possibly-vacant list beats no run),
			// and the warning makes the degraded source visible.
			logger.GetLogger(ctx).Warn("WARNING: live-claims discovery failed; inventory enumerated from static spec (may include vacant slots)",
				"cloud", cloudName,
				"err", err)
		} else {
			logger.GetLogger(context.Background()).Warn("WARNING: cloud has no lease-listing capability; inventory enumerated from static spec (may include vacant slots)",
				"cloud", cloudName)
		}
	}

	if spec == nil {
		return nil
	}
	return hostInventory(*spec)
}

// dnsZone returns the cluster's private DNS zone.
func (s *nodeState) dnsZone() string { return s.Identity.DNSZone }

// loadNodeState reads /var/lib/chinit/state.json.
func loadNodeState(path string) (*nodeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fail.Fatal(fmt.Errorf("read %s: %w (did `chinit boot` complete?)", path, err))
	}
	var st nodeState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fail.Fatal(fmt.Errorf("parse %s: %w", path, err))
	}
	if st.Identity.Hostname == "" {
		return nil, fail.Fatal(fmt.Errorf("%s carries no settled identity — run `chinit boot` first", path))
	}
	return &st, nil
}

// adminPassword reads the boot's admin password file. chctl's CLI has
// no -password-file flag (plain -password only); the value is read
// here so the file stays 0600 and the chctl argv carries it like every
// other credential-bearing CLI call. The path seam keeps tests off
// the real /var/lib.
func adminPassword() string {
	return adminPasswordAt(adminPasswordPath())
}

// adminPasswordPath is the admin password file seam (tests redirect).
var adminPasswordPath = func() string { return service.BootstrapDir + "/admin-password" }

// adminPasswordAt reads one password file, trimming the newline.
func adminPasswordAt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// instanceIDOf extracts the EC2 instance id from the identity (the
// AWS provider stores it with an i- prefix).
func instanceIDOf(st *nodeState) string {
	return st.Identity.InstanceID
}
