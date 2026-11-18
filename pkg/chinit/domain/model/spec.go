// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model

import (
	"fmt"
	"strconv"
	"strings"
)

// Role is the node's role in the cluster.
type Role string

const (
	RoleWriter Role = "writer"
	RoleReader Role = "reader"
	RoleKeeper Role = "keeper"
	RoleBackup Role = "backup"
)

// IdentityMode selects how the node derives its hostname:
//   - static: derives the hostname from the private IP's last octet.
//   - lease: claims a stable ordinal from the cloud provider's lease table before rendering configuration.
type IdentityMode string

const (
	IdentityStatic IdentityMode = "static"
	IdentityLease  IdentityMode = "lease"
)

// Default host prefixes: the claim-table key and DNS record are
// prefix+ordinal, and inventory (live-claims and static fallback both)
// matches claims by these prefixes when the spec leaves them unset.
const (
	DefaultWriterHostPrefix = "writer-"
	DefaultReaderHostPrefix = "reader-"
)

// BootSpec is the complete, validated input to a node boot.
type BootSpec struct {
	// Cloud, when set, overrides cloud detection (used by packer builds
	// and tests; the booting node normally relies on detection).
	Cloud string `yaml:"cloud,omitempty" json:"cloud,omitempty"`

	// Node identity.
	Role             Role         `yaml:"role"                        json:"role"`
	IdentityMode     IdentityMode `yaml:"identity_mode"               json:"identity_mode"`
	ReaderHostPrefix string       `yaml:"reader_hostname_prefix,omitempty" json:"reader_hostname_prefix,omitempty"`

	// ReaderCount is the readers' baseline count (render-time
	// inventory: the reserved host-name list). In lease mode the
	// ordinal claim ceiling is ReaderMax (the ASG max — scale-out
	// beyond this baseline must be able to claim); ReaderCount stays
	// the compat baseline the pre-max Terraform render relies on.
	ReaderCount int `yaml:"reader_count,omitempty" json:"reader_count,omitempty"`

	// ReaderMax is the ordinal claim ceiling for leased readers = the
	// readers ASG max_size. Readers claim ordinals in [0, ReaderMax).
	// Why: the ASG can scale past ReaderCount, so the ceiling must be
	// the maximum the fleet can ever reach, not today's baseline
	// otherwise a scale-out boots a node that can never claim an
	// identity. Empty/0 for non-leased readers (static inventory uses
	// ReaderCount).
	ReaderMax int `yaml:"reader_max,omitempty" json:"reader_max,omitempty"`

	// WriterHostPrefix is the hostname prefix leased writers claim
	// (writer-0, writer-1, ...). Consumers default it to "writer-" so
	// the Terraform template can omit it. Why a prefix field at all:
	// the claim-table key and DNS record are prefix+ordinal, and tests
	// render both from one place.
	WriterHostPrefix string `yaml:"writer_hostname_prefix,omitempty" json:"writer_hostname_prefix,omitempty"`

	// WriterMax is the ordinal claim ceiling for leased writers = the
	// writers ASG max_size. Writers claim ordinals in [0, WriterMax).
	// Why: CR slots are rendered from fixed ceilings (stable CR names
	// across scaling events), and the claimer needs the bound it may
	// hand out. 0 for static writers (the octet map fixes the ordinal).
	WriterMax int `yaml:"writer_max,omitempty" json:"writer_max,omitempty"`

	// Cluster topology (rendered into the CHI/keeper spec macros).
	ClusterName string `yaml:"cluster_name"             json:"cluster_name"`
	DNSZoneName string `yaml:"dns_zone_name"            json:"dns_zone_name"`

	// WriterCount and WriterNodes are DEPRECATED for lease identity:
	// they remain the source for the static-identity writer path only
	// (static EC2s with fixed IPs, being replaced by the writer ASG).
	// Lease-mode writers derive identity from WriterHostPrefix/WriterMax
	// instead. Retire both fields when the static writer path is gone.
	WriterCount int      `yaml:"writer_count"             json:"writer_count"`
	WriterNodes []string `yaml:"writer_nodes,omitempty"  json:"writer_nodes,omitempty"`
	KeeperNodes []string `yaml:"keeper_nodes,omitempty"   json:"keeper_nodes,omitempty"`

	// Config bundle: the S3/Blob prefix bootstrap.sh downloaded from.
	// chinit fetches it itself now (config_bundle_sha256 pins the exact
	// content: a spec edit rolls the hash and the fleet follows).
	ConfigBundle ConfigBundle `yaml:"config_bundle" json:"config_bundle"`

	// S3 data bucket (the tiered storage's raw disk endpoint root) and
	// the node timezone: the boot writes both into the server's
	// EnvironmentFile (storage.xml reads the endpoint via from_env
	// an unset variable is a START error, Code 36 Empty protocol in
	// the URL, not a warning).
	S3DataBucket string `yaml:"s3_data_bucket,omitempty" json:"s3_data_bucket,omitempty"`
	Timezone     string `yaml:"timezone,omitempty"        json:"timezone,omitempty"`

	// CasEnabled mirrors the Terraform cas_config.enabled opt-in. When
	// true, the server EnvironmentFile pins CH_CAS_SERVER_ROOT_ID to the
	// node's deterministic root id (storage.xml's from_env contract), so
	// every ASG replacement of an ordinal claims the SAME CAS server-root
	// anchor across fresh EBS. Unset/false keeps the /etc payload exactly
	// as before 0.2.1 (no env contract), and CAS disks using from_env
	// resolve per-incarnation roots (empty var → server uuid).
	CasEnabled bool `yaml:"cas_enabled,omitempty" json:"cas_enabled,omitempty"`

	// ClaimerTableName is the provider's ordinal-lease table (lease
	// identity: DynamoDB on AWS, Table storage on Azure). The settle
	// step claims reader-N/writer-N from it.
	ClaimerTableName string `yaml:"claimer_table_name,omitempty" json:"claimer_table_name,omitempty"`

	// Disk labels (mount by LABEL, never device path — NVMe renaming).
	Disks Disks `yaml:"disks" json:"disks"`

	// Secrets to resolve via the provider's SecretSource. Names only;
	// values never enter the spec, the state file or logs.
	Secrets Secrets `yaml:"secrets" json:"secrets"`

	// Network access lists (ClickHouse <networks> sources).
	Network Network `yaml:"network" json:"network"`

	// Backup (clickhouse-backup) — rendered against the provider's
	// object storage; empty bucket = backups disabled.
	Backup Backup `yaml:"backup" json:"backup"`

	// Remote log shipping (optional; empty endpoint = disabled).
	LogShipping LogShipping `yaml:"log_shipping" json:"log_shipping"`

	// Disruption is the fleet's voluntary-disruption budget (the
	// operator's PodDisruptionBudget equivalent): the healthy-node
	// floors the drain/refresh/rollout gates enforce. Optional; zero
	// floors take the safe defaults (writers: 1).
	Disruption DisruptionBudget `yaml:"disruption" json:"disruption"`

	// Hooks is the lifecycle hook list (the operator's
	// spec.reconcile.hooks parity): commands at the boot and drain
	// boundaries. Optional; the boundaries are no-ops without hooks.
	Hooks Hooks `yaml:"hooks" json:"hooks"`

	// Providers append their region/zone here after metadata probing.
	Region string `yaml:"region"         json:"region"`
	Zone   string `yaml:"zone,omitempty" json:"zone,omitempty"`

	// MultiAZ configures multi-AZ replica placement and the required
	// shared-blob cold storage tier ("cas").
	MultiAZ MultiAZ `yaml:"multi_az,omitempty" json:"multi_az,omitempty"`
}

// MultiAZ is the spec's multi-AZ placement policy. The zero value is
// the safe default everywhere: disabled (single-AZ shard placement).
type MultiAZ struct {
	// Enabled mirrors the Terraform-side opt-in; false = single-AZ
	// shard placement (the cross-AZ part-fetch tax never applies).
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// ColdTier names the shared-blob cold tier multi-AZ placement
	// rides on ("cas" = Altinity Antalya CAS). Empty with Enabled
	// fails validation — the pairing IS the contract.
	ColdTier string `yaml:"cold_tier,omitempty" json:"cold_tier,omitempty"`
}

// ConfigBundle locates and pins the node's configuration bundle in the
// provider's object storage.
type ConfigBundle struct {
	Bucket string `yaml:"bucket"     json:"bucket"`
	Prefix string `yaml:"prefix"     json:"prefix"`
	// SHA256 of the bundle contents; empty skips the gate (dev only).
	SHA256 string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
}

// Disks are the volume labels the mount step resolves (label survives
// NVMe device renaming; first boot falls back to the EBS device path).
type Disks struct {
	MetadataLabel string `yaml:"metadata_label" json:"metadata_label"`
	CacheLabel    string `yaml:"cache_label,omitempty"   json:"cache_label,omitempty"`
	KeeperLabel   string `yaml:"keeper_label,omitempty"  json:"keeper_label,omitempty"`
}

// Secrets names the secret-store entries the boot must resolve.
type Secrets struct {
	DefaultPassword  string `yaml:"default_password"           json:"default_password"`
	AdminPassword    string `yaml:"admin_password"             json:"admin_password"`
	WriterPassword   string `yaml:"writer_password,omitempty"   json:"writer_password,omitempty"`
	TLSCertificate   string `yaml:"tls_certificate,omitempty"   json:"tls_certificate,omitempty"`
	TLSPrivateKey    string `yaml:"tls_private_key,omitempty"   json:"tls_private_key,omitempty"`
	TLSCACertificate string `yaml:"tls_ca_certificate,omitempty" json:"tls_ca_certificate,omitempty"`
}

// Network carries the per-role network allow lists and the subnet CIDRs
// the <networks> sections consume.
type Network struct {
	SubnetCIDR                 string   `yaml:"subnet_cidr"                    json:"subnet_cidr"`
	NLBSubnetCIDR              string   `yaml:"nlb_subnet_cidr,omitempty"       json:"nlb_subnet_cidr,omitempty"`
	AdminAllowedNetworks       []string `yaml:"admin_allowed_networks,omitempty"       json:"admin_allowed_networks,omitempty"`
	WriterAllowedNetworks      []string `yaml:"writer_allowed_networks,omitempty"      json:"writer_allowed_networks,omitempty"`
	DefaultUserAllowedNetworks []string `yaml:"default_user_allowed_networks,omitempty" json:"default_user_allowed_networks,omitempty"`
	// SubnetCIDRs are the EXTRA per-AZ nodes subnet CIDRs multi-AZ
	// placement adds (peers in other AZs arrive from those CIDRs;
	// each render allowlists them beside the node's own SubnetCIDR).
	// Empty in the Single-AZ posture (the pre-multi-AZ shape).
	SubnetCIDRs []string `yaml:"subnet_cidrs,omitempty" json:"subnet_cidrs,omitempty"`
}

// NodeSubnetCIDRs answers every nodes-subnet CIDR peer traffic can
// arrive from: the node's own subnet plus the multi-AZ extras
// (deduplicated, the SubnetCIDR first — the Single-AZ shape answers
// exactly the historical one-entry list).
func (s *BootSpec) NodeSubnetCIDRs() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, cidr := range append([]string{s.Network.SubnetCIDR}, s.Network.SubnetCIDRs...) {
		if cidr == "" || seen[cidr] {
			continue
		}
		seen[cidr] = true
		out = append(out, cidr)
	}
	return out
}

// Backup configures clickhouse-backup against provider object storage.
type Backup struct {
	Bucket                string `yaml:"bucket"                             json:"bucket"`
	Prefix                string `yaml:"prefix,omitempty"                   json:"prefix,omitempty"`
	RetentionDays         int    `yaml:"retention_days,omitempty"           json:"retention_days,omitempty"`
	CompressionFormat     string `yaml:"compression_format,omitempty"       json:"compression_format,omitempty"`
	IsBackupReplica       bool   `yaml:"is_backup_replica"                  json:"is_backup_replica"`
	BackupReplicaHostname string `yaml:"backup_replica_hostname,omitempty"  json:"backup_replica_hostname,omitempty"`
	// Schedule is the cron expression ("0 2 * * *") or systemd
	// OnCalendar expression ("*-*-* 02:00:00") for the backup pass.
	// Empty defaults to "*-*-* 02:00:00" (02:00 UTC daily).
	Schedule string `yaml:"schedule,omitempty"  json:"schedule,omitempty"`
	// OnDemand spins the dedicated backup replica up on Schedule and
	// stops it once the backup pass completes (the StatefulSet 0<->1
	// pattern: the ordinal-0 writer wakes backup-0 on Schedule;
	// backup-0 syncs its replica catalog, runs clickhouse-backup, and
	// stops its own instance).
	OnDemand bool `yaml:"on_demand,omitempty" json:"on_demand,omitempty"`
}

// DefaultBackupCalendar is the default systemd OnCalendar schedule for
// clickhouse-backup when Backup.Schedule is unset (02:00 UTC daily).
const DefaultBackupCalendar = "*-*-* 02:00:00"

// CalendarSchedule returns the systemd OnCalendar expression for
// Backup.Schedule, converting 5-field standard cron expressions
// ("0 2 * * *") into systemd calendar syntax ("*-*-* 02:00:00").
func (b Backup) CalendarSchedule() string {
	return CronToCalendar(b.Schedule)
}

// CronToCalendar converts a 5-field POSIX cron expression ("min hour
// dom month dow", e.g. "0 2 * * *" or "30 4 * * 1-5") into a systemd
// OnCalendar specification ("*-*-* 02:00:00" or "Mon..Fri *-*-*
// 04:30:00"). Non-5-field strings (already systemd OnCalendar syntax
// such as "*-*-* 02:00:00", "daily", "hourly") pass through verbatim;
// an empty string returns DefaultBackupCalendar.
func CronToCalendar(expr string) string {
	s := strings.TrimSpace(expr)
	if s == "" {
		return DefaultBackupCalendar
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return s
	}
	minField, hourField, domField, monthField, dowField := fields[0], fields[1], fields[2], fields[3], fields[4]
	calMin := cronTimePart(minField)
	calHour := cronTimePart(hourField)
	calDom := cronDatePart(domField)
	calMonth := cronDatePart(monthField)
	calDow := cronDowPart(dowField)
	dateTime := fmt.Sprintf("*-%s-%s %s:%s:00", calMonth, calDom, calHour, calMin)
	if calDow != "" {
		return calDow + " " + dateTime
	}
	return dateTime
}

func cronTimePart(f string) string {
	if f == "*" {
		return "*"
	}
	if strings.HasPrefix(f, "*/") {
		return "00/" + strings.TrimPrefix(f, "*/")
	}
	if n, err := strconv.Atoi(f); err == nil && n >= 0 && n <= 59 {
		return fmt.Sprintf("%02d", n)
	}
	return strings.ReplaceAll(f, "-", "..")
}

func cronDatePart(f string) string {
	if f == "*" {
		return "*"
	}
	if strings.HasPrefix(f, "*/") {
		return "01/" + strings.TrimPrefix(f, "*/")
	}
	if n, err := strconv.Atoi(f); err == nil && n >= 1 && n <= 31 {
		return fmt.Sprintf("%02d", n)
	}
	return strings.ReplaceAll(f, "-", "..")
}

func cronDowPart(f string) string {
	if f == "*" {
		return ""
	}
	names := map[string]string{
		"0": "Sun", "1": "Mon", "2": "Tue", "3": "Wed",
		"4": "Thu", "5": "Fri", "6": "Sat", "7": "Sun",
	}
	var parts []string
	for _, item := range strings.Split(f, ",") {
		if a, b, ok := strings.Cut(item, "-"); ok {
			if na, oka := names[a]; oka {
				a = na
			}
			if nb, okb := names[b]; okb {
				b = nb
			}
			parts = append(parts, a+".."+b)
			continue
		}
		if n, ok := names[item]; ok {
			parts = append(parts, n)
		} else {
			parts = append(parts, item)
		}
	}
	return strings.Join(parts, ",")
}

// LogShipping is the optional remote log pipeline (OTEL or Fluent Bit).
type LogShipping struct {
	Agent           string `yaml:"agent,omitempty"       json:"agent,omitempty"`
	Endpoint        string `yaml:"endpoint,omitempty"     json:"endpoint,omitempty"`
	Headers         string `yaml:"headers,omitempty"      json:"headers,omitempty"`
	FluentbitOutput string `yaml:"fluentbit_output,omitempty" json:"fluentbit_output,omitempty"`
}

// Validate checks the invariants every role shares. Role-specific shapes
// (readers need reader_count, keepers need keeper_nodes) are validated
// where consumed; this catches the malformed-spec class early, before any
// provider client is built.
func (s *BootSpec) Validate() error {
	switch s.Role {
	case RoleWriter, RoleReader, RoleKeeper, RoleBackup:
	default:
		return &SpecError{Field: "role", Reason: "unknown role " + string(s.Role)}
	}
	switch s.IdentityMode {
	case IdentityStatic, IdentityLease:
	case "":
		return &SpecError{Field: "identity_mode", Reason: "required"}
	default:
		return &SpecError{Field: "identity_mode", Reason: "unknown mode " + string(s.IdentityMode)}
	}
	if s.Role == RoleReader && s.IdentityMode == IdentityLease && s.ReaderMax <= 0 {
		return &SpecError{Field: "reader_max", Reason: "lease identity requires reader_max > 0"}
	}
	if s.Role == RoleWriter && s.IdentityMode == IdentityLease && s.WriterMax <= 0 {
		return &SpecError{Field: "writer_max", Reason: "lease identity for writers requires writer_max > 0"}
	}
	if s.ConfigBundle.Bucket == "" {
		return &SpecError{Field: "config_bundle.bucket", Reason: "required"}
	}
	if s.Disks.MetadataLabel == "" {
		return &SpecError{Field: "disks.metadata_label", Reason: "required (metadata volume must be mounted)"}
	}
	if s.ClusterName == "" {
		return &SpecError{Field: "cluster_name", Reason: "required"}
	}
	if s.Secrets.DefaultPassword == "" {
		return &SpecError{Field: "secrets.default_password", Reason: "required"}
	}
	// Placement contract: a multi-az-enabled spec must name the
	// shared-blob cold tier it relies on ("cas" today). Stock
	// ReplicatedMergeTree fetches parts peer-to-peer, so multi-AZ
	// placement without that tier bills the $0.02/GB cross-AZ
	// transfer on every fetch — Fatal at spec load (the operator
	// fixes the tfvars, not the node).
	if s.MultiAZ.Enabled && s.MultiAZ.ColdTier == "" {
		return &SpecError{Field: "multi_az.cold_tier", Reason: "multi-AZ placement requires shared_blobs cold_tier (\"cas\") — stock peer-to-peer replication across AZs bills $0.02/GB cross-AZ per part fetch"}
	}
	return nil
}

// WriterCeiling is the fixed writer section size of the CR's replica
// list: WriterMax (the ASG max — every ordinal a writer can ever claim)
// when set, else WriterCount (the static baseline). Why a ceiling and
// not count: reader CR slots are rendered at
// writerCeiling+ordinal, so the slot an ordinal maps to must never
// shift when a writer scales in/out — that would re-key every reader's
// CR name mid-flight (the CR-slot stability contract).
func (s *BootSpec) WriterCeiling() int {
	if s.WriterMax > 0 {
		return s.WriterMax
	}
	return s.WriterCount
}

// ReaderCeiling is the readers' enumeration bound for inventory: the
// ASG max when set (a scale-out past ReaderCount must still be listed
// once it claims), else the static baseline ReaderCount.
func (s *BootSpec) ReaderCeiling() int {
	if s.ReaderMax > 0 {
		return s.ReaderMax
	}
	return s.ReaderCount
}

// WriterPrefix is the leased-writer hostname prefix ("writer-").
func (s *BootSpec) WriterPrefix() string {
	if s.WriterHostPrefix != "" {
		return s.WriterHostPrefix
	}
	return DefaultWriterHostPrefix
}

// ReaderPrefix is the leased-reader hostname prefix ("reader-").
func (s *BootSpec) ReaderPrefix() string {
	if s.ReaderHostPrefix != "" {
		return s.ReaderHostPrefix
	}
	return DefaultReaderHostPrefix
}

// SpecError names the spec field that failed validation so the operator
// reading boot logs can fix the tfvars, not the code.
type SpecError struct {
	Field  string
	Reason string
}

func (e *SpecError) Error() string { return "chinit.yaml: " + e.Field + ": " + e.Reason }

// NodeIdentity is the SETTLED identity every later step and unit uses.
// It is snapshotted after the hostname is claimed/derived — never before
// (the pre-claim ip-* capture is the bug class the node-env contract
// killed; the typed struct makes the same guarantee in Go).
type NodeIdentity struct {
	Role        Role   `json:"role"`
	Hostname    string `json:"hostname"`
	InstanceID  string `json:"instance_id"`
	PrivateIP   string `json:"private_ip"`
	Region      string `json:"region"`
	ClusterName string `json:"cluster_name"`
	DNSZone     string `json:"dns_zone"`
	// CRName is the operator CR host name for server nodes ("01-0"),
	// empty for the backup replica (not in the CR) and keepers.
	CRName string `json:"cr_name,omitempty"`
	// CROrdinal is the numeric suffix of CRName (writers/readers), or
	// the keeper's own ordinal; -1 when not applicable.
	CROrdinal int `json:"cr_ordinal"`
	// ZoneID is the private DNS zone (provider cache of the claim-time
	// resolution); the spot drain releases the record through it.
	ZoneID string `json:"zone_id,omitempty"`
	// LeaseTable is the provider's ordinal lease table when the node
	// holds a lease (readers); the drain releases it.
	LeaseTable string `json:"lease_table,omitempty"`
}

// StepStatus is the recorded outcome of one boot step.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// StepResult records one step execution for resume and triage. Duration
// feeds the [TIMING] summary the bash script printed (kept verbatim
// dashboards grep it).
type StepResult struct {
	Name      string     `json:"name"`
	Status    StepStatus `json:"status"`
	Duration  int64      `json:"duration_ms"`
	Error     string     `json:"error,omitempty"`
	StartedAt string     `json:"started_at,omitempty"`
}

// StateFile is the persistent, non-secret boot record. Completed steps
// short-circuit on re-run (convergence); identity is the settled snapshot
// subcommands (schema-sync, spot-watch, …) reload instead of re-deriving.
type StateFile struct {
	Version  int          `json:"version"`
	Identity NodeIdentity `json:"identity"`
	Steps    []StepResult `json:"steps"`
	BootTime string       `json:"boot_time,omitempty"`
}

// FindStep returns the recorded result for a step, or nil when it has
// never run to completion (Pending steps re-run).
func (st *StateFile) FindStep(name string) *StepResult {
	for i := range st.Steps {
		if st.Steps[i].Name == name {
			return &st.Steps[i]
		}
	}
	return nil
}

// RecordStep stores or replaces a step result.
func (st *StateFile) RecordStep(r StepResult) {
	for i := range st.Steps {
		if st.Steps[i].Name == r.Name {
			st.Steps[i] = r
			return
		}
	}
	st.Steps = append(st.Steps, r)
}
