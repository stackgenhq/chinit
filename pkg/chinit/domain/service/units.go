// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/altinity/clickhouse-operator/pkg/model/chi/backupconfig"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/system"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// chinitUnit templates the recurring-job units. ExecStart is always
// chinit itself — the subcommands share the boot's provider registry,
// state file and secrets surface, so the era of embedded bash in
// ExecStart is over. Identity arrives via the state file (the
// subcommands reload it), never via sed tokens.
func (b *Boot) chinitUnit(name, description, args string) system.UnitFile {
	return system.UnitFile{
		Name: name,
		Content: fmt.Sprintf(`[Unit]
Description=%s
After=network-online.target clickhouse-server.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/chinit %s
TimeoutStartSec=600

[Install]
WantedBy=multi-user.target
`, description, args),
	}
}

// chinitTimer wraps a unit with a systemd timer.
func chinitTimer(name, description, calendar string) system.UnitFile {
	return system.UnitFile{
		Name: name,
		Content: fmt.Sprintf(`[Unit]
Description=%s

[Timer]
OnCalendar=%s
Persistent=true

[Install]
WantedBy=timers.target
`, description, calendar),
	}
}

// installUnits writes the recurring-job units (server nodes; keepers
// only carry the keeper-health cron):
// - replica-sync: oneshot per boot (SYSTEM SYNC REPLICA on every
// replicated table — operator HostSyncTables parity)
// - schema-sync: oneshot per boot + hourly safety net (operator
// ClusterSchemer parity, -prune-stale-self for ASG replacements)
// - lease-renew: hourly (lease-mode nodes — readers AND writers; the
// 15-min TTL gets a 5-min cadence inside)
// - keeper-health: keepers
// - dead-replica-gc / orphaned-data-gc: writers (the service gates
// itself on the ordinal-0 holder — every writer installs the
// units, whichever machine holds writer-{prefix}0 does the work)
//
// chinitUnits is the pure assembler (the matrix's test seam); this
// method only installs what it returns.
// unitFiles is a domain collection of systemd unit definitions.
type unitFiles []system.UnitFile

// names returns the unit filenames in order.
func (units unitFiles) names() []string {
	out := make([]string, 0, len(units))
	for _, u := range units {
		out = append(out, u.Name)
	}
	return out
}

// isBootStartService reports whether the service unit should be started
// once immediately at boot in addition to its recurring timer.
func isBootStartService(name string) bool {
	switch name {
	case "disk-autoscale-boot.service", "dead-replica-gc.service", "queue-heal.service", "lease-renew.service":
		return true
	default:
		return false
	}
}

func (b *Boot) chinitUnits() unitFiles {
	var units unitFiles
	switch b.Spec.Role {
	case model.RoleKeeper:
		// Self-healing: keeper-health runs every minute (the bash-era
		// cron's cadence) — keeper_up datapoints older than 2 periods
		// are exactly what keeper_down's evaluation window keys on. The
		// blind window: the service installed without its
		// timer, so the probe never ran again after boot.
		units = append(units,
			b.chinitUnit("keeper-health.service", "ClickHouse Keeper ensemble health probe", "keeper-health"),
			chinitTimer("keeper-health.timer", "Probe keeper health and publish keeper_up every minute", "*-*-* *:*:00"),
			// The keeper's config-drift convergence pair: keeper.yaml
			// rides the bundle and the keeper's role spec changes
			// (the ensemble's identity sections) converge the same
			// way every server role's do.
			b.chinitUnit("spec-sync.service", "Role-spec drift convergence (config refresher's replacement)", "spec-sync"),
			chinitTimer("spec-sync.timer", "Pull the role spec and converge on drift", "*-*-* *:00/10:00"),
			// The keeper VERTICAL autoscale coordinator (the Lambda's
			// keeper path replacement): keeper-0 self-resizes when the
			// ensemble's own signals breach (max latency,
			// outstanding requests, own CPU). The resize is
			// stop→modify→start — keeper_up dips one tick by design.
			// Non-0 keepers no-op exit 0 (keeper-0 is a STATIC
			// identity; no lease election applies).
			b.chinitUnit("autoscale.service", "Vertical autoscale coordinator (keeper-0 self-resize)", "autoscale"),
			chinitTimer("autoscale.timer", "Vertical autoscale decision tick", "*-*-* *:0/15:00"),
		)
	case model.RoleWriter, model.RoleReader:
		units = append(units,
			b.chinitUnit("replica-sync.service", "Post-boot replica synchronization (SYSTEM SYNC REPLICA)", "replica-sync"),
			// The config-convergence watchdog: re-fetch spec+bundle on a
			// timer, re-render on drift (boot --refresh restarts the
			// server only when a rendered file actually changed). A
			// config change lands in the bucket and every node
			// converges on its own — nothing pushes.
			b.chinitUnit("config-check.service", "Config-convergence watchdog (re-render on drift)", "config-check"),
			chinitTimer("config-check.timer", "Check config drift every 15 minutes", "*-*-* *:00/15:00"),
			b.chinitUnit("schema-sync.service", "ClickHouse schema auto-propagation from cluster peers", "schema-sync -retries 3 -prune-stale-self"),
			chinitTimer("schema-sync.timer", "Run schema sync periodically", "hourly"),
			// Self-healing disk headroom: effective free below the
			// floor (the keep_free_space-aware number merges gate on)
			// grows the node's own volume + filesystem online. Effective
			// free (the keep_free_space-aware number) can sit near
			// zero while df still says 30% free. Five minutes is the
			// merge-stall
			// early-warning cadence: parts climb ~10/5min at ingest.
			b.chinitUnit("disk-autoscale.service", "Data-disk headroom keeper (online volume + fs growth)", "disk-autoscale"),
			chinitTimer("disk-autoscale.timer", "Check data-disk headroom every 5 minutes", "*-*-* *:00/5:00"),
			// Pre-backfill disk pre-grow (the 2026-11-16 load-test
			// finding): a scale-out node begins its replication
			// backfill the moment clickhouse-server starts, and a
			// fresh node's fetch storm exhausts unreserved disk
			// space WITHIN the first timer interval — Code 243
			// NOT_ENOUGH_SPACE on fetch reservations (measured on
			// two fresh readers at 0.98-2.03 GiB unreserved while
			// their volumes were mid-OPTIMIZING). This one-shot runs
			// the same idempotent probe once the server is up, BEFORE
			// the fetch storm can outrun the 5-minute timer: above
			// the floor it is a no-op; below it, the volume grow
			// starts 5 minutes earlier — inside the window where
			// the timer would have been the first responder.
			b.chinitUnit("disk-autoscale-boot.service", "One-shot pre-backfill disk headroom probe", "disk-autoscale"),
		)
		// Identity-mode gate, never a role gate: the ordinal lease is
		// claimed by BOTH ASG roles (writers since the writer-ASG
		// rollout), so lease renewal installs on any lease-mode node.
		// The subcommand renews through the state's LeaseTable/hostname
		// — role data it already carries.
		// The config-drift convergence pair (the config-refresher
		// Lambda's replacement): every node pulls its own role spec
		// from the bundle prefix and converges on drift (the in-place
		// boot --refresh). The per-role lock in the claims table
		// serializes the restart-bearing converge across the role.
		units = append(units,
			b.chinitUnit("spec-sync.service", "Role-spec drift convergence (config refresher's replacement)", "spec-sync"),
			chinitTimer("spec-sync.timer", "Pull the role spec and converge on drift", "*-*-* *:00/10:00"),
			// The fleet task layer's executor half: every server node
			// polls for its OWN task row (a refresh motion the
			// coordinator queued). The claim's CAS is the atomicity —
			// two polls on one node serialize, the loser sees the
			// winner's phase.
			b.chinitUnit("task-poll.service", "Fleet task executor (claim and run this node's task)", "task-poll"),
			chinitTimer("task-poll.timer", "Poll this node's fleet task table row", "*-*-* *:00/2:00"),
			// The fleet metrics endpoint (the exporter daemon): the
			// fleet-layer exposition + the ClickHouse pass-through,
			// one scrape target on :9150. Type=simple (the daemon
			// pattern spot-watch uses — chinitUnit's oneshot shape
			// would exit and the endpoint would vanish).
			system.UnitFile{
				Name: "chinit-exporter.service",
				Content: `[Unit]
Description=Fleet metrics endpoint (chinit exporter: /metrics + /clickhouse-metrics)
After=network-online.target clickhouse-server.service

[Service]
Type=simple
ExecStart=/usr/local/bin/chinit exporter
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`,
			},
		)
		if b.Spec.IdentityMode == model.IdentityLease {
			units = append(units,
				b.chinitUnit("lease-renew.service", "Node ordinal lease renewal", "lease-renew"),
				chinitTimer("lease-renew.timer", "Renew the ordinal lease and converge peers every minute", "*-*-* *:*:00"),
			)
		}
		if b.Spec.Role == model.RoleWriter {
			// The maintenance units install on EVERY writer: with ASG
			// writers "writer-0" is a lease ordinal (whoever holds it),
			// not a machine, so the unit set must survive ordinal
			// handoffs. runDeadReplicaGc/runOrphanedGc no-op exit 0 on
			// non-ordinal-0 holders — the timers fire everywhere, the
			// work happens on the holder (or the elected lowest-ordinal
			// live writer when the ordinal-0 lease is vacant).
			units = append(units,
				b.chinitUnit("dead-replica-gc.service", "Dead replica GC (ordinal-0 writer only)", "dead-replica-gc"),
				// dead-replica-gc runs every 5 minutes (was hourly): a scale-in's
				// shutdown drain can lose the ASG termination race under load, and
				// an AMI roll replaces the entire writer fleet within 15-20 minutes;
				// a 1-hour GC window left dead replicas' stale registrations in
				// Keeper blocking PartCheckThread (searchForMissingPartOnOtherReplicas)
				// and GET_PART fetches across the fleet until active parts crossed
				// parts_to_throw_insert (Code 252).
				chinitTimer("dead-replica-gc.timer", "Run dead replica GC every 5 minutes", "*-*-* *:00/5:00"),
				b.chinitUnit("orphaned-data-gc.service", "Orphaned-resource sweep (ordinal-0 writer only)", "orphaned-gc"),
				// DAILY (the untagged-volume reclaim's cadence:
				// churn debris accumulates in hours, and the age
				// threshold — not the schedule — is the safety bound).
				chinitTimer("orphaned-data-gc.timer", "Run the orphaned-resource sweep daily", "daily"),
				// Self-healing replication queues (the 2026-11-16/17 incidents:
				// a NO_REPLICA_HAS_PART queue head or post-roll readonly replica
				// blocks merges and S3Queue ingest within minutes at 60TB/day).
				// Each writer heals its OWN queue every 5 minutes — no ordinal
				// gate — so the unit set installs on every writer and the
				// subcommand is a per-node no-op when nothing is stuck.
				b.chinitUnit("queue-heal.service", "Stuck replication-queue healer (merge-idle + stuck entries)", "queue-heal"),
				chinitTimer("queue-heal.timer", "Heal stuck replication queues every 5 minutes", "*-*-* *:00/5:00"),
				// The writers' VERTICAL autoscale coordinator (the
				// vertical-autoscaler Lambda's replacement): the
				// ordinal-0 holder reads the fleet's writer CPU and
				// steps the launch template (AutoRollback armed).
				// Non-holders no-op exit 0 (the same gate as the GC
				// jobs above); the cooldown record paces steps and
				// the ladder bounds them. Native ASG policies own
				// horizontal — this job never touches them.
				b.chinitUnit("autoscale.service", "Vertical autoscale coordinator (ordinal-0 writer; fleet roll)", "autoscale"),
				chinitTimer("autoscale.timer", "Vertical autoscale decision tick", "*-*-* *:0/15:00"),
				// The fleet task layer's coordinator sweep (the
				// operator's control-loop equivalent at the VM level):
				// the ordinal-0 holder re-dispatches stale task rows
				// and fails exhausted ones. Non-holders no-op exit 0
				// (the same gate as the GCs and autoscale); the
				// vacancy election keeps the sweep alive when the
				// ordinal-0 holder dies.
				b.chinitUnit("fleet-converge.service", "Fleet task sweep (stale re-dispatch, exhausted fail)", "fleet-converge"),
				chinitTimer("fleet-converge.timer", "Sweep the fleet task table every 5 minutes", "*-*-* *:00/5:00"),
			)
			if b.Spec.Backup.OnDemand && b.Spec.Backup.BackupReplicaHostname != "" {
				// On-demand backup replica wake coordinator (StatefulSet
				// 0<->1 parity): the ordinal-0 writer holder wakes
				// backup-0 on the configured schedule; backup-0 syncs
				// its replica catalog, runs clickhouse-backup, and
				// stops its own instance.
				units = append(units,
					b.chinitUnit("backup-wake.service", "On-demand backup replica wake coordinator (ordinal-0 writer only)", "backup-wake"),
					chinitTimer("backup-wake.timer", "Wake the on-demand backup replica on schedule", b.Spec.Backup.CalendarSchedule()),
				)
			}
		}
		if b.Spec.LogShipping.EffectiveTracesEndpoint() != "" {
			// Forward ClickHouse's in-table system.opentelemetry_span_log
			// spans to the remote OTLP/VictoriaTraces endpoint every 30s.
			units = append(units,
				b.chinitUnit("span-ship.service", "Ship ClickHouse system.opentelemetry_span_log spans over OTLP", "span-ship"),
				chinitTimer("span-ship.timer", "Forward ClickHouse OpenTelemetry spans every 30 seconds", "*-*-* *:*:00/30"),
			)
		}
	case model.RoleBackup:
		// The backup replica materializes replicated tables via HostCreateTables.
		units = append(units,
			b.chinitUnit("schema-sync.service", "ClickHouse schema auto-propagation from cluster peers", "schema-sync -retries 3 -prune-stale-self"),
			chinitTimer("schema-sync.timer", "Run schema sync periodically", "hourly"),
			// The backup converges config drift too — its role spec
			// uploads with every other (enable_backup gates only the
			// fleet's awareness of the node, never its own converge).
			b.chinitUnit("spec-sync.service", "Role-spec drift convergence (config refresher's replacement)", "spec-sync"),
			chinitTimer("spec-sync.timer", "Pull the role spec and converge on drift", "*-*-* *:00/10:00"),
			// The backup heals its OWN replication state (the
			// 2026-11-17 backup-0 finding: its 3 summary tables sat
			// read-only with a keeper-registered replica path —
			// the RESTART-when-path-exists class in queue-heal —
			// and no unit on the role could issue the heal).
			b.chinitUnit("queue-heal.service", "Stuck replication-queue healer (merge-idle + stuck entries)", "queue-heal"),
			chinitTimer("queue-heal.timer", "Heal stuck replication queues every 5 minutes", "*-*-* *:00/5:00"),
			// The static-node lease-renew: no lease to extend, but
			// the subcommand's convergeStaticNodePeers path heals
			// /etc/hosts + DNS + remote_servers from the live claims
			// (the 2026-11-17 backup-0 finding: the role installed
			// NO peer convergence, /etc/hosts carried zero cluster
			// entries, and every bare-name part fetch died on
			// DNS_ERROR for hours).
			b.chinitUnit("lease-renew.service", "Static peer convergence (/etc/hosts + DNS + remote_servers)", "lease-renew"),
			chinitTimer("lease-renew.timer", "Converge static-node peers every minute", "*-*-* *:*:00"),
		)
	}
	return units
}

func (b *Boot) installUnits(ctx context.Context) error {
	units := b.chinitUnits()
	installer := system.NewUnitInstaller("/etc/systemd/system")
	if _, err := installer.InstallUnits(units); err != nil {
		return err
	}
	if out, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w: %s", err, out)
	}
	// Boot-time oneshots: enable --now runs them once per boot, right
	// here in the boot transaction (their After=clickhouse-server orders
	// them past the server start). The timers below carry the recurring
	// jobs. The pre-backfill disk probe is the one with a race to win:
	// install_units runs BEFORE the first replication fetch storm can
	// exhaust unreserved space, so the probe's grow (if any) lands
	// within the window the 5-minute timer would otherwise miss.
	for _, u := range units {
		if strings.HasSuffix(u.Name, ".timer") || u.Name == "chinit-exporter.service" {
			if out, err := runCommand(ctx, "systemctl", "enable", "--now", u.Name); err != nil {
				// BestEffort step: enable failures are loud but the
				// boot continues (the bash || echo WARNING shape).
				logger.GetLogger(ctx).Warn("WARNING: unit enable failed", "unit", u.Name, "out", out)
			}
		}
		if isBootStartService(u.Name) {
			if out, err := runCommand(ctx, "systemctl", "start", u.Name); err != nil {
				// Best-effort: the 5-minute timer is the backstop; a
				// failed boot-time probe must not fail the boot.
				logger.GetLogger(ctx).Warn("WARNING: unit start failed", "unit", u.Name, "out", out)
			}
		}
	}
	return nil
}

// backupConfigPath and backupConfigDir are the seams configureBackup
// writes through (tests redirect; production binds the real
// clickhouse-backup paths — the bootstrapDDLPath pattern).
var (
	backupConfigDir  = func() string { return "/etc/clickhouse-backup" }
	backupConfigPath = func() string { return backupConfigDir() + "/config.yml" }
)

// configureBackup writes clickhouse-backup's config.yml against the
// provider object storage and (on the backup replica) the daily timer.
// backupToolBin is the seam for the clickhouse-backup presence check
// (tests point it at an existing file; production binds the golden AMI
// path — the backupConfigDir pattern).
var backupToolBin = func() string { return "/usr/local/bin/clickhouse-backup" }

func (b *Boot) configureBackup(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil
	}
	if b.Spec.Backup.Bucket == "" {
		return nil // backups disabled
	}
	if _, err := os.Stat(backupToolBin()); err != nil {
		logger.GetLogger(ctx).Warn("WARNING: clickhouse-backup absent (golden AMI expected it) — backups disabled")
		return nil
	}
	if err := os.MkdirAll(backupConfigDir(), 0755); err != nil {
		return err
	}

	// ClickHouse credentials for the backup tool. The golden AMI ships
	// clickhouse-backup 2.5.0, which predates the `password_file` config key
	// (added in v3.0) — a password_file entry is silently ignored and the
	// tool authenticates with an empty password, failing with code 516.
	// The `default` user is chctl-restricted (readonly mode rejects the
	// tool's `connect_timeout` setting), so the admin user is used with
	// its password rendered inline. Without this, every node boot writes
	// an unusable config.yml and the nightly backup never lands.
	adminPw := ""
	if pw, err := os.ReadFile(BootstrapDir + "/admin-password"); err == nil {
		adminPw = strings.TrimSpace(string(pw))
	}
	if adminPw == "" {
		return fmt.Errorf("configureBackup: %s/admin-password missing or empty", BootstrapDir)
	}

	region := b.Identity.Region
	if region == "" {
		region = b.Spec.Region
	}
	if region == "" {
		region = b.Meta.Region
	}
	retention := b.Spec.Backup.RetentionDays
	if retention == 0 {
		retention = 7
	}
	compressionLevel := 1
	if b.Spec.Backup.CompressionFormat == "tar" || b.Spec.Backup.CompressionFormat == "none" {
		compressionLevel = 0
	}
	prefix := strings.Trim(b.Spec.Backup.Prefix, "/") + "/" + b.Spec.ClusterName

	// Build the config.yml with the operator's backupconfig.Build —
	// the same builder that generates the sidecar configuration
	// Secrets on-cluster. The storage settings (bucket/path/region/
	// compression) go in as the YAML storage config, and the admin
	// credentials ride the ClickHouse credentials; Build keeps every
	// unknown key, sets the storage paths' defaults, and turns env
	// override back on for the settings it recognizes.
	//
	// NOTE on `path` vs the old hand-rolled `prefix` key: the AMI's
	// clickhouse-backup 2.5.0 (and every later release) reads s3.path —
	// its config struct has no `prefix` field, and go-yaml ignores
	// unknown keys, so the previous hand-rolled template's `prefix:`
	// line was silently DROPPED and backups landed at the bucket root
	// instead of under <prefix>/<cluster>/. Building through
	// backupconfig emits s3.path, restoring the intended prefix.
	storage := fmt.Sprintf(`general:
  remote_storage: s3
  max_file_size: 107374182400
  disable_progress_bar: false
  backups_to_keep_local: 3
  backups_to_keep_remote: %d
  log_level: info
  upload_concurrency: 4
  download_concurrency: 4
  retries_on_failure: 3
  retries_pause: 5s

s3:
  bucket: %s
  path: %s
  region: %s
  compression_level: %d
  compression_format: %s

clickhouse:
  host: localhost
  port: 9000
  data_path: /var/lib/clickhouse
  skip_tables:
    - system.*
    - information_schema.*
    - INFORMATION_SCHEMA.*
`, retention, b.Spec.Backup.Bucket, prefix, region, compressionLevel, b.Spec.Backup.CompressionFormat)

	built, err := backupconfig.Build(
		[]byte(storage),
		backupconfig.Credentials{
			ClickHouseUsername: "admin",
			ClickHousePassword: adminPw,
		},
		backupconfig.Defaults{
			// The storage path is always set in the storage config above;
			// object_disk_path gets the operator's macro-shaped default so
			// object-disk data lands beside the backups under the same
			// prefix, not at the bucket root.
			Path:           "backup/" + prefix,
			ObjectDiskPath: "object_disks/" + prefix,
		},
	)
	if err != nil {
		return fmt.Errorf("configureBackup: build config.yml: %w", err)
	}

	if err := os.WriteFile(backupConfigPath(), built.File, 0600); err != nil {
		return err
	}

	// Daily backup timer on the backup replica only.
	if b.Spec.Role == model.RoleBackup || b.Spec.Backup.IsBackupReplica {
		installer := system.NewUnitInstaller("/etc/systemd/system")
		svcContent := `[Unit]
Description=ClickHouse Daily Backup
After=network-online.target clickhouse-server.service chinit-boot.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/chinit backup-daily
User=root
`
		if b.Spec.Backup.OnDemand {
			svcContent += `
[Install]
WantedBy=multi-user.target
`
		}
		units := []system.UnitFile{
			{
				Name:    "clickhouse-backup-daily.service",
				Content: svcContent,
			},
			chinitTimer("clickhouse-backup-daily.timer", "Run ClickHouse backup daily", b.Spec.Backup.CalendarSchedule()),
		}
		if _, err := installer.InstallUnits(units); err != nil {
			return err
		}
		if out, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("daemon-reload: %w: %s", err, out)
		}
		if out, err := runCommand(ctx, "systemctl", "enable", "--now", "clickhouse-backup-daily.timer"); err != nil {
			return fmt.Errorf("enable backup timer: %w: %s", err, out)
		}
		if b.Spec.Backup.OnDemand {
			// On-demand backup replica (StatefulSet 0<->1 pattern):
			// enable the oneshot service on boot and queue it (ordered
			// After=chinit-boot.service so first boot completes all
			// steps before the backup pass runs and self-stops the
			// instance). Every subsequent StartInstances wake-up from
			// backup-wake boots into multi-user.target, runs the pass,
			// and stops the instance again.
			if out, err := runCommand(ctx, "systemctl", "enable", "clickhouse-backup-daily.service"); err != nil {
				return fmt.Errorf("enable on-demand backup service: %w: %s", err, out)
			}
			if out, err := runCommand(ctx, "systemctl", "start", "--no-block", "clickhouse-backup-daily.service"); err != nil {
				logger.GetLogger(ctx).Warn("WARNING: queue on-demand backup service failed", "out", out, "err", err)
			}
		}
	}
	return nil
}

// configureMonitoring writes the OTEL/Fluent Bit configs for remote
// log shipping (disabled silently when the spec's endpoint is empty;
// on AWS the local CloudWatch agent is configured separately by
// configureCloudWatch — the image only installs the deb, chinit owns
// the config).
//
// The OTEL config ships TWO sources: the ClickHouse server logs
// (filelog) and chinit's own journal records (journald receiver over
// the chinit unit names the boot installs). The journal records are
// the CLI's structured outcome lines — the sweep/poll/probe numbers
// as attributes — so the Log Explorer rules can alert on them with
// zero new emission code (the metrics pipeline carries the
// aggregates; the log pipeline carries the per-tick detail).
// otelcolConfigPath, otelcolBin, and otelcolUnitDir are seams for
// configureMonitoring tests; production binds standard system paths.
var (
	otelcolConfigPath = func() string { return "/etc/otelcol/config.yaml" }
	otelcolBin        = func() string { return "/usr/local/bin/otelcol" }
	otelcolUnitDir    = func() string { return "/etc/systemd/system" }
)

// OTelCollectorRenderer renders /etc/otelcol/config.yaml for logs,
// metrics (Prometheus scrape + local OTLP -> OTLP HTTP or Prometheus
// Remote Write), and traces (local OTLP -> OTLP HTTP / VictoriaTraces).
type OTelCollectorRenderer struct {
	spec     *model.BootSpec
	hostname string
	units    []system.UnitFile
}

// NewOTelCollectorRenderer constructs a collector config renderer over
// the node's BootSpec, settled hostname, and role unit inventory.
func NewOTelCollectorRenderer(spec *model.BootSpec, hostname string, units []system.UnitFile) *OTelCollectorRenderer {
	return &OTelCollectorRenderer{spec: spec, hostname: hostname, units: units}
}

// Render produces the complete OpenTelemetry Collector YAML document.
func (r *OTelCollectorRenderer) Render() string {
	var b strings.Builder
	r.writeReceivers(&b)
	r.writeProcessors(&b)
	r.writeExporters(&b)
	r.writePipelines(&b)
	return b.String()
}

func (r *OTelCollectorRenderer) hasPrometheusScrape() bool {
	return r.spec.LogShipping.EffectiveMetricsEndpoint() != "" && r.spec.Role != model.RoleKeeper
}

func (r *OTelCollectorRenderer) writeReceivers(b *strings.Builder) {
	ls := r.spec.LogShipping
	logsURL := ls.EffectiveLogsEndpoint()
	metricsURL := ls.EffectiveMetricsEndpoint()
	tracesURL := ls.EffectiveTracesEndpoint()

	b.WriteString("receivers:\n")
	if metricsURL != "" || tracesURL != "" {
		b.WriteString("  otlp:\n    protocols:\n      http:\n        endpoint: 127.0.0.1:4318\n")
	}
	if logsURL != "" {
		if r.spec.Role == model.RoleKeeper {
			b.WriteString("  filelog:\n    include: [/var/log/clickhouse-keeper/*.log, /var/log/clickhouse-server/*.log]\n")
		} else {
			b.WriteString("  filelog:\n    include: [/var/log/clickhouse-server/*.log, /var/log/clickhouse-server/*-*.log]\n")
		}
		fmt.Fprintf(b, "  journald/chinit:\n    units: [%s]\n    priority: info\n", journaldUnitList(r.units))
	}
	if r.hasPrometheusScrape() {
		fmt.Fprintf(b, `  prometheus:
    config:
      scrape_configs:
        - job_name: clickhouse
          scrape_interval: %s
          static_configs:
            - targets: ["127.0.0.1:9363"]
`, ls.EffectiveScrapeInterval())
		if r.spec.Role == model.RoleWriter || r.spec.Role == model.RoleReader {
			fmt.Fprintf(b, `        - job_name: chinit
          scrape_interval: %s
          static_configs:
            - targets: ["127.0.0.1:9150"]
`, ls.EffectiveScrapeInterval())
		}
	}
}

func (r *OTelCollectorRenderer) writeProcessors(b *strings.Builder) {
	fmt.Fprintf(b, `
processors:
  batch: {}
  resource:
    attributes:
      - key: host.name
        value: %s
        action: upsert
      - key: chinit.cluster
        value: %s
        action: upsert
      - key: chinit.role
        value: %s
        action: upsert
      - key: service.name
        from_attribute: journald.journal.systemd.unit
        action: insert
`, r.hostname, r.spec.ClusterName, string(r.spec.Role))
}

func (r *OTelCollectorRenderer) writeExporters(b *strings.Builder) {
	ls := r.spec.LogShipping
	logsURL := ls.EffectiveLogsEndpoint()
	metricsURL := ls.EffectiveMetricsEndpoint()
	tracesURL := ls.EffectiveTracesEndpoint()

	b.WriteString("\nexporters:\n")
	if logsURL != "" {
		fmt.Fprintf(b, "  otlphttp:\n    logs_endpoint: %s\n", logsURL)
		if h := otlpHeadersBlock(ls.Headers); h != "" {
			b.WriteString(h + "\n")
		}
	}
	if metricsURL != "" {
		if ls.IsPrometheusRemoteWrite() {
			fmt.Fprintf(b, "  prometheusremotewrite:\n    endpoint: %s\n    resource_to_telemetry_conversion:\n      enabled: true\n", metricsURL)
		} else {
			fmt.Fprintf(b, "  otlphttp/metrics:\n    metrics_endpoint: %s\n", metricsURL)
		}
		if h := otlpHeadersBlock(ls.EffectiveMetricsHeaders()); h != "" {
			b.WriteString(h + "\n")
		}
	}
	if tracesURL != "" {
		fmt.Fprintf(b, "  otlphttp/traces:\n    traces_endpoint: %s\n", tracesURL)
		if h := otlpHeadersBlock(ls.EffectiveTracesHeaders()); h != "" {
			b.WriteString(h + "\n")
		}
	}
}

func (r *OTelCollectorRenderer) writePipelines(b *strings.Builder) {
	ls := r.spec.LogShipping
	b.WriteString("\nservice:\n  pipelines:\n")
	if ls.EffectiveLogsEndpoint() != "" {
		b.WriteString("    logs:\n      receivers: [filelog, journald/chinit]\n      processors: [resource, batch]\n      exporters: [otlphttp]\n")
	}
	if ls.EffectiveMetricsEndpoint() != "" {
		metricsReceivers := "[otlp]"
		if r.hasPrometheusScrape() {
			metricsReceivers = "[otlp, prometheus]"
		}
		metricsExporter := "[otlphttp/metrics]"
		if ls.IsPrometheusRemoteWrite() {
			metricsExporter = "[prometheusremotewrite]"
		}
		fmt.Fprintf(b, "    metrics:\n      receivers: %s\n      processors: [resource, batch]\n      exporters: %s\n", metricsReceivers, metricsExporter)
	}
	if ls.EffectiveTracesEndpoint() != "" {
		b.WriteString("    traces:\n      receivers: [otlp]\n      processors: [resource, batch]\n      exporters: [otlphttp/traces]\n")
	}
}

func (b *Boot) configureMonitoring(ctx context.Context) error {
	if !b.Spec.LogShipping.HasAnyEndpoint() {
		return nil // remote shipping disabled
	}
	agent := b.Spec.LogShipping.Agent
	if agent == "" {
		agent = "otel"
	}
	switch agent {
	case "otel":
		cfgPath := otelcolConfigPath()
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0755); err != nil {
			return err
		}
		cfg := NewOTelCollectorRenderer(b.Spec, b.Identity.Hostname, b.chinitUnits()).Render()
		if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
			return err
		}
		if _, err := os.Stat(otelcolBin()); err != nil {
			logger.GetLogger(ctx).Warn("WARNING: otelcol binary absent; config written without starting service", "bin", otelcolBin())
			return nil
		}
		installer := system.NewUnitInstaller(otelcolUnitDir())
		unit := system.UnitFile{
			Name: "otelcol.service",
			Content: fmt.Sprintf(`[Unit]
Description=OpenTelemetry Collector (logs, metrics, traces remote push)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s --config=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, otelcolBin(), cfgPath),
		}
		if _, err := installer.InstallUnits([]system.UnitFile{unit}); err != nil {
			return err
		}
		if out, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("otelcol daemon-reload: %w: %s", err, out)
		}
		if out, err := runCommand(ctx, "systemctl", "enable", "otelcol.service"); err != nil {
			logger.GetLogger(ctx).Warn("WARNING: otelcol enable failed", "out", out, "err", err)
		}
		if out, err := runCommand(ctx, "systemctl", "restart", "otelcol.service"); err != nil {
			logger.GetLogger(ctx).Warn("WARNING: otelcol restart failed", "out", out, "err", err)
		}
	case "fluentbit":
		logsEndpoint := b.Spec.LogShipping.EffectiveLogsEndpoint()
		if logsEndpoint == "" {
			return nil
		}
		if err := os.MkdirAll("/etc/fluent-bit", 0755); err != nil {
			return err
		}
		output := b.Spec.LogShipping.FluentbitOutput
		if output == "" {
			output = "http"
		}
		cfg := fmt.Sprintf(`[SERVICE]
    Flush        5
    Daemon       Off

[INPUT]
    Name         tail
    Path         /var/log/clickhouse-server/*.log

[OUTPUT]
    Name         %s
    Match        *
    Endpoint     %s
`, output, logsEndpoint)
		if err := os.WriteFile("/etc/fluent-bit/fluent-bit.conf", []byte(cfg), 0644); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown log shipping agent %q (want otel or fluentbit)", agent)
	}
	return nil
}

// journaldUnitList renders the chinit unit names the journald receiver
// subscribes to (comma-separated). The boot unit is the user-data
// wrapper's chinit-boot.service — the one unit chinitUnits() never
// returns (the wrapper owns it) — so it rides first, then the role's
// own unit list (services only; timers carry no journal records).
func journaldUnitList(units []system.UnitFile) string {
	names := []string{"chinit-boot.service"}
	for _, u := range units {
		if !strings.HasSuffix(u.Name, ".service") {
			continue
		}
		names = append(names, u.Name)
	}
	return strings.Join(names, ", ")
}

// otlpHeadersBlock renders the exporter's headers stanza from the
// spec's comma-separated "k=v,k=v" string ("" = an empty line; the
// collector's YAML parses a bare newline cleanly). The pairs are
// verbatim — the spec author owns the token.
func otlpHeadersBlock(headers string) string {
	if strings.TrimSpace(headers) == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("    headers:\n")
	for _, pair := range strings.Split(headers, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			fmt.Fprintf(&b, "      %s: %s\n", strings.TrimSpace(k), strings.TrimSpace(v))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// spotWatchEligible answers whether the role's departure carries state
// the notice window must release — the ASG members (writers + readers):
// a live replica registration, an ordinal lease and a DNS record. The
// spot watcher's drain is the only path that releases all three inside
// the 2-minute ITN budget. Keepers hold no lease and no server (drain
// is meaningless); the backup replica is static (on-demand, never
// spot) and must never drain its identity on a notice.
func spotWatchEligible(role model.Role) bool {
	switch role {
	case model.RoleWriter, model.RoleReader:
		return true
	}
	return false
}

// installSpotWatch writes the spot-watch service (writers + readers):
// a long-running chinit daemon that drains on the interruption notice.
//
// Writers included SINCE writer_spot_ratio made Spot writers real
// (dev runs writer-1..N 100% Spot above the on-demand base): a Spot
// writer's departure is exactly a reader's — a 2-minute ITN window
// the node must use for the graceful exit (queues stopped, server
// shut down, replica dropped on a live peer, DNS + lease released).
// Without the watcher the writer's only drain is the ExecStop
// shutdown unit, which must skip the peer drop (Code 305 while the
// local server still runs) — the ghost registrations then wait on
// the dead-replica GC, and a GC pass that itself fails wedges every
// peer's replication queue behind log entries a dead ordinal owns
// (the 2026-11-19 writer-5/writer-7 finding). On-demand writers
// poll a 404 forever — harmless.
func (b *Boot) installSpotWatch(ctx context.Context) error {
	if !spotWatchEligible(b.Spec.Role) {
		return nil
	}
	watcher := chinitport.AsSpotSignals(b.Provider)
	if watcher == nil {
		logger.GetLogger(ctx).Warn("WARNING: cloud has no spot-watch capability; no drain handler installed",
			"cloud", b.Provider.Name())
		return nil
	}
	installer := system.NewUnitInstaller("/etc/systemd/system")
	units := []system.UnitFile{{
		Name: "chinit-spot-watch.service",
		Content: `[Unit]
Description=Spot interruption drain watcher (chinit spot-watch)
After=network-online.target clickhouse-server.service

[Service]
Type=simple
ExecStart=/usr/local/bin/chinit spot-watch
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
`,
	}}
	if _, err := installer.InstallUnits(units); err != nil {
		return err
	}
	if out, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w: %s", err, out)
	}
	if out, err := runCommand(ctx, "systemctl", "enable", "--now", "chinit-spot-watch.service"); err != nil {
		return fmt.Errorf("enable chinit-spot-watch: %w: %s", err, out)
	}
	return nil
}

// installShutdownDrain writes the in-node shutdown drain unit
// (writers + readers): chinit-shutdown-drain.service, whose ExecStop
// runs `chinit drain-shutdown` when systemd tears the node down.
//
// Why a shutdown unit instead of relying on the ASG termination hook's
// SSM dispatch alone: the OS shutdown path is the ONE signal every
// termination mode goes through — ASG scale-in, EC2 health-check
// replacement (which NEVER fires the hook), spot reclaim at the
// deadline, operator stop. The hook's SSM dispatch also races the
// agent's own teardown. systemd runs the drain from INSIDE the node:
// the unit is ordered After=clickhouse-server.service, so on
// shutdown (reverse dependency order) the drain runs FIRST while
// ClickHouse is still up to serve the drain's peer drop and SYSTEM
// STOP queries, then the server stops.
//
// Writers and readers ONLY (the ASG roles with a lease, a DNS record
// and a replica registration to release): keepers hold no lease and
// run no clickhouse-server; the backup replica survives reboots with
// its identity — draining it on shutdown would drop a replica that
// is coming right back (the subcommand's reboot guard covers the
// reboot case for the ASG roles the same way).
//
// The unit is a no-op at start (ExecStart=/bin/true +
// RemainAfterExit=yes): its ONLY purpose is to be "active" so systemd
// runs ExecStop against it during the shutdown transaction. The
// drain-shutdown subcommand carries the reboot guard (skip when
// reboot.target is the pending job — identity survives a reboot) and
// the deadline discipline (150s, bounded under EC2's window).
// shutdownDrainUnit is the pure unit builder — split out so the
// role-gating and unit-content contracts are testable without
// executing systemctl (the matrix's test seam; installShutdownDrain
// owns the install and enable calls).
func (b *Boot) shutdownDrainUnit() system.UnitFile {
	return system.UnitFile{
		Name: "chinit-shutdown-drain.service",
		Content: `[Unit]
Description=In-node shutdown drain (chinit drain-shutdown)
# After=clickhouse-server: on shutdown systemd stops units in reverse
# dependency order — this unit's ExecStop runs BEFORE the server
# stops, so the drain's peer drop and SYSTEM STOP queries have a
# live server to talk to. Wants=network-online: the drain talks to
# the lease table and DNS.
After=network-online.target clickhouse-server.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/true
ExecStop=/usr/local/bin/chinit drain-shutdown
# The drain's own budget is 150s (the subcommand's constant); the
# unit bound sits above it so the drain is never cut short by its
# own wrapper — EC2's shutdown window (180s) remains the outer bound.
TimeoutStopSec=170
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`,
	}
}

// installShutdownDrain writes the in-node shutdown drain unit
// (writers + readers) and enables it.
func (b *Boot) installShutdownDrain(ctx context.Context) error {
	if !b.Spec.Role.HasStatelessVolumes() {
		return nil
	}
	installer := system.NewUnitInstaller("/etc/systemd/system")
	units := []system.UnitFile{b.shutdownDrainUnit()}
	if _, err := installer.InstallUnits(units); err != nil {
		return err
	}
	if out, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w: %s", err, out)
	}
	if out, err := runCommand(ctx, "systemctl", "enable", "--now", "chinit-shutdown-drain.service"); err != nil {
		return fmt.Errorf("enable chinit-shutdown-drain: %w: %s", err, out)
	}
	return nil
}

// volumeReleaseUnit is the pure unit builder for the shutdown volume
// release (the shutdownDrainUnit pattern — split out so the
// unit-content contract is testable without executing systemctl).
//
// The ordering is the design: Before=clickhouse-server means the unit
// is stopped BEFORE the server on shutdown (a unit whose ExecStop
// deletes the volumes the server writes to MUST NOT run while the
// server is still writing). systemd stops Before=-ordered units
// first in the shutdown transaction, so the sequence is:
// drain-shutdown's ExecStop (After=clickhouse-server — identity
// release against a LIVE server) → clickhouse-server stops → this
// unit's ExecStop deletes the now-quiescent volumes.
func (b *Boot) volumeReleaseUnit() system.UnitFile {
	return system.UnitFile{
		Name: "chinit-volume-release.service",
		Content: `[Unit]
Description=In-node shutdown volume release (chinit volume-release)
# Before=clickhouse-server: systemd stops Before=-ordered units FIRST
# on shutdown, so the volume delete runs only after the server (and
# every writer above it) has stopped. The reboot guard lives in the
# subcommand (reboot.target in list-jobs skips the release).
Before=clickhouse-server.service
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/bin/true
ExecStop=/usr/local/bin/chinit volume-release
# The release is bounded under EC2's 180s shutdown window with
# headroom for the detach settle; EC2 remains the outer bound.
TimeoutStopSec=90
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`,
	}
}

// installVolumeRelease writes the in-node shutdown volume-release
// unit (writers + readers — the stateless-volume roles) and enables
// it. The subcommand carries the reboot guard; the unit's only
// purpose is to be active so systemd runs ExecStop at shutdown.
func (b *Boot) installVolumeRelease(ctx context.Context) error {
	if !b.Spec.Role.HasStatelessVolumes() {
		return nil
	}
	installer := system.NewUnitInstaller("/etc/systemd/system")
	units := []system.UnitFile{b.volumeReleaseUnit()}
	if _, err := installer.InstallUnits(units); err != nil {
		return err
	}
	if out, err := runCommand(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload: %w: %s", err, out)
	}
	if out, err := runCommand(ctx, "systemctl", "enable", "--now", "chinit-volume-release.service"); err != nil {
		return fmt.Errorf("enable chinit-volume-release: %w: %s", err, out)
	}
	return nil
}

// trackOwnVolumes records this node's own volume IDs into the
// persisted state (writers/readers — the roles whose volumes are
// stateless by contract). Live discovery is authoritative; the
// recorded list is the shutdown path's fallback for the window where
// the cloud API is unreachable mid-teardown. No capability, no
// instance id, or a listing failure degrades to an empty recording
// (loud warning) — the release path and the orphan sweep both have
// live-discovery paths of their own.
func (b *Boot) trackOwnVolumes(ctx context.Context) error {
	if !b.Spec.Role.HasStatelessVolumes() {
		return nil
	}
	releaser := chinitport.AsVolumeReleaser(b.Provider)
	if releaser == nil {
		logger.GetLogger(ctx).Info("track-own-volumes: provider has no volume-release capability; nothing recorded",
			"cloud", b.Provider.Name())
		return nil
	}
	vols, err := releaser.ListOwnVolumes(ctx, chinitport.VolumeReleaseRequest{InstanceID: b.Meta.InstanceID})
	if err != nil {
		logger.GetLogger(ctx).Warn("track-own-volumes: listing failed; the shutdown release path falls back to live discovery",
			"err", err.Error())
		return nil
	}
	ids := make([]string, 0, len(vols))
	for _, v := range vols {
		if v.VolumeID != "" {
			ids = append(ids, v.VolumeID)
		}
	}
	if b.PersistVolumes != nil {
		b.PersistVolumes(ids)
	}
	logger.GetLogger(ctx).Info("track-own-volumes: recorded own volumes",
		"count", len(ids),
		"instance_id", b.Meta.InstanceID)
	return nil
}

// cloudWatchConfig is the pure config-shape builder configureCloudWatch
// writes to disk — split out so the contracts (role gating, namespace
// split, identity-label placement) are testable without executing
// system commands.
type cloudWatchConfig struct {
	PromCfg         string
	AgentJSON       string
	ChNamespace     string
	MetricSelectors string
}

// cloudWatchConfig renders the agent's prometheus scrape + JSON config
// for a role. Custom identity labels live ONLY in the Prometheus EMF
// dimensions: append_dimensions with custom keys fails agent config
// validation on EC2, so node-metric alarms must key on the
// auto-appended InstanceID dimension instead.
func (b *Boot) cloudWatchConfig(role model.Role, cluster string) cloudWatchConfig {
	cfgDir := "/opt/aws/amazon-cloudwatch-agent/etc"

	// Exact exposition names from ClickHouse 26.8 :9363/metrics — the
	// operator's alert-catalog thresholds key on these. Keepers have no
	// server logs and servers no keeper logs; pointing the agent at
	// files a role cannot exist with makes it retry forever.
	//
	// Self-healing additions :
	// - ClickHouseMetrics_DelayedInserts feeds the
	// writer-insert-backpressure alarm — it sat INSUFFICIENT_DATA
	// since creation because the selector omitted it.
	// - ClickHouseMetrics_Merge is the merge-drain signal: parts can
	// pile up with zero merges running (the disk-full stall) and
	// neither CPU nor parts-count shows WHY.
	// - ClickHouseAsyncMetrics_DiskAvailable{disk} is the effective
	// free space (volume minus keep_free_space) — the 32 GiB volume
	// with a 10 GiB reservation left ~1 MiB effective while df
	// showed 30% free; the disk-level alarm must key on the
	// metric ClickHouse itself refuses writes at.
	chNamespace := "ClickHouse/Server"
	metricSelectors := `^ClickHouse(AsyncMetrics_)?(Uptime|ReplicasMaxAbsoluteDelay|MaxPartCountForPartition)$|^ClickHouseMetrics_(ReadonlyReplica|DistributedFilesToInsert|Query|DelayedInserts|Merge)$|^ClickHouseAsyncMetrics_Disk(Available|Total|Used)$`
	logEntries := `,
                    {
                        "file_path": "/var/log/clickhouse-server/clickhouse-server.log",
                        "log_group_name": "/clickhouse/` + cluster + `/server",
                        "log_stream_name": "{instance_id}"
                    },
                    {
                        "file_path": "/var/log/clickhouse-server/clickhouse-server.err.log",
                        "log_group_name": "/clickhouse/` + cluster + `/error",
                        "log_stream_name": "{instance_id}"
                    }`
	if role == model.RoleKeeper {
		chNamespace = "ClickHouse/Keeper"
		metricSelectors = `^ClickHouse(AsyncMetrics_)?Keeper[A-Za-z]*$|^ClickHouseProfileEvents_KeeperSnapshot[A-Za-z]*$|^ClickHouseMetrics_KeeperOutstandingRequests$`
		logEntries = `,
                    {
                        "file_path": "/var/log/clickhouse-keeper/clickhouse-keeper.log",
                        "log_group_name": "/clickhouse/` + cluster + `/keeper",
                        "log_stream_name": "{instance_id}"
                    },
                    {
                        "file_path": "/var/log/clickhouse-keeper/clickhouse-keeper.err.log",
                        "log_group_name": "/clickhouse/` + cluster + `/keeper",
                        "log_stream_name": "{instance_id}"
                    },
                    {
                        "file_path": "/var/log/keeper_health.log",
                        "log_group_name": "/clickhouse/` + cluster + `/keeper",
                        "log_stream_name": "{instance_id}"
                    }`
	}

	promCfg := `global:
  scrape_interval: 60s
  scrape_timeout: 10s
scrape_configs:
  - job_name: clickhouse
    static_configs:
      - targets:
          - 127.0.0.1:9363
        labels:
          ClusterName: "` + cluster + `"
          NodeType: "` + string(role) + `"
`
	agentCfg := `{
    "agent": {
        "metrics_collection_interval": 60,
        "run_as_user": "cwagent"
    },
    "metrics": {
        "namespace": "ClickHouse/Node",
        "metrics_collected": {
            "cpu": { "measurement": ["cpu_usage_idle", "cpu_usage_iowait"], "resources": ["*"] },
            "mem": { "measurement": ["mem_used_percent", "mem_available"] },
            "disk": { "measurement": ["used_percent"], "resources": ["/", "/var/lib/clickhouse"] },
            "swap": { "measurement": ["swap_used_percent"] },
            "net": { "measurement": ["bytes_recv", "bytes_sent"], "resources": ["*"] }
        }
    },
    "logs": {
        "metrics_collected": {
            "prometheus": {
                "cluster_name": "` + cluster + `",
                "log_group_name": "/clickhouse/` + cluster + `/prometheus",
                "prometheus_config_path": "` + cfgDir + `/prometheus.yaml",
                "emf_processor": {
                    "metric_namespace": "` + chNamespace + `",
                    "metric_declaration": [
                        {
                            "source_labels": ["job"],
                            "label_matcher": "^clickhouse$",
                            "metric_selectors": ["` + metricSelectors + `"],
                            "dimensions": [["ClusterName", "NodeType"], ["ClusterName"], ["ClusterName", "disk"], ["ClusterName", "NodeType", "disk"]]
                        }
                    ]
                }
            }
        },
        "logs_collected": {
            "files": {
                "collect_list": [
                    {
                        "file_path": "/var/log/bootstrap.log",
                        "log_group_name": "/clickhouse/` + cluster + `/bootstrap",
                        "log_stream_name": "{instance_id}"
                    }` + logEntries + `
                ]
            }
        }
    }
}
`
	return cloudWatchConfig{
		PromCfg:         promCfg,
		AgentJSON:       agentCfg,
		ChNamespace:     chNamespace,
		MetricSelectors: metricSelectors,
	}
}

// configureCloudWatch configures the AWS node-metrics + log-shipping
// agent — the step the bash-era configure_cloudwatch_agent carried
// (deleted in the chinit cutover). Nothing else configures the agent:
// packer only INSTALLS the deb, so without this step every node is
// invisible to every alarm while its logs pile up unreadable on local
// disk (observed live: a full-day outage whose 2,328 errors existed
// only on the node).
//
// Why chinit and not the launch template: instances come and go — an
// ASG instance refresh or replacement must boot with visibility
// configured, or the fix is a per-node manual patch that dies with the
// instance. The config is the verbatim agent JSON from the bash era:
// node metrics (ClickHouse/Node), the :9363 Prometheus EMF bridge
// (ClickHouse/Server or ClickHouse/Keeper by role), and the per-role
// log collect_list into /clickhouse/<cluster>/{server,error,keeper,
// bootstrap,prometheus}.
//
// AWS-only (the Provider.Name branch the aws-cli binary requirement
// already uses); a no-op on other clouds (Azure's AMA keeps its
// VM-extension path).
func (b *Boot) configureCloudWatch(ctx context.Context) error {
	if b.Provider.Name() != "aws" {
		return nil
	}

	// The agent runs as cwagent but ClickHouse writes its logs 640
	// clickhouse:clickhouse — group membership is the standard read
	// grant (keepers may lack the group; the server package creates
	// it on server roles).
	if _, err := runCommand(ctx, "getent", "group", "clickhouse"); err != nil {
		if out, gerr := runCommand(ctx, "groupadd", "clickhouse"); gerr != nil {
			return fmt.Errorf("groupadd clickhouse: %w: %s", gerr, out)
		}
	}
	// Best-effort: the cwagent user exists on the golden AMI.
	_, _ = runCommand(ctx, "usermod", "-aG", "clickhouse", "cwagent")

	cfgDir := "/opt/aws/amazon-cloudwatch-agent/etc"
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		return err
	}

	cluster := b.Identity.ClusterName
	if cluster == "" {
		cluster = b.Spec.ClusterName
	}

	cfg := b.cloudWatchConfig(b.Identity.Role, cluster)
	if err := os.WriteFile(cfgDir+"/prometheus.yaml", []byte(cfg.PromCfg), 0644); err != nil {
		return err
	}
	agentPath := cfgDir + "/amazon-cloudwatch-agent.json"
	if err := os.WriteFile(agentPath, []byte(cfg.AgentJSON), 0644); err != nil {
		return err
	}

	if out, err := runCommand(ctx, "/opt/aws/amazon-cloudwatch-agent/bin/amazon-cloudwatch-agent-ctl",
		"-a", "fetch-config", "-m", "ec2", "-c", "file:"+agentPath, "-s"); err != nil {
		return fmt.Errorf("cw-agent fetch-config: %w: %s", err, out)
	}
	if out, err := runCommand(ctx, "systemctl", "enable", "amazon-cloudwatch-agent"); err != nil {
		return fmt.Errorf("enable amazon-cloudwatch-agent: %w: %s", err, out)
	}
	return nil
}
