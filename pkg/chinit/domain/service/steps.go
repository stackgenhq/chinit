// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/system"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// ordinalLeaseSeconds is the ordinal-lease duration (45 minutes) used
// by boot claims and refresh renewals so an instance-refresh reboot
// does not expire mid-restart. The periodic lease-renew timer extends
// active leases every 5 minutes.
const ordinalLeaseSeconds = 2700

// Retry policies used across boot pipeline steps.
var (
	// apiRetry retries transient cloud API failures (throttling, DNS, IMDS lag).
	apiRetry = Policy{Attempts: 5, BaseDelay: 2 * time.Second, MaxDelay: 10 * time.Second}
	// bootWait waits on local services or endpoints to become ready.
	bootWait = Policy{Attempts: 30, BaseDelay: 2 * time.Second, MaxDelay: 10 * time.Second}
	// ebsAttachWait waits up to 5 minutes for asynchronous cloud volume attachment on launch.
	ebsAttachWait = Policy{Attempts: 60, BaseDelay: 3 * time.Second, MaxDelay: 10 * time.Second, Deadline: 5 * time.Minute}
	// onceFatal executes once without retry for configuration errors where retrying would mask a bug.
	onceFatal = Policy{Attempts: 1}
	// claimSweep bounds ordinal lease claiming across one full lease window.
	claimSweep = Policy{Attempts: 3, BaseDelay: 5 * time.Second, MaxDelay: 60 * time.Second, Deadline: 16 * time.Minute}
)

// Steps builds the ordered boot pipeline. Phase 1 runs independent
// I/O-bound steps in parallel; Phase 2 runs sequentially to configure
// and start ClickHouse and its maintenance units.
func (b *Boot) Steps(ctx context.Context) ([]Step, error) {
	runner := system.ExecRunner{}
	disks := system.NewDiskManager(runner).WithPlatform(b.Provider.Name())
	tuner := system.NewOSTuner(runner)
	configs := system.NewStaticConfigInstaller(runner).WithRole(b.Spec.Role)

	steps := []Step{
		// 0. Pre-boot lifecycle hooks (the operator's pre-create
		// moment): register the node with external systems before the
		// pipeline touches the machine. A CRITICAL hook failure aborts
		// the boot (the boundary's contract); the identity env carries
		// the settled role facts the hook needs.
		StepFunc("hooks_pre_boot", Critical, onceFatal, func(ctx context.Context) error {
			return RunHooks(ctx, ShellHookRunner{}, &b.Spec.Hooks, model.HookPreBoot, b.hookIdentity(), false)
		}),

		// PHASE 1 (parallel): the boot's wall-clock poles are IO-bound
		// and INDEPENDENT — the claim sweep can legally wait out a
		// predecessor's lease for many minutes (claimSweep's 16-min
		// deadline) while the bundle downloads, EBS attaches and the
		// kernel settings apply. Running them together pays the MAX
		// of the poles instead of the SUM; each child keeps its own
		// retry policy and criticality (a Critical failure cancels
		// the siblings — see ParallelGroup).
		ParallelGroup{Children: []Step{
			// 1. Fetch the config bundle.
			// SHA mismatch inside is Fatal — no retry.
			StepFunc("fetch_config_bundle", Critical, apiRetry, func(ctx context.Context) error {
				_, err := b.Provider.FetchBundle(ctx, b.Spec.ConfigBundle, BootstrapDir)
				return err
			}),

			// 2-3. OS tuning + binary presence. THP/limits are convergent;
			// a missing AMI binary is Fatal (the ASG must replace the node).
			StepFunc("os_optimizations", Critical, onceFatal, func(ctx context.Context) error {
				return tuner.Tune(ctx)
			}),
			StepFunc("require_binaries", Critical, onceFatal, func(ctx context.Context) error {
				// clickhouse binaries: every cloud. The render and the
				// recurring-job subcommands are in-process library calls
				// no chctl binary. The `aws` CLI is AWS-only — an Azure
				// node never has it, and requiring it there would fail every
				// boot fatally.
				bins := []string{"clickhouse-server", "clickhouse-client"}
				if b.Provider.Name() == "aws" {
					bins = append(bins, "aws")
				}
				return tuner.RequireBinaries(ctx, bins)
			}),

			// 4. Mount data disks (metadata Critical; cache/keeper degrade
			// inside). EBS volumes attach asynchronously — ebsAttachWait
			// covers the attach window (the 5-minute patient window: the
			// r8g scale-out wedged at bootWait's ~60s because
			// the metadata volume attached at minute 2; a manual re-run
			// was the only recovery).
			StepFunc("mount_data_disks", Critical, ebsAttachWait, func(ctx context.Context) error {
				_, err := disks.MountAll(ctx, *b.Spec, nil)
				return err
			}),

			// 4b. Record this node's own volumes (writers/readers):
			// the state file carries the IDs the shutdown
			// volume-release path needs when live cloud discovery
			// cannot run mid-teardown. Critical with phase 1's
			// contract (every child the phase depends on); the
			// release path re-discovers live first and falls back
			// to this list only when the cloud API is unreachable.
			StepFunc("track_own_volumes", Critical, apiRetry, func(ctx context.Context) error {
				return b.trackOwnVolumes(ctx)
			}),

			// 6. Identity settles BEFORE any render (the ip-* macro bug:
			// ASG members must claim their ordinal before macros render). The
			// claimer is an optional capability — static identity (keepers,
			// backup — and the interim static-writer path) needs no claim.
			// Concurrent-safety: the claim touches only the provider (the
			// lease table + DNS), sharing no files with the phase siblings.
			StepFunc("settle_identity", Critical, claimSweep, b.settleIdentity),

			// 7. Passwords land BEFORE the render (passwordFrom refs and
			// the distributed-query password files the render consumes).
			// Writes 0600 files under BootstrapDir — distinct names from
			// the bundle's files, so the concurrent fetch races nothing.
			StepFunc("retrieve_passwords", Critical, apiRetry, b.retrievePasswords),

			// 7b. TLS material: server cert/key/CA from the cloud secret
			// store, or self-signed interserver-only when no TLS secrets
			// are configured (acm mode: the NLB terminates client TLS, the
			// interserver port still requires the files to EXIST). Writes
			// only /etc/clickhouse-server/ssl — no phase sibling touches it.
			StepFunc("configure_tls", Critical, apiRetry, b.configureTLS),
		}},

		// PHASE 2 (serial): everything below reads what phase 1 wrote
		// the render needs the identity, the passwords and the bundle;
		// the server start needs the render; the units need the server.

		// 5. Static configs: validate + install + retire overlays
		// (reads the fetched bundle).
		StepFunc("install_static_configs", Critical, onceFatal, func(ctx context.Context) error {
			return configs.Install(ctx, BootstrapDir)
		}),

		// 5b. Peer /etc/hosts entries: short names must resolve
		// locally (systemd-resolved's stub breaks short-name DNS and
		// the replication queue addresses the leader by its SHORT
		// name — Cannot resolve host writer-0 stalls every part
		// fetch). Needs the settled identity (the lease table scan).
		StepFunc("configure_peer_hosts", Critical, onceFatal, b.configurePeerHosts),

		// 7c. Server environment: /etc/default/clickhouse-server + the
		// systemd EnvironmentFile drop-in. storage.xml resolves the S3
		// endpoint via from_env — the variable must be set before the
		// server starts.
		StepFunc("configure_server_env", Critical, onceFatal, b.configureServerEnv),

		// 8. Render the node config — chctl's library, not its CLI.
		StepFunc("render_node_config", Critical, onceFatal, b.renderNodeConfig),

		// 9. Keeper config (keeper nodes only) + state cleanup.
		// CRITICAL for keepers (the step no-ops for every other role):
		// keeper launches with an explicit --config, so without this
		// render it runs whatever stale config the image left behind
		// a misconfigured RAFT member can disrupt the ensemble quorum
		// itself. A BestEffort classification made a keeper-node
		// boot "succeed" while the cluster lost its coordination.
		StepFunc("configure_keeper", Critical, onceFatal, b.configureKeeper),

		// 10. The identity contract file for legacy unit consumers.
		StepFunc("write_node_env", BestEffort, onceFatal, func(ctx context.Context) error {
			if b.Spec.Role == model.RoleKeeper {
				return nil // keepers wrote theirs in configure_keeper
			}
			return b.WriteNodeEnv(NodeEnvPath)
		}),

		// 10b. Install the shutdown-drain unit immediately after identity
		// settles so any termination during or after boot releases the
		// node's ordinal lease and DNS record.
		StepFunc("install_shutdown_drain", Critical, onceFatal, b.installShutdownDrain),

		// 10c. Install the volume-release unit (writers/readers): the
		// dying node deletes its OWN stateless volumes after the drain,
		// inside the same systemd shutdown transaction. Critical for
		// the same reason as the drain unit — a node that boots without
		// the release path re-creates the orphan-volume leak class.
		StepFunc("install_volume_release", Critical, onceFatal, b.installVolumeRelease),

		// 11. Start clickhouse-server (CRITICAL; keepers skip inside).
		StepFunc("start_clickhouse", Critical, onceFatal, b.startClickhouse),

		// 11c. CloudWatch agent (AWS): BEFORE the peer-heavy steps.
		// The 2026-11-18 finding: health_gate's 11-minute retry
		// budget (bootWait x attempts against a fleet mid-refresh,
		// where every peer is cold) ran out BEFORE the tail steps
		// ran, so the CW agent never installed on fresh nodes —
		// 30-min S3Queue lag blind spot at the 30 TB benchmark
		// (no watermark metrics from any node), no CPU/iowait
		// series during the whole rolling refresh window, and a
		// `ch-s3queue-lag` alarm blind for 30+ consecutive
		// minutes. Running configure_cloudwatch immediately after
		// the server starts (no peer needed; it's node-local
		// fetch-config) guarantees every boot has telemetry even
		// when the health gate later retries the whole budget.
		StepFunc("configure_cloudwatch", BestEffort, onceFatal, b.configureCloudWatch),

		// 11a. Storage-loss detection (server nodes): a re-imaged node
		// whose ordinal claims a name with EXISTING Keeper replica
		// registrations and an EMPTY data directory would stall every
		// cross-node fan-out on registrations whose parts no longer
		// exist — until the hourly dead-replica-gc or a manual drop.
		// The detector records the condition in the journal (BestEffort
		// — never fatal: the DDL pass's Code 253 conflict-retry drops
		// the stale registrations and re-creates; the replica-sync
		// timer converges the catch-up).
		StepFunc("detect_storage_loss", BestEffort, onceFatal, b.detectStorageLoss),

		// 11b. Bootstrap schema (server nodes): execute optional cold-start
		// bootstrap-ddl.sql directly via clickhouse-client without parsing SQL.
		StepFunc("bootstrap_schema", BestEffort, apiRetry, b.bootstrapSchema),

		// 11c. Sync schema (server nodes): copy database and table schema
		// from live peers via HostCreateTables, pruning any stale self replica
		// registration on Code 253.
		StepFunc("sync_schema", BestEffort, onceFatal, b.syncSchema),

		// 12. Health gate: NON-FATAL by design — a reader booting while
		// a peer is down (fleet rolling, spot churn) must still join; a
		// fatal gate wedges the ASG in a boot loop.
		StepFunc("health_gate", BestEffort, onceFatal, b.healthGate),

		// 13. Prometheus endpoint reload + verification.
		StepFunc("prometheus_reload", BestEffort, bootWait, b.prometheusReload),

		// 14. Recurring-job units (chinit subcommands as ExecStart).
		// CRITICAL for prod: the units carry the node's identity
		// lifecycle — lease-renew is what keeps the ordinal claim
		// alive (a node that boots WITHOUT it loses its lease 15
		// minutes later and another instance claims the same
		// ordinal: two nodes serving one identity is the worst
		// failure this pipeline can produce), schema-sync is what
		// materializes this node's tables, disk-autoscale is the
		// merge-stall early-warning. A boot that "succeeds" without
		// them is a liability, not a node — the ASG replacement a
		// Critical failure triggers is the remedy.
		StepFunc("install_units", Critical, onceFatal, b.installUnits),

		// 15. Backup (clickhouse-backup config + timer). CRITICAL when
		// the operator EXPLICITLY pinned a backup bucket: a silent
		// no-backup node is a data-loss risk discovered at restore
		// time. The step no-ops cleanly when backups are not
		// configured (bucket empty) or the binary is absent (a
		// deliberate, warned, non-cluster choice).
		StepFunc("configure_backup", Critical, onceFatal, b.configureBackup),

		// 16. Monitoring agents (OTEL/Fluent Bit remote shipping;
		// the AWS CloudWatch agent lives in step 11c so telemetry
		// exists even when the health gate burns its retry budget).
		StepFunc("configure_monitoring", BestEffort, onceFatal, b.configureMonitoring),

		// 17. Spot watcher service (readers). BestEffort stays: the
		// spot notice is an optimization over the shutdown drain +
		// the lease TTL (a missed notice costs a 15-minute ghost
		// window, not identity theft), and spot-capability is a
		// cloud property the boot must not die on.
		StepFunc("install_spot_watch", BestEffort, onceFatal, b.installSpotWatch),

		// 19. boot-finished marker (health verification greps it).
		StepFunc("boot_finished", Critical, onceFatal, func(ctx context.Context) error {
			if err := os.MkdirAll(filepath.Dir(BootFinishedMarker), 0755); err != nil {
				return err
			}
			stamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
			return os.WriteFile(BootFinishedMarker, []byte(stamp), 0644)
		}),

		// 20. Post-boot lifecycle hooks (the operator's HostCreate POST
		// hook): notify, validate, warm caches — the node is fully
		// converged and the hook sees the finished machine.
		StepFunc("hooks_post_boot", BestEffort, onceFatal, func(ctx context.Context) error {
			return RunHooks(ctx, ShellHookRunner{}, &b.Spec.Hooks, model.HookPostBoot, b.hookIdentity(), false)
		}),
	}
	return steps, nil
}

// crNameFromHostname derives the CR slot ("01-<n>") from a settled hostname:
// writers occupy slots 0..WriterMax-1, readers occupy WriterMax..WriterMax+ReaderMax-1,
// and the backup replica takes the first slot after the readers' ceiling
// (the +1 Terraform renders into replicasCount when backup is enabled).
// Returns ok=false for hostnames that carry no CR slot (keepers).
func crNameFromHostname(spec *model.BootSpec, hostname string) (string, int, bool) {
	writerPrefix := spec.WriterHostPrefix
	if writerPrefix == "" {
		writerPrefix = "writer-"
	}
	readerPrefix := spec.ReaderHostPrefix
	if readerPrefix == "" {
		readerPrefix = "reader-"
	}
	switch {
	case strings.HasPrefix(hostname, writerPrefix):
		if n, err := strconv.Atoi(strings.TrimPrefix(hostname, writerPrefix)); err == nil && n >= 0 {
			return fmt.Sprintf("01-%d", n), n, true
		}
	case strings.HasPrefix(hostname, readerPrefix):
		offset := spec.WriterCeiling()
		if n, err := strconv.Atoi(strings.TrimPrefix(hostname, readerPrefix)); err == nil && n >= 0 {
			return fmt.Sprintf("01-%d", offset+n), offset + n, true
		}
	case strings.HasPrefix(hostname, spec.Backup.BackupReplicaHostname) && spec.Backup.BackupReplicaHostname != "":
		// The backup's CR slot: the first after the readers' ceiling
		// the same arithmetic CRName lands for the role.
		ordinal := spec.WriterCeiling() + spec.ReaderCeiling()
		return fmt.Sprintf("01-%d", ordinal), ordinal, true
	}
	return "", 0, false
}

// restoreIdentity is the refresh path's identity settle: take the identity
// the boot persisted in the state file and reuse it. The identity is the
// cluster's name for this node — CR slot in remote_servers/macros, the DNS
// A record, the ReplicatedMergeTree replica path — so a refresh must
// reproduce it exactly or fail loudly. Re-claiming is NOT an option (the
// old flow silently renumbered reader from reader-0/01-2 to
// reader-1/01-3; the CR slot, DNS and replicated metadata disagree on who
// the node is).
//
// Validation by identity mode:
// - lease: the lease row for the persisted hostname must still be held
// by THIS instance (renew proves it without moving the ordinal). If it
// lapsed and a different instance took it, this node has lost its
// claim to the identity and must not serve under it — Fatal (the ASG
// replaces the node).
// - static: re-derive from the private IP exactly as a first boot would
// and compare; a mismatch means the machine was re-provisioned with a
// different offset and must not keep serving the persisted name.
//
// On success the lease is renewed (best-effort TTL top-up), the hostname is
// re-asserted, and the state file identity is refreshed in place (the DNS
// zone id may have been learned since the last settle).
func (b *Boot) restoreIdentity(ctx context.Context) error {
	statePath := ChinitStatePath
	if b.StatePath != "" {
		statePath = b.StatePath
	}
	stored, err := loadBootIdentity(statePath)
	if err != nil {
		return Fatal(err)
	}
	if model.Role(stored.Role) != b.Spec.Role {
		return Fatal(fmt.Errorf("state role %q != spec role %q — refusing to refresh a node whose role changed; re-image instead", stored.Role, b.Spec.Role))
	}
	if stored.InstanceID != b.Meta.InstanceID {
		return Fatal(fmt.Errorf("state instance %s != this instance %s — refusing to refresh under a foreign identity (re-image the node instead)", stored.InstanceID, b.Meta.InstanceID))
	}

	id := model.NodeIdentity{
		Role:        model.Role(stored.Role),
		Hostname:    stored.Hostname,
		InstanceID:  stored.InstanceID,
		PrivateIP:   b.Meta.PrivateIP, // authoritative from metadata, not the (possibly stale) snapshot
		Region:      b.Meta.Region,
		ClusterName: b.Spec.ClusterName,
		DNSZone:     b.Spec.DNSZoneName,
		CRName:      stored.CRName,
		CROrdinal:   stored.CROrdinal,
		ZoneID:      stored.ZoneID,
		LeaseTable:  stored.LeaseTable,
	}

	switch b.Spec.IdentityMode {
	case model.IdentityStatic:
		hostname, ordinal := StaticHostname(b.Meta.PrivateIP)
		if hostname == "" {
			return Fatal(fmt.Errorf("private IP %s does not map to a static role offset (writers +10..19, keepers +20..29, backup +30)", b.Meta.PrivateIP))
		}
		if hostname != stored.Hostname {
			return Fatal(fmt.Errorf("state hostname %q != static derivation %q from IP %s — the node's placement changed; re-image instead of refreshing", stored.Hostname, hostname, b.Meta.PrivateIP))
		}
		id.CROrdinal = ordinal
	case model.IdentityLease:
		if id.LeaseTable == "" {
			return Fatal(fmt.Errorf("state carries no lease table for lease-mode node — refusing to guess; re-image instead"))
		}
		claimer := port.AsOrdinalClaimer(b.Provider)
		if claimer == nil {
			return Fatal(fmt.Errorf("cloud %s has no ordinal-lease capability but spec demands identity_mode: lease", b.Provider.Name()))
		}
		// Renew proves ownership without moving the ordinal: the renew
		// condition pins instance_id, so a renewal succeeds iff WE hold the
		// lease. The STOLEN-LEASE failure (the port's ErrLeaseStolen
		// the provider's conditional-check rejection) is Fatal: do NOT
		// keep serving under a recycled name. A TRANSIENT failure
		// (throttling, network, DynamoDB blip) is a plain error the
		// claimSweep policy retries — only an exhausted sweep fails the
		// refresh. Classifying the transient class Fatal aborted healthy
		// in-place refreshes on one API blip (the same misclassification
		// the disk-attach race carried).
		if err := claimer.RenewLease(ctx, port.RenewRequest{
			Table:        id.LeaseTable,
			Hostname:     id.Hostname,
			InstanceID:   b.Meta.InstanceID,
			LeaseSeconds: ordinalLeaseSeconds,
		}); err != nil {
			if errors.Is(err, port.ErrLeaseStolen) {
				return Fatal(fmt.Errorf("lease renew for %s failed (%w) — another instance holds it now; do NOT keep serving under this name; re-image the node", id.Hostname, err))
			}
			return fmt.Errorf("lease renew for %s failed: %w", id.Hostname, err)
		}
		logger.GetLogger(ctx).Info("identity: persisted lease renewed", "hostname", id.Hostname, "lease_table", id.LeaseTable)
	default:
		return Fatal(fmt.Errorf("unknown identity_mode %q in spec", b.Spec.IdentityMode))
	}

	// CR name: derive it from the HOSTNAME's ordinal rather than from the
	// persisted CROrdinal, whose meaning differs by role (writers store
	// the bare ordinal; readers store the writer-offset slot — Boot.CRName
	// expects the bare ordinal in Identity.CROrdinal). The hostname is the
	// canonical claim; everything else derives from it.
	if crName, ord, ok := crNameFromHostname(b.Spec, id.Hostname); ok {
		if stored.CRName != "" && crName != stored.CRName {
			return Fatal(fmt.Errorf("CR name derived from hostname %q is %q but state says %q — spec topology changed under this node; re-image instead of refreshing", id.Hostname, crName, stored.CRName))
		}
		id.CRName = crName
		id.CROrdinal = ord
	} else if stored.CRName != "" {
		// Keepers/backup: no CR slot; carry the persisted values through.
		id.CRName = stored.CRName
		id.CROrdinal = stored.CROrdinal
	}

	b.Identity = id
	if b.PersistIdentity != nil {
		b.PersistIdentity(id)
	}
	if err := b.setHostname(ctx, id.Hostname); err != nil {
		logger.GetLogger(ctx).Warn("WARNING: could not set system hostname", "hostname", id.Hostname, "err", err)
	}

	// Re-assert DNS (same record the first boot wrote): best-effort, same
	// contract as the boot path.
	if dns := port.AsDNSRegistrar(b.Provider); dns != nil {
		zoneID := id.ZoneID
		if zoneID == "" {
			zoneID, _ = dns.ResolveZoneID(ctx, b.Spec.DNSZoneName)
		}
		if zoneID != "" {
			id.ZoneID = zoneID
			b.Identity = id
			if err := dns.UpsertARecord(ctx, port.ARecordRequest{
				ZoneID:     zoneID,
				ZoneName:   b.Spec.DNSZoneName,
				RecordName: id.Hostname,
				IP:         id.PrivateIP,
				TTL:        60,
			}); err != nil {
				logger.GetLogger(ctx).Warn("WARNING: DNS re-registration failed; boot continues", "hostname", id.Hostname, "err", err)
			} else if b.PersistIdentity != nil {
				b.PersistIdentity(id)
			}
		}
	}
	return nil
}

// HydrateIdentityFromState populates b.Identity from the persisted boot
// state file when present for this instance. Called before the resume
// engine runs so that if settle_identity is skipped ("already done") on a
// restarted boot, downstream steps (configure_keeper, bootstrap_schema,
// configure_server_env) still receive the settled hostname and CR slot.
func (b *Boot) HydrateIdentityFromState(statePath string) {
	if b.Identity.Hostname != "" {
		return
	}
	if statePath == "" {
		statePath = ChinitStatePath
	}
	if stored, err := loadBootIdentity(statePath); err == nil && stored.Hostname != "" && stored.InstanceID == b.Meta.InstanceID {
		b.Identity = model.NodeIdentity{
			Role:        model.Role(stored.Role),
			Hostname:    stored.Hostname,
			InstanceID:  b.Meta.InstanceID,
			PrivateIP:   b.Meta.PrivateIP,
			Region:      b.Meta.Region,
			ClusterName: b.Spec.ClusterName,
			DNSZone:     b.Spec.DNSZoneName,
			CRName:      stored.CRName,
			CROrdinal:   stored.CROrdinal,
			ZoneID:      stored.ZoneID,
			LeaseTable:  stored.LeaseTable,
		}
	}
}

// settleIdentity derives or claims the node identity and registers DNS.
// Static nodes (keepers/backup — and writers while the static path still
// exists) map the IP octet; lease nodes (BOTH ASG roles — readers and,
// since the writer-ASG rollout, writers) claim via the provider's
// OrdinalClaimer. The capability is probed once, and its absence under
// lease identity is Fatal (an ASG member without a claimer would boot
// with the ephemeral ip-* name and break replication identity — the
// exact production).
//
// The claim is role-aware: the claim sweeps ordinals within the ROLE'S
// ASG max (Prefix/Count come from the spec's role fields — writer-
// /WriterMax for writers, reader-/ReaderMax for readers), so a
// scaled-out node claims the first free slot in [0, max) and never
// exceeds the CR slot ceiling the CHI spec reserved.
func (b *Boot) settleIdentity(ctx context.Context) error {
	// Refresh mode: an in-place refresh of a converged node must keep the
	// identity the cluster already knows — re-claiming here would renumber
	// the node out from under ReplicatedMergeTree's replica paths and the
	// DNS A records. The persisted identity is re-validated against the
	// lease table when there is one (a lease that lapsed AND was taken by
	// another instance means we must NOT keep serving under the old name
	// fail loudly; the ASG replaces this node), then reused verbatim.
	if b.Refresh {
		return b.restoreIdentity(ctx)
	}

	b.HydrateIdentityFromState(b.StatePath)

	id := model.NodeIdentity{
		Role:        b.Spec.Role,
		InstanceID:  b.Meta.InstanceID,
		PrivateIP:   b.Meta.PrivateIP,
		Region:      b.Meta.Region,
		ClusterName: b.Spec.ClusterName,
		DNSZone:     b.Spec.DNSZoneName,
		CROrdinal:   -1,
	}

	switch b.Spec.IdentityMode {
	case model.IdentityStatic:
		hostname, ordinal := StaticHostname(b.Meta.PrivateIP)
		if hostname == "" {
			return Fatal(fmt.Errorf("private IP %s does not map to a static role offset (writers +10..19, keepers +20..29, backup +30)", b.Meta.PrivateIP))
		}
		id.Hostname = hostname
		id.CROrdinal = ordinal
		// Keepers' CROrdinal is their own ordinal; server nodes get the
		// CR name from CRName at render time.
	case model.IdentityLease:
		claimer := port.AsOrdinalClaimer(b.Provider)
		if claimer == nil {
			return Fatal(fmt.Errorf("cloud %s has no ordinal-lease capability but spec demands identity_mode: lease", b.Provider.Name()))
		}
		// Role-aware claim parameters: prefix defaults keep pre-writer-ASG
		// spec templates working (the model documents the defaults), and
		// the ceiling is the role's ASG max — the validation contract
		// already requires it for lease identity.
		var prefix string
		var ceiling int
		switch b.Spec.Role {
		case model.RoleWriter:
			prefix = b.Spec.WriterHostPrefix
			if prefix == "" {
				prefix = "writer-"
			}
			ceiling = b.Spec.WriterMax
		case model.RoleReader:
			prefix = b.Spec.ReaderHostPrefix
			if prefix == "" {
				prefix = "reader-"
			}
			ceiling = b.Spec.ReaderMax
		default:
			// Lease identity is an ASG-member property: keepers and the
			// backup replica are static EC2s (raft safety — they are not
			// CR members and must never claim a CR slot).
			return Fatal(fmt.Errorf("identity_mode: lease is only valid for writer/reader roles (got %s)", b.Spec.Role))
		}
		leaseTable := b.Spec.ClaimerTableName
		if leaseTable == "" {
			leaseTable = os.Getenv("CHINIT_LEASE_TABLE")
		}
		// The empty-table blind spot: a lease identity with no table
		// name claims against "" — DynamoDB answers a confusing
		// validation error from inside the ASG's boot loop and the
		// operator has nothing pointing at the rendered spec being
		// incomplete. Fatal, named, immediately.
		if leaseTable == "" {
			return Fatal(fmt.Errorf("lease identity requires claimer_table_name in the spec (or CHINIT_LEASE_TABLE env) — the rendered chinit spec is missing the field"))
		}
		logger.GetLogger(ctx).Info("identity: claiming ordinal lease",
			"hostname_prefix", prefix,
			"claim_ceiling", ceiling,
			"lease_table", leaseTable)
		ordinal, err := claimer.ClaimOrdinal(ctx, port.ClaimRequest{
			Table:      leaseTable,
			Prefix:     prefix,
			Count:      ceiling,
			InstanceID: b.Meta.InstanceID,
			PrivateIP:  b.Meta.PrivateIP,
			// The boot claim carries the LONG window (45 min): an
			// instance-refresh replacement's lease must survive its
			// OWN reboot cycle (stop → new EC2 → boot ≈ 20 min
			// measured on the 2026-11-17 roll). The 15-min window
			// expired mid-reboot, a second node claimed the same
			// ordinal, and the returning node split-brained. The
			// 5-minute renewal timer keeps a live node's claim fresh
			// at the SHORT window (900s) — only a node whose renewer
			// is dead (crash mid-cycle) holds the long one.
			LeaseSeconds:    ordinalLeaseSeconds,
			DeadlineSeconds: 960,
		})
		if err != nil {
			return err
		}
		id.Hostname = fmt.Sprintf("%s%d", prefix, ordinal)
		id.CROrdinal = ordinal
		id.LeaseTable = leaseTable
		logger.GetLogger(ctx).Info("identity: lease claimed",
			"hostname", id.Hostname,
			"ordinal", ordinal,
			"lease_table", leaseTable)
	}

	// The CR host name derives from the settled role + ordinal (writers
	// 01-<ordinal>, readers 01-<writer ceiling + ordinal>; backup and
	// keepers carry none).
	b.Identity = id
	if crName, ord := b.CRName(); crName != "" {
		id.CRName = crName
		id.CROrdinal = ord
	}

	// Set the hostname (static and transient) so macros/replication see
	// the settled name. Best-effort per the bash script (hostnamectl
	// can fail inside cloud-init contexts) but a missing identity is
	// Fatal — this is the cluster's view of who we are.
	b.Identity = id
	if b.PersistIdentity != nil {
		// Persist the settled identity IMMEDIATELY (before renders and
		// the server start): a crash later must not lose it, and the
		// job subcommands read it from the state file.
		b.PersistIdentity(id)
	}
	if err := b.setHostname(ctx, id.Hostname); err != nil {
		// Best-effort (hostnamectl can fail inside cloud-init); the
		// identity itself is settled and persisted.
		logger.GetLogger(ctx).Warn("WARNING: could not set system hostname",
			"hostname", id.Hostname,
			"err", err)
	}

	// DNS registration: optional capability, best-effort with a loud
	// warning (the bash script's `|| echo WARNING`).
	if dns := port.AsDNSRegistrar(b.Provider); dns != nil {
		zoneID, err := dns.ResolveZoneID(ctx, b.Spec.DNSZoneName)
		if err == nil && zoneID != "" {
			id.ZoneID = zoneID
			b.Identity = id
			if err := dns.UpsertARecord(ctx, port.ARecordRequest{
				ZoneID:     zoneID,
				ZoneName:   b.Spec.DNSZoneName,
				RecordName: id.Hostname,
				IP:         id.PrivateIP,
				TTL:        60,
			}); err != nil {
				// Best-effort: registration failure must not fail the
				// boot; the peer-hosts entries carry discovery.
				logger.GetLogger(ctx).Warn("WARNING: DNS registration failed; boot continues",
					"hostname", id.Hostname,
					"err", err)
			} else {
				logger.GetLogger(ctx).Info("identity: DNS A record registered",
					"hostname", id.Hostname,
					"zone_id", zoneID)
			}
		} else if err != nil {
			logger.GetLogger(ctx).Warn("WARNING: zone resolution failed; DNS registration skipped",
				"dns_zone", b.Spec.DNSZoneName,
				"err", err)
		} else {
			logger.GetLogger(ctx).Warn("WARNING: zone resolution returned an empty zone id; DNS registration skipped",
				"dns_zone", b.Spec.DNSZoneName)
		}
	}
	// Persist identity a second time when the DNS block enriched it: the
	// drain path releases the A record through the state's ZoneID, and
	// the first persist (pre-DNS) carried an empty zone id.
	if id.ZoneID != "" && b.PersistIdentity != nil {
		b.PersistIdentity(id)
	}
	return nil
}

// retrievePasswords resolves the boot's secrets into 0600 files the
// render and the post-boot consumers read. Values never enter the
// state file or logs (the file write is the only landing zone).
func (b *Boot) retrievePasswords(ctx context.Context) error {
	secretFiles := map[string]string{
		"default": b.Spec.Secrets.DefaultPassword,
		"admin":   b.Spec.Secrets.AdminPassword,
		"writer":  b.Spec.Secrets.WriterPassword,
	}
	if err := os.MkdirAll(BootstrapDir, 0700); err != nil {
		return Fatal(fmt.Errorf("create %s: %w", BootstrapDir, err))
	}
	for role, secretName := range secretFiles {
		if secretName == "" {
			continue
		}
		value, err := b.Provider.GetSecretValue(ctx, port.SecretRequest{Name: secretName})
		if err != nil {
			return err
		}
		dest := filepath.Join(BootstrapDir, role+"-password")
		if err := os.WriteFile(dest, []byte(value), 0600); err != nil {
			return Fatal(fmt.Errorf("write %s: %w", dest, err))
		}
	}
	return nil
}

// tlsCertDir and clickhouseClientConfigDir are the seams for TLS setup tests.
var (
	tlsCertDir                = func() string { return "/etc/clickhouse-server/ssl" }
	clickhouseClientConfigDir = func() string { return "/etc/clickhouse-client/config.d" }
)

func (b *Boot) selfSignedCertCN() string {
	if b.Identity.Hostname != "" {
		return b.Identity.Hostname
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "localhost"
}

// ensureClientCAConfig installs a clickhouse-client config.d drop-in trusting
// the node's CA alongside system CAs. ClickHouse 26.8+ bare clickhouse-client
// invocations probe both 9000 and 9440 concurrently; without trusting ca.crt,
// every local CLI/exporter/timer call logs `tlsv1 alert unknown ca` on 9440.
func ensureClientCAConfig(ctx context.Context, caCertPath string) error {
	dir := clickhouseClientConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Fatal(fmt.Errorf("create %s: %w", dir, err))
	}
	cfg := fmt.Sprintf(`<config>
    <openSSL>
        <client>
            <caConfig>%s</caConfig>
            <loadDefaultCAFile>true</loadDefaultCAFile>
        </client>
    </openSSL>
</config>
`, caCertPath)
	dest := filepath.Join(dir, "chinit-ca.xml")
	if err := os.WriteFile(dest, []byte(cfg), 0o644); err != nil {
		return Fatal(fmt.Errorf("write %s: %w", dest, err))
	}
	logger.GetLogger(ctx).Info("TLS: configured clickhouse-client CA trust drop-in", "path", dest)
	return nil
}

// configureTLS writes the interserver TLS material the server's
// interserver port (9010) requires: server cert/key + CA. With TLS
// secrets configured, the values come from the cloud secret store;
// without them (acm mode — the NLB terminates client TLS, the
// interserver port is internal) a self-signed pair is generated — the
// bash-era generate_interserver_selfsigned_cert shape. The server
// FAILS TO START when the ssl.xml paths point at files that do not
// exist (Code 210 File not found
// the bash script's configure_tls step had no chinit equivalent).
func (b *Boot) configureTLS(ctx context.Context) error {
	sslDir := tlsCertDir()
	if err := os.MkdirAll(sslDir, 0o755); err != nil {
		return Fatal(fmt.Errorf("create %s: %w", sslDir, err))
	}
	caPath := filepath.Join(sslDir, "ca.crt")

	if b.Spec.Secrets.TLSCertificate == "" {
		// No TLS secrets configured (acm mode): interserver-only
		// self-signed, preserved across reboots (idempotent).
		crt := filepath.Join(sslDir, "server.crt")
		if _, err := os.Stat(crt); err == nil {
			return ensureClientCAConfig(ctx, caPath)
		}
		out, err := runCommand(ctx, "openssl", "req", "-x509", "-newkey", "rsa:2048",
			"-nodes", "-days", "3650",
			"-subj", "/CN="+b.selfSignedCertCN(),
			"-keyout", filepath.Join(sslDir, "server.key"),
			"-out", crt)
		if err != nil {
			return Fatal(fmt.Errorf("generate self-signed interserver cert: %w: %s", err, out))
		}
		if err := os.WriteFile(caPath, mustRead(crt), 0o644); err != nil {
			return Fatal(fmt.Errorf("write ca.crt: %w", err))
		}
		if _, err := runCommand(ctx, "chown", "-R", "clickhouse:clickhouse", sslDir); err != nil {
			return Fatal(err)
		}
		logger.GetLogger(ctx).Info("TLS: generated self-signed cert for interserver TLS (no TLS secrets configured)")
		return ensureClientCAConfig(ctx, caPath)
	}

	material := []struct {
		secret string
		dest   string
		mode   os.FileMode
	}{
		{b.Spec.Secrets.TLSCertificate, filepath.Join(sslDir, "server.crt"), 0o644},
		{b.Spec.Secrets.TLSPrivateKey, filepath.Join(sslDir, "server.key"), 0o600},
		{b.Spec.Secrets.TLSCACertificate, caPath, 0o644},
	}
	for _, m := range material {
		if m.secret == "" {
			continue
		}
		value, err := b.Provider.GetSecretValue(ctx, port.SecretRequest{Name: m.secret})
		if err != nil {
			return err
		}
		if err := os.WriteFile(m.dest, []byte(value), m.mode); err != nil {
			return Fatal(fmt.Errorf("write %s: %w", m.dest, err))
		}
	}
	if _, err := runCommand(ctx, "chown", "-R", "clickhouse:clickhouse", sslDir); err != nil {
		return Fatal(err)
	}
	logger.GetLogger(ctx).Info("TLS: material installed from the cloud secret store")
	return ensureClientCAConfig(ctx, caPath)
}

// mustRead reads a file, panicking on failure (used only on files this
// boot just wrote).
func mustRead(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		panic("mustRead " + path + ": " + err.Error())
	}
	return data
}

// configureServerEnv writes the server's EnvironmentFile (the
// /etc/default/clickhouse-server shape the bash era wrote) and the
// systemd drop-in that loads it. storage.xml's <endpoint
// from_env="CH_S3_ENDPOINT"> is a START requirement on AWS: unset, the
// server dies with Code 36 Empty protocol in the URL (the live
// chinit-cutover — the bash configure_environment_variables
// step had no chinit equivalent). On Azure the storage disk is
// azure_blob_storage with the account name/key INLINE — no endpoint
// env exists to write, so an empty s3_data_bucket is correct there,
// not a spec error; writing the S3 line anyway would hand the server
// a garbage URL (https://.s3...). Credentials NEVER go here: the SDK
// chain reads the instance role.
// probeCacheDiskMaxBytes probes the mounted /var/lib/clickhouse/disks
// volume capacity and returns 90% of its total bytes, or 0 when no
// dedicated cache volume is mounted. Package-level seam for tests.
var probeCacheDiskMaxBytes = defaultProbeCacheDiskMaxBytes

func defaultProbeCacheDiskMaxBytes(ctx context.Context) uint64 {
	const cacheMount = "/var/lib/clickhouse/disks"
	if _, err := runCommand(ctx, "mountpoint", "-q", cacheMount); err != nil {
		return 0
	}
	out, err := runCommand(ctx, "df", "-B1", "--output=size", cacheMount)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return 0
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	n, err := strconv.ParseUint(last, 10, 64)
	if err != nil || n == 0 {
		return 0
	}
	return n * 90 / 100
}

// serverEnvContent builds the /etc/default/clickhouse-server payload
// for the node (pure over the spec + probed cache size). With
// spec.cas_enabled, the deterministic CAS root id rides along — the
// storage.xml from_env contract pairs it with CH_S3_ENDPOINT, so the
// emitted payload always satisfies every from_env reference the config
// can render.
func (b *Boot) serverEnvContent(ctx context.Context) (string, error) {
	if b.Spec.S3DataBucket == "" && b.Provider.Name() == "aws" {
		return "", Fatal(fmt.Errorf("spec s3_data_bucket is empty — storage.xml's endpoint env cannot be set (Code 36 at server start)"))
	}
	region := b.Spec.Region
	if region == "" {
		region = b.Meta.Region
	}
	var sb strings.Builder
	if b.Spec.Timezone != "" {
		fmt.Fprintf(&sb, "CLICKHOUSE_TZ=%s\n", b.Spec.Timezone)
	}
	if b.Spec.S3DataBucket != "" {
		if endpoint := strings.TrimSpace(os.Getenv("CHINIT_S3_ENDPOINT")); b.Provider.Name() == "compose" && endpoint != "" {
			fmt.Fprintf(&sb, "CH_S3_ENDPOINT=%q\n", endpoint)
		} else {
			fmt.Fprintf(&sb, "CH_S3_ENDPOINT=\"https://%s.s3.%s.amazonaws.com\"\n", b.Spec.S3DataBucket, region)
		}
	}
	if cacheMax := probeCacheDiskMaxBytes(ctx); cacheMax > 0 {
		fmt.Fprintf(&sb, "CH_CACHE_MAX_SIZE_BYTES=\"%d\"\n", cacheMax)
	}
	if (b.Spec.CasEnabled || b.Spec.S3DataBucket != "") && b.Identity.Hostname != "" {
		// Always export CH_CAS_SERVER_ROOT_ID whenever the node has an S3 data
		// bucket or CAS enabled: non-CAS storage.xml ignores unused env vars,
		// while CAS storage.xml (<cas_server_root_id from_env="CH_CAS_SERVER_ROOT_ID" />)
		// is guaranteed to receive the deterministic UUID even if cas_enabled
		// was omitted from the role's chinit.yaml (preventing per-boot i-*
		// mount-slot leaks in system.cas_mounts).
		fmt.Fprintf(&sb, "CH_CAS_SERVER_ROOT_ID=\"%s\"\n", b.serverRootID())
	}
	return sb.String(), nil
}

// serverUUIDPath is the seam for tests verifying deterministic server UUID seeding.
var serverUUIDPath = func() string { return "/var/lib/clickhouse/uuid" }

// chStorePath is the state-fulness seam (test redirect; production binds
// the MergeTree store path — its non-emptiness is the CAS anchor signal).
var chStorePath = func() string { return "/var/lib/clickhouse/store" }

// deterministicServerUUID derives a stable RFC-4122 UUID from
// clusterName + "/" + hostname so that an ASG replacement node claiming the
// same ordinal slot (and mounting the same CAS cas_server_root_id in S3)
// presents the exact same server_uuid across ephemeral EBS volumes. Without
// this, ClickHouse generates a random /var/lib/clickhouse/uuid on every fresh
// EBS volume and Altinity Antalya CAS refuses to mount the slot with Code 246
// ("CAS server-root is owned by a different server").
func deterministicServerUUID(clusterName, hostname string) string {
	sum := sha256.Sum256([]byte("chinit-server-uuid:" + clusterName + "/" + hostname))
	sum[6] = (sum[6] & 0x0f) | 0x50 // RFC 4122 version 5
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 variant
	u, err := uuid.FromBytes(sum[:16])
	if err != nil {
		return ""
	}
	return u.String()
}

// serverRootID derives the node's deterministic CAS server-root identity.
func (b *Boot) serverRootID() string {
	cluster := b.Spec.ClusterName
	if cluster == "" {
		cluster = b.Meta.InstanceID
	}
	return deterministicServerUUID(cluster, b.Identity.Hostname)
}

// adoptServerUUID aligns /var/lib/clickhouse/uuid with the deterministic root
// id on stateless boots while preserving existing lineage over stateful stores.
func (b *Boot) adoptServerUUID(ctx context.Context) {
	if b.Spec == nil || b.Spec.Role == model.RoleKeeper || b.Identity.Hostname == "" {
		return
	}
	uuidFile := serverUUIDPath()
	want := b.serverRootID()
	if current, err := os.ReadFile(uuidFile); err == nil {
		got := strings.TrimSpace(string(current))
		if got == want && len(current) == len(want) {
			return
		}
		if b.statefulNode() {
			logger.GetLogger(ctx).Warn("server uuid diverges from the deterministic root id; keeping the existing lineage (CAS claims over data would Code 246)",
				"file", uuidFile, "existing", got, "deterministic", want)
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(uuidFile), 0o750); err != nil {
		logger.GetLogger(ctx).Warn("server uuid seeding skipped", "err", err.Error())
		return
	}
	// ClickHouse's ServerUUID::load runs readUUIDText followed by assertEOF, so
	// a trailing newline triggers Code 27 and resets the file to a random UUIDv4.
	if err := os.WriteFile(uuidFile, []byte(want), 0o640); err != nil {
		logger.GetLogger(ctx).Warn("server uuid seeding skipped", "err", err.Error())
		return
	}
	logger.GetLogger(ctx).Info("seeded the deterministic CAS server uuid", "file", uuidFile, "uuid", want)
	_, _ = runCommand(ctx, "chown", "clickhouse:clickhouse", uuidFile)
}

// statefulNode reports whether the machine carries ClickHouse data state
// (/var/lib/clickhouse/store, the MergeTree write path) from a completed
// boot. Before first_boot_done is written by start_clickhouse, any store/
// directory on root/EBS is image-bake residue that start_clickhouse wipes.
func (b *Boot) statefulNode() bool {
	if chStorePath() == "/var/lib/clickhouse/store" {
		if _, err := os.Stat("/var/lib/clickhouse/first_boot_done"); os.IsNotExist(err) {
			return false
		}
	}
	entries, err := os.ReadDir(chStorePath())
	if err != nil {
		return false
	}
	return len(entries) > 0
}

func (b *Boot) configureServerEnv(ctx context.Context) error {
	content, err := b.serverEnvContent(ctx)
	if err != nil {
		return err
	}
	b.adoptServerUUID(ctx)
	// Nothing to pin (Azure with no timezone and no mounted cache disk):
	// skip the file write AND the drop-in — an EnvironmentFile pointing
	// at nothing is noise, and the step must not touch /etc on a node
	// with no env contract.
	if len(content) == 0 {
		return nil
	}
	envFile := "/etc/default/clickhouse-server"
	if err := os.WriteFile(envFile, []byte(content), 0644); err != nil {
		return Fatal(fmt.Errorf("write %s: %w", envFile, err))
	}
	if _, err := runCommand(ctx, "chown", "clickhouse:clickhouse", envFile); err != nil {
		return Fatal(err)
	}

	dropIn := "/etc/systemd/system/clickhouse-server.service.d/env.conf"
	if err := os.MkdirAll(filepath.Dir(dropIn), 0755); err != nil {
		return Fatal(err)
	}
	if err := os.WriteFile(dropIn, []byte("[Service]\nEnvironmentFile=-/etc/default/clickhouse-server\n"), 0644); err != nil {
		return Fatal(fmt.Errorf("write %s: %w", dropIn, err))
	}
	if _, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return Fatal(err)
	}
	return nil
}

// setHostname sets the node's hostname through the Boot's injectable
// seam: the injected recorder (tests) or the hostnamectl/hostname
// sequence. Kept separate from setSystemHostname so specs can assert
// the settled name never escapes the pipeline.
func (b *Boot) setHostname(ctx context.Context, hostname string) error {
	if b.SetHostname != nil {
		return b.SetHostname(ctx, hostname)
	}
	return setSystemHostname(ctx, hostname)
}

// setSystemHostname sets the node's static + transient hostname so
// ClickHouse macros and Keeper replica paths see the settled cluster
// identity instead of the ephemeral cloud-assigned ip-* name. Both
// modes: --transient alone does not survive a reboot, --static alone
// can fail inside early cloud-init contexts — try both, warn, never
// fail the boot.
func setSystemHostname(ctx context.Context, hostname string) error {
	if _, err := runCommand(ctx, "hostnamectl", "set-hostname", hostname, "--static"); err != nil {
		if _, fallbackErr := runCommand(ctx, "hostname", hostname); fallbackErr != nil {
			return err
		}
	}
	_, _ = runCommand(ctx, "hostnamectl", "set-hostname", hostname, "--transient")
	return nil
}

// configurePeerHosts writes /etc/hosts entries for the cluster's
// SHORT names: the replication queue addresses replica sources by
// their short registered name (the {replica} macro — identity), and
// systemd-resolved's stub resolver does not answer short names
// "Cannot resolve host writer-0" stalls every part fetch (the bash
// era's configure_peer_hosts step, restored for the chinit cutover
// ; reader extended it from writers-only to
// every server node: the reader's fetch queue addressed the writer
// by short name and died on DNS_ERROR while interserver-by-IP
// worked).
//
// Leased-fleet era: peer addresses come from the LIVE claims (the
// same inventory the render uses) — the old code fabricated the
// static EC2 offsets (10.0.1.10+i), which on the writers ASG pinned
// DEAD IPs: the replacement writer inherited the /etc/hosts line of
// its ordinal and every self/peer connection timed out against a
// vacant address (writers-ASG cutover — the hosts
// entry beats DNS). Stale entries from prior boots are removed first
// (the old contains-check kept a wrong entry forever). When a slot
// has no live claim it gets no hosts line — DNS (self-registered at
// claim) is the source of truth, and an absent peer is unreachable
// either way.
//
// The boot step is the FIRST pass, not the only one: the periodic
// convergence (the lease-renew timer's service.ConvergePeerHosts)
// owns the ongoing correctness, because a node booting mid-churn
// sees zero live writer claims and legitimately writes an empty
// hosts file here (0s step, hours of
// DNS_ERROR until a human patched hosts by hand).
func (b *Boot) configurePeerHosts(ctx context.Context) error {
	// Keepers register themselves in Keeper's own host records; the
	// short-name problem is a server-node concern.
	if b.Spec.Role == model.RoleKeeper {
		return nil
	}
	return ConvergePeerHosts(ctx, b.Provider, ConvergeHostsArgs{
		LeaseTable:   b.Identity.LeaseTable,
		WriterPrefix: b.Spec.WriterPrefix(),
		ReaderPrefix: b.Spec.ReaderPrefix(),
		DNSZoneName:  b.Spec.DNSZoneName,
		// The lease-less backup replica: the claim scan never lists
		// it, so its hosts line rides the hostname (the address is
		// resolved at convergence time — Code 198 on every peer
		// resolving its short name for part fetches otherwise).
		BackupReplicaHostname: b.Spec.Backup.BackupReplicaHostname,
	})
}
