// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/keeper"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/operator"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// renderNodeConfig renders the whole node through chctl's library
// ONE call (identity is settled by now), installing host sections +
// inventory + users.d with ownership/perms/prune.
//
// Why the library and not the CLI: colocated binaries share the domain
// packages; the CLI boundary would re-parse flags, lose typed errors
// and fork a process per render. The NodeRenderer is the same code path
// the operator uses on Kubernetes.
func (b *Boot) renderNodeConfig(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil // configure_keeper owns the keeper render
	}
	cr, err := b.LoadCHI(ctx)
	if err != nil {
		return Fatal(err)
	}

	// The settled identity's CRName is authoritative — NOT a fresh
	// CRName call: settle_identity already mapped the reader ordinal
	// (writer_count + claimed), and re-applying the offset here
	// double-shifts the reader past the end of the replica list
	// .
	crName := b.Identity.CRName
	if crName == "" {
		crName, _ = b.CRName()
	}

	renderer := operator.NewNodeRenderer(operator.NewConfigRenderer(), operator.NewUsersRenderer(providerSecrets{p: b.Provider}))
	request := port.RenderNodeRequest{
		NormalizedCR: cr,
		NodeType:     string(b.Spec.Role),
		HostName:     crName,
		OutputDir:    ConfigDir,
		UsersDir:     UsersDir,
		// Per-user IP allowlists from the spec: the users.d render
		// defaults to localhost-only when the map is empty, which
		// breaks every cross-node distributed query (Code 516
		// Authentication failed — the interserver peer arrives from
		// the VPC CIDR, not 127.0.0.1; chinit-cutover
		//). The bash era passed the same lists via -networks.
		Networks: map[string][]string{
			// Every nodes-subnet CIDR: the Single-AZ shape answers
			// exactly the historical one-entry list; the multi-AZ
			// opt-in adds the per-AZ extras so peers in other AZs
			// never hit Code 516.
			"admin":   withSubnet(b.Spec.Network.AdminAllowedNetworks, b.Spec.NodeSubnetCIDRs()),
			"writer":  withSubnet(b.Spec.Network.WriterAllowedNetworks, b.Spec.NodeSubnetCIDRs()),
			"default": withSubnet(b.Spec.Network.DefaultUserAllowedNetworks, b.Spec.NodeSubnetCIDRs()),
		},
	}
	// The inventory host list renders remote_servers; writers, readers
	// AND the backup replica are CR members — the backup materializes
	// the same replicated tables, so its remote_servers render needs
	// host inventory like any server node. WITHOUT the map
	// the render falls back to the CR's POD-pattern host names
	// (chi-<chi>-<cluster>-<shard>-<replica>) and the server
	// crash-loops at startup on DNS_ERROR Code 198 for every
	// unresolvable slot (restart
	// counter 67 on exactly this render). The list is built ONCE
	// here (live-claims discovery is a lease-table scan; the replica
	// map and remote_servers must never disagree by racing two scans).
	request.Replicas = map[string]string{}
	hosts, note := b.clusterHostsWithSource()
	if note != "" {
		logger.GetLogger(ctx).Warn(note)
	}
	request.Hosts = hosts
	// Replica names (the {replica} macro value): without this map
	// the macros render the operator's POD pattern
	// (chi-<chi>-<cluster>-<shard>-<replica>) as the replica
	// identity — the keeper replica_path then registers under a
	// name no cloud tooling recognizes, and SYSTEM SYNC REPLICA
	// / DROP REPLICA operate on the wrong identity (the live
	//). The bash era passed -replica
	// 01-$idx=$hostname.
	for _, h := range hosts {
		request.Replicas[h.Name] = strings.SplitN(h.Address, ".", 2)[0]
	}
	// Distributed-query credentials: without an injected
	// <user>/<password> in remote_servers, every cross-node query
	// authenticates as the passwordless default against nodes
	// whose default REQUIRES a password (Code 516 on every
	// clusterAllReplicas — ; the
	// bash era passed -remote-user/-remote-password-file).
	if pw, err := os.ReadFile(filepath.Join(BootstrapDir, "default-password")); err == nil {
		request.RemoteUser = "default"
		request.RemotePassword = strings.TrimSpace(string(pw))
	}
	result, err := renderer.RenderNode(ctx, request)
	if err != nil {
		return Fatal(fmt.Errorf("render node config: %w", err))
	}
	_ = result
	return nil
}

// clusterHostsWithSource builds the CR inventory plus the provenance
// note for the boot log (live-claims vs static fallback + its
// warning) — split so the inventory tests assert the source without
// scraping stderr. The namer keys entries by the operator CR host name
// ({shard}-{index}, writers 01-0.. then readers
// 01-(writerCeiling+readerOrdinal).. — the same order CRName
// derives), with the stack's private-zone DNS as the address. Keying
// by hostname instead of CR name renders remote_servers <host> entries
// as unresolvable chi-<chi>-<cluster>-<shard>-<replica> pod names (the
// live boot server crash-loops on DNS_ERROR Code 198).
//
// The inventory's SOURCE OF TRUTH is lease table when the
// node holds a lease (identity_mode: lease): a clusterAllReplicas DDL
// read fans out to every host in this list, so a reserved-but-vacant
// slot (the ASG's max is a RESERVATION, not a member) would fail every
// fan-out with an unreachable host. The lease table owns liveness
// only ACTIVE claim rows become hosts. The static spec lists stay as
// the fallback for clouds without the listing capability (Azure
// interim), reported loudly so a vacant-slot regression is visible in
// the boot log.
//
// Source selection:
// - lease identity + listable lease table → ACTIVE claims (the
// contract above). The table name is the SETTLED identity's
// LeaseTable (the boot claimed from it; the spec field is the
// pre-settle fallback for direct unit wiring).
// - lease identity without the capability (Azure interim) or a
// listing failure → static lists + a loud WARNING: a lease-mode
// fleet's reserved lists contain vacant slots, and a silent
// fallback would put unreachable hosts in clusterAllReplicas
// reads with nothing in the log pointing at the cause.
// - static identity → static lists, no warning: every static node
// is a real machine, so the reserved list IS the member list.
func (b *Boot) clusterHostsWithSource() (hosts []domain.HostIdentity, note string) {
	if b.Spec.IdentityMode != model.IdentityLease {
		return staticSpecInventory(b), ""
	}
	table := b.Identity.LeaseTable
	if table == "" {
		table = b.Spec.ClaimerTableName
	}
	lister := port.AsLeaseLister(b.Provider)
	if lister == nil {
		return staticSpecInventory(b),
			fmt.Sprintf("WARNING: cloud %s has no lease-listing capability; inventory enumerated from static spec (may include vacant slots)", b.Provider.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	claims, err := lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: table})
	if err != nil {
		// A lease-table outage must not wedge the boot in a half
		// inventory: fall back to the static lists and say so.
		return staticSpecInventory(b),
			fmt.Sprintf("WARNING: live-claims discovery on cloud %s failed (%s); inventory enumerated from static spec (may include vacant slots)", b.Provider.Name(), err)
	}
	return claimsInventory(b, claims), ""
}

// claimsInventory maps ACTIVE lease rows to CR slots: writers take
// 01-{writerOrdinal}; readers take 01-{writerCeiling+readerOrdinal}
// (writerCeiling = WriterMax, fallback WriterCount — the fixed-ceiling
// arithmetic CRName lands, so reader slots never shift when the
// writer fleet scales). Ordinals at or beyond a role's ceiling are
// stale rows (a shrunk writer_max) — the CR no longer reserves their
// slot, so they are dropped. Order is CR slot (stable across runs: the
// lease-table Scan order is unordered). THIS node's claim is included
// deliberately: remote_servers and the -host inventories include self.
//
// Invariant: a lease-mode spec must carry writer_max (the Terraform
// render writes it into every role's spec — reader CR slots derive
// from it); with it unset the reader offset degenerates to WriterCount.
func claimsInventory(b *Boot, claims []port.Claim) []domain.HostIdentity {
	writerPrefix, readerPrefix := b.Spec.WriterPrefix(), b.Spec.ReaderPrefix()
	ceiling := b.Spec.WriterCeiling()
	readerBound := b.Spec.ReaderCeiling()
	var hosts []domain.HostIdentity
	for _, claim := range claims {
		ordinal := -1
		role := domain.Role("")
		switch {
		case strings.HasPrefix(claim.Hostname, writerPrefix):
			if n, err := strconv.Atoi(strings.TrimPrefix(claim.Hostname, writerPrefix)); err == nil && n >= 0 && (ceiling <= 0 || n < ceiling) {
				ordinal, role = n, domain.RoleWriter
			}
		case strings.HasPrefix(claim.Hostname, readerPrefix):
			if n, err := strconv.Atoi(strings.TrimPrefix(claim.Hostname, readerPrefix)); err == nil && n >= 0 && (readerBound <= 0 || n < readerBound) {
				ordinal, role = ceiling+n, domain.RoleReader
			}
		}
		if ordinal < 0 {
			continue // foreign row (backup-0, out-of-ceiling stale claim)
		}
		hosts = append(hosts, domain.HostIdentity{
			Cloud:   b.Provider.Name(),
			Region:  b.Meta.Region,
			Name:    fmt.Sprintf("01-%d", ordinal),
			Address: fmt.Sprintf("%s.%s", claim.Hostname, b.Spec.DNSZoneName),
			Role:    role,
		})
	}
	sort.SliceStable(hosts, func(i, j int) bool {
		// Claim keys are unique (hostname is the table key), so Name
		// which encodes the slot — is a total order.
		return hosts[i].Name < hosts[j].Name
	})
	// The backup replica holds no lease (static identity) but IS a CR
	// member — append it when always-on, or on the backup node itself so
	// its own CR slot renders macros/zookeeper. When backup.on_demand is
	// true, the backup EC2 is stopped outside its scheduled backup window,
	// so omitting it on writers/readers prevents clusterAllReplicas()
	// queries (health, dead-replica-gc, schema-sync) from stalling on TCP
	// SYN timeouts against the stopped instance's private IP.
	if b.Spec.Backup.BackupReplicaHostname != "" && (!b.Spec.Backup.OnDemand || b.Spec.Role == model.RoleBackup) {
		ordinal := ceiling + b.Spec.ReaderCeiling()
		hosts = append(hosts, domain.HostIdentity{
			Cloud:   b.Provider.Name(),
			Region:  b.Meta.Region,
			Name:    fmt.Sprintf("01-%d", ordinal),
			Address: fmt.Sprintf("%s.%s", b.Spec.Backup.BackupReplicaHostname, b.Spec.DNSZoneName),
			Role:    domain.RoleBackup,
		})
	}
	return hosts
}

// staticSpecInventory enumerates from the spec's reserved lists — the
// pre-lease inventory shape, kept as the fallback for clouds without
// the listing capability (Azure interim) and static-identity paths.
// Leased specs (writer_max set) enumerate the reserved writer ordinals
// by prefix — the same shape hostInventory (the app package's -host
// renderer) uses, so the boot's remote_servers and the schema-sync
// flags agree — and readers enumerate to ReaderMax when set (a
// scale-out past the baseline is a member the moment it claims)
// falling back to ReaderCount; the reader slot offset is
// WriterCeiling, the same arithmetic path uses, so static
// and live inventories agree on CR slots.
func staticSpecInventory(b *Boot) []domain.HostIdentity {
	var hosts []domain.HostIdentity
	if b.Spec.WriterMax > 0 {
		prefix := b.Spec.WriterPrefix()
		for i := 0; i < b.Spec.WriterMax; i++ {
			name := fmt.Sprintf("%s%d", prefix, i)
			hosts = append(hosts, domain.HostIdentity{
				Cloud:   b.Provider.Name(),
				Region:  b.Meta.Region,
				Name:    fmt.Sprintf("01-%d", i),
				Address: fmt.Sprintf("%s.%s", name, b.Spec.DNSZoneName),
				Role:    domain.RoleWriter,
			})
		}
	} else {
		for i, name := range b.Spec.WriterNodes {
			hosts = append(hosts, domain.HostIdentity{
				Cloud:   b.Provider.Name(),
				Region:  b.Meta.Region,
				Name:    fmt.Sprintf("01-%d", i),
				Address: fmt.Sprintf("%s.%s", name, b.Spec.DNSZoneName),
				Role:    domain.RoleWriter,
			})
		}
	}
	ceiling := b.Spec.WriterCeiling()
	for i := 0; i < b.Spec.ReaderCeiling(); i++ {
		name := fmt.Sprintf("%s%d", b.Spec.ReaderPrefix(), i)
		hosts = append(hosts, domain.HostIdentity{
			Cloud:   b.Provider.Name(),
			Region:  b.Meta.Region,
			Name:    fmt.Sprintf("01-%d", ceiling+i),
			Address: fmt.Sprintf("%s.%s", name, b.Spec.DNSZoneName),
			Role:    domain.RoleReader,
		})
	}
	// The backup replica (static identity, no lease): its CR slot is the
	// first after the readers' ceiling — the same slot CRName derives
	// for the role and Terraform renders into replicasCount. Omitted on
	// writers/readers when backup.on_demand is true (the instance is
	// stopped outside its backup window) while still included on the
	// backup node itself so its own CR slot renders macros/zookeeper.
	if b.Spec.Backup.BackupReplicaHostname != "" && (!b.Spec.Backup.OnDemand || b.Spec.Role == model.RoleBackup) {
		ordinal := ceiling + b.Spec.ReaderCeiling()
		hosts = append(hosts, domain.HostIdentity{
			Cloud:   b.Provider.Name(),
			Region:  b.Meta.Region,
			Name:    fmt.Sprintf("01-%d", ordinal),
			Address: fmt.Sprintf("%s.%s", b.Spec.Backup.BackupReplicaHostname, b.Spec.DNSZoneName),
			Role:    domain.RoleBackup,
		})
	}
	return hosts
}

// withSubnet merges the node subnet CIDRs into a user's allowlist: the
// cluster's own peers, the NLB health checks and the config refresher
// all arrive from the nodes subnet(s) — without them every cross-node
// query is Code 516 Authentication failed. Multi-AZ renders carry the
// per-AZ extras beside the node's own subnet.
func withSubnet(networks []string, subnetCIDRs []string) []string {
	out := networks
	for _, subnetCIDR := range subnetCIDRs {
		if subnetCIDR == "" {
			continue
		}
		found := false
		for _, n := range out {
			if n == subnetCIDR {
				found = true
				break
			}
		}
		if found {
			continue
		}
		next := make([]string, 0, len(out)+1)
		next = append(next, out...)
		out = append(next, subnetCIDR)
	}
	return out
}

// configureKeeper renders the keeper single-file config, enables the
// keeper service, and clears stale coordination state (prevents
// split-brain on re-image). Keeper nodes skip the server start.
func (b *Boot) configureKeeper(ctx context.Context) error {
	if b.Spec.Role != model.RoleKeeper {
		return nil
	}
	if b.Identity.Hostname == "" && b.Meta.PrivateIP != "" {
		if h, ord := StaticHostname(b.Meta.PrivateIP); h != "" {
			b.Identity.Hostname = h
			b.Identity.CROrdinal = ord
		}
	}
	// RENDER the complete keeper config from the CHK spec (the same
	// single-file chctl path the bash era ran): keeper launches with
	// an explicit --config so config.d auto-include is DISABLED
	// without this render the node runs whatever stale config the
	// image or a previous era left behind (chinit-cutover
	// keeper listened on 127.0.0.1 because the root-level
	// listen_host from the spec's files: section never rendered).
	cr, err := b.LoadCHK(ctx)
	if err != nil {
		return Fatal(err)
	}
	// Keeper CR hosts are 0-0..0-(N-1) in keeper_nodes order; THIS
	// node's ordinal is its position in that list.
	myOrdinal := -1
	for i, name := range b.Spec.KeeperNodes {
		if name == b.Identity.Hostname {
			myOrdinal = i
			break
		}
	}
	if myOrdinal < 0 {
		return Fatal(fmt.Errorf("keeper %s not in spec keeper_nodes %v", b.Identity.Hostname, b.Spec.KeeperNodes))
	}
	// server_id: the raft disk's myid file is authoritative across
	// re-images (ensemble identity lives on persistent state) — but
	// NEVER 0: keeper ids are 1-based, a stale 0 myid (from a broken
	// boot) would re-introduce the Code 568 mismatch; fall through to
	// the seeded 1-based value and rewrite the file. A fresh disk
	// seeds the ordinal+1.
	serverID := myOrdinal + 1
	myidPath := "/var/lib/clickhouse-keeper/log/myid"
	if data, err := os.ReadFile(myidPath); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n > 0 {
			serverID = n
		}
	}
	if err := os.MkdirAll(filepath.Dir(myidPath), 0755); err == nil {
		_ = os.WriteFile(myidPath, []byte(fmt.Sprintf("%d", serverID)), 0644)
	}
	// Keeper addresses + raft ids: every ensemble member's cloud DNS,
	// with 1-BASED raft ids matching the server_id/myid convention
	// the CR's zero-based replica indices render <id>0</id> and keeper
	// refuses to start (Code 568: our server id N not found in
	// raft_configuration — server_id is 1-based by the myid disk
	// convention;).
	keeperAddrs := map[string]string{}
	raftIDs := map[string]int{}
	for i, name := range b.Spec.KeeperNodes {
		keeperAddrs[fmt.Sprintf("0-%d", i)] = fmt.Sprintf("%s.%s", name, b.Spec.DNSZoneName)
		raftIDs[fmt.Sprintf("0-%d", i)] = i + 1
	}
	renderer := operator.NewNodeRenderer(operator.NewConfigRenderer(), nil)
	_, err = renderer.RenderNode(ctx, port.RenderNodeRequest{
		NormalizedCR:    cr,
		NodeType:        string(b.Spec.Role),
		HostName:        fmt.Sprintf("0-%d", myOrdinal),
		Keeper:          true,
		SingleFilePath:  "/etc/clickhouse-keeper/keeper_config.xml",
		KeeperAddresses: keeperAddrs,
		RaftIDs:         raftIDs,
		ServerID:        serverID,
		OutputDir:       "/etc/clickhouse-keeper",
	})
	if err != nil {
		return Fatal(fmt.Errorf("keeper render: %w", err))
	}
	// Stale coordination state: a re-image with a live raft log wedges
	// the ensemble — the bash script wiped coordination/* deliberately.
	// BOTH path families exist: the package default renders
	// /var/lib/clickhouse/coordination/{logs,snapshots}; the stack's
	// keeper spec overrides to /var/lib/clickhouse-keeper/{log,snapshots}.
	// Clear and prepare BOTH so whichever the rendered config uses, the
	// keeper (running as user clickhouse) can write it — a root-owned
	// coordination dir is exit code 76 CANNOT_OPEN_FILE Permission
	// denied (the dir was created
	// AFTER the mount step's chown ran, as root, and never re-owned).
	//
	// Refresh mode MUST NOT wipe: the node is a LIVE ensemble member and
	// the raft log/snapshots are its vote. Wiping on a refresh would
	// amputate the member from quorum. (First boot on a fresh disk has
	// nothing to wipe anyway.)
	if !b.Refresh {
		for _, base := range []string{
			"/var/lib/clickhouse/coordination",
			"/var/lib/clickhouse-keeper",
		} {
			if err := os.RemoveAll(filepath.Join(base, "coordination")); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clear keeper coordination state: %w", err)
			}
		}
	}
	for _, dir := range []string{
		"/var/lib/clickhouse/coordination/logs",
		"/var/lib/clickhouse/coordination/snapshots",
		"/var/lib/clickhouse-keeper/log",
		"/var/lib/clickhouse-keeper/snapshots",
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	out, err := runCommand(ctx, "chown", "-R", "clickhouse:clickhouse",
		"/var/lib/clickhouse/coordination", "/var/lib/clickhouse-keeper")
	if err != nil {
		return fmt.Errorf("chown keeper state dirs: %w: %s", err, out)
	}
	if b.Refresh {
		// A refresh must make the (possibly re-rendered) config take effect;
		// enable --now would leave the running keeper on the old config.
		// Raft state is on disk — a restart rejoins the ensemble.
		if out, rerr := runCommand(ctx, "systemctl", "restart", "clickhouse-keeper"); rerr != nil {
			return fmt.Errorf("restart clickhouse-keeper after config refresh: %w: %s", rerr, out)
		}
	} else {
		out, err = runCommand(ctx, "systemctl", "enable", "--now", "clickhouse-keeper")
		if err != nil {
			return fmt.Errorf("enable clickhouse-keeper: %w: %s", err, out)
		}
	}
	// clickhouse-server must NOT run on a keeper: keepers skip
	// render_node_config by design (configure_keeper owns the keeper
	// render), so no users.d ever renders and the server would serve
	// with the package's stock users. The deb's systemd preset enables
	// the unit anyway (keeper-0 ran a
	// clickhouse-server answering AUTHENTICATION_FAILED for every
	// spec user — the "empty users.d" reports were the SERVER running
	// where it never should have). Runs on BOTH branches — first
	// boot AND refresh: a pre-0.2.23 keeper that upgrades via
	// in-place refresh keeps its stray server until this converges
	// it (the refresh branch returned
	// early and the keeper's server kept answering stock users).
	if out, derr := runCommand(ctx, "systemctl", "disable", "--now", "clickhouse-server"); derr != nil {
		return fmt.Errorf("disable clickhouse-server on keeper: %w: %s", derr, out)
	}
	return nil
}

// startClickhouse enables and starts the server, waits for /ping, and
// verifies the S3 disk is not broken. Keepers return immediately
// (clickhouse-server on a keeper node is wrong AND fails on
// keeper-only configs).
func (b *Boot) startClickhouse(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil
	}

	// First-boot metadata cleanup (UNKNOWN_DISK avoidance): a blank
	// metadata disk with stale system tables from image-baking wedges
	// the first start. Skipped on refresh — wiping /var/lib/clickhouse
	// on a live node would delete the very data the node serves.
	marker := "/var/lib/clickhouse/first_boot_done"
	if b.Refresh {
		// fall through to the restart below
	} else if _, err := os.Stat(marker); os.IsNotExist(err) {
		_, _ = runCommand(ctx, "systemctl", "stop", "clickhouse-server")
		_ = os.RemoveAll("/var/lib/clickhouse/store")
		_ = os.RemoveAll("/var/lib/clickhouse/data/system")
		if err := os.MkdirAll("/var/lib/clickhouse/store", 0755); err != nil {
			return err
		}
		_, _ = runCommand(ctx, "chown", "-R", "clickhouse:clickhouse", "/var/lib/clickhouse/store")
		if err := os.WriteFile(marker, []byte("done"), 0644); err != nil {
			return err
		}
	}

	// The golden AMI bakes the distro unit masked (the random-package-uuid
	// boot race — see the packer template comment): enable silently
	// no-ops on a masked unit, so unmask before the enable.
	_, _ = runCommand(ctx, "systemctl", "unmask", "clickhouse-server")
	if out, err := runCommand(ctx, "systemctl", "enable", "clickhouse-server"); err != nil {
		return Fatal(fmt.Errorf("enable clickhouse-server: %w: %s", err, out))
	}
	if b.Refresh {
		// Refresh: make the re-rendered config take effect. restart
		// (not start) so a running server re-reads users.d/config.d.
		if out, err := runCommand(ctx, "systemctl", "restart", "clickhouse-server"); err != nil {
			return Fatal(fmt.Errorf("restart clickhouse-server: %w: %s", err, out))
		}
	} else if out, err := runCommand(ctx, "systemctl", "start", "clickhouse-server"); err != nil {
		return Fatal(fmt.Errorf("start clickhouse-server: %w: %s", err, out))
	}

	// /ping wait: the retry policy at the step level retries the whole
	// function; the ping loop is the bash wait_for_service shape.
	if err := waitForHTTP(ctx, "http://localhost:8123/ping", 30, 2); err != nil {
		return Fatal(fmt.Errorf("clickhouse-server did not become ready: %w", err))
	}

	// S3 disk health probe (a failed disk health check at boot marks
	// the S3 disk broken and excludes it — surfacing it in the boot log
	// is the diagnostic the bash script printed).
	out, _ := execAdminQuery(ctx, "SELECT name, is_broken FROM system.disks WHERE name = 's3'")
	if strings.Contains(out, "\t1") {
		logger.GetLogger(ctx).Warn("WARNING: S3 disk is marked broken — check the instance role's s3:GetObject/PutObject on the data bucket")
	}
	return nil
}

// bootstrapSchema executes the bundle's bootstrap-ddl.sql idempotently
// on server nodes — the durable home for the fleet's schema (the
// full-churn when every peer churns within one
// refresh, schema-sync has no peer to propagate from and the fleet
// stays schemaless until a manual DDL session recovers it from keeper
// metadata; the DDL now ships with the config bundle so every node
// re-materializes its own tables at boot).
//
// The DDL's {shard}/{replica} are CLICKHOUSE macros — the server
// substitutes them from chop-generated-macros.xml at statement time,
// so this function passes the file through UNCHANGED. Every statement
// is IF NOT EXISTS: a node that already holds the schema no-ops.
//
// BestEffort by design: a fleet with pre-existing znode drift (replica
// name conflicts from prior generations) fails a CREATE with
// REPLICA_ALREADY_EXISTS — that must not wedge the boot (the schema-sync
// unit and the health gate remain the heal paths); the failure is loud
// in the boot log for triage.
// bootstrapSchema is the boot step; the convergence body is
// ApplyBootstrapDDL (the `chinit schema-bootstrap` subcommand runs the
// same pass on its timer — reader-0 exhausted
// the boot's 5 apiRetry attempts while the keeper rolled in the same
// refresh and stayed schemaless for HOURS because nothing retried the
// bundle DDL after the boot gave up).
func (b *Boot) bootstrapSchema(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil // keepers carry no tables
	}
	result, err := ApplyBootstrapDDL(ctx, b.Identity.Hostname, string(b.Spec.Role))
	if err != nil {
		return err
	}
	logger.GetLogger(ctx).Info("schema bootstrap: statements applied",
		"applied", result.Created,
		"ddl_path", result.DDLPath,
		"failed", result.Failed)
	// A retryable error surfaces when anything failed: the statement set
	// is idempotent (IF NOT EXISTS + the drop-own-znode conflict path),
	// so a re-run converges instead of wedging. BestEffort classification
	// still keeps a final-pass failure from killing the boot — the
	// schema-sync unit and health gate remain the heal paths.
	if result.Failed > 0 {
		return fmt.Errorf("schema bootstrap: %d statement(s) failed (retrying; transient keeper races converge on the next pass)", result.Failed)
	}
	// Post-DDL reconciliation: a cas_config.enabled toggle lands as a
	// storage.xml render BEFORE the server restarts — the on-disk table
	// metadata (and ATTACHed live tables) must match the new policy set,
	// or the restart Code 452s on tiered_cas tables whose disks vanished.
	reconcileStoragePolicyMetadata(ctx)
	return nil
}

// DDLApplyResult reports one convergence pass of the bundle DDL.
type DDLApplyResult struct {
	DDLPath string
	Created int
	Failed  int
	// Skipped counts statements the role filter dropped (the backup's
	// staging-pipeline objects — Code 499 AccessDenied on the queue's
	// poll).
	Skipped int
}

// bootstrapDDLPath and bootstrapAdminPasswordPath are the path seams
// ApplyBootstrapDDL reads (tests redirect; production binds the
// bundle's constants — the peerHostsPath pattern).
var (
	bootstrapDDLPath           = func() string { return filepath.Join(BootstrapDir, "bootstrap-ddl.sql") }
	bootstrapAdminPasswordPath = func() string { return BootstrapDir + "/admin-password" }
)

// ApplyBootstrapDDL runs one idempotent pass of the bundle's
// bootstrap-ddl.sql on this node: IF NOT EXISTS creates, the
// Code 253 stale-znode drop-and-retry, and a failed count the caller
// turns into a retryable error. It is the shared body of the boot's
// bootstrap_schema step and the schema-bootstrap timer unit — the
// heal path for a boot that lost the keeper race. Statements tagged
// "-- chinit:roles=..." for OTHER roles are skipped (the staging
// S3Queue + MV are writer/reader-only: the backup's IAM role cannot
// list staging).
func ApplyBootstrapDDL(ctx context.Context, hostname, role string) (*DDLApplyResult, error) {
	if hostname == "" {
		if h, err := os.Hostname(); err == nil {
			hostname = h
		}
	}
	ddlPath := bootstrapDDLPath()
	data, err := os.ReadFile(ddlPath)
	if err != nil {
		// A bundle without the DDL is the pre-0.2.14 image shape
		// loud no-op, never fatal (the schema-sync path remains).
		logger.GetLogger(ctx).Warn("WARNING: bootstrap-ddl.sql not in the bundle (older AMI?) — schema bootstrap skipped", "err", err)
		return &DDLApplyResult{DDLPath: ddlPath}, nil
	}
	pw, err := os.ReadFile(bootstrapAdminPasswordPath())
	if err != nil {
		return nil, fmt.Errorf("read admin password file: %w", err)
	}
	adminPw := strings.TrimSpace(string(pw))
	result := &DDLApplyResult{DDLPath: ddlPath}

	// Statement-split: one statement per TOP-LEVEL terminator. The DDL
	// carries SQL comments (-- headers explaining each object) and
	// parenthesized defaults like CODEC(ZSTD(1)) / toIntervalDay(7);
	// a naive strings.Split(";") shreds both — the first statement
	// would arrive at the server still prefixed by the comment text
	// ("Code 62 at position 1"), and a CODEC's own paren splits
	// mid-expression.
	// leaving a churned fleet WITHOUT its S3Queue drain surface).
	// The splitter is comment-aware (line comments removed before
	// splitting; ClickHouse DDL in this bundle uses no block comments)
	// and delimiter-depth-aware (semicolons inside parentheses
	// functions, codecs, tuples — never terminate a statement).
	statements := splitSQLStatements(string(data))
	for _, tagged := range statements {
		s := strings.TrimSpace(tagged.sql)
		if !isDDLStatement(s) {
			continue
		}
		// Role-scoped statements (the staging pipeline is
		// writer/reader-only): skipped BEFORE reaching the server
		// a node whose IAM cannot serve the object must not
		// materialize it (the backup's queue spewed Code 499
		// AccessDenied on every poll). The filter covers ALTERs too:
		// a role-scoped table's settings only converge on the roles
		// that own the table.
		if !tagged.appliesToRole(role) {
			result.Skipped++
			continue
		}
		// S3Queue keeper-path AND endpoint convergence (the
		// 2026-11-17 fan-out fix + the Terraform-rendered URL): a
		// CREATE declaring keeper_path against a LIVE queue table
		// carrying a DIFFERENT path must DROP + re-create once —
		// 26.8 refuses MODIFY SETTING keeper_path for unordered mode
		// (Code 344 SUPPORT_IS_DISABLED), and without the explicit
		// path every node's CREATE IF NOT EXISTS invents its own
		// UUID-derived claim tree (the live finding: 5 writers, 5
		// trees, 4-of-5 claim races per file, keeper at 760k znodes).
		// The same convergence covers ENDPOINT drift once the URL is
		// rendered from storage_config instead of shipped as a literal
		// (the gateway-endpoint audit): a live queue still pointing at
		// an old bucket/region must re-create, or a bucket rename /
		// region move would leave writers draining the old prefix
		// forever while the bundle's URL no-ops behind CREATE IF NOT
		// EXISTS. Safe ONLY for the queue-table shape: S3Queue holds
		// no data (after_processing=delete emptied the prefix as
		// files were ingested); the old tree ages out via
		// tracked_file_ttl_sec; abandoned in-flight claims re-list and
		// are re-claimed under the shared path — exactly-once
		// preserved. A non-S3Queue CREATE never takes this path (a
		// data table would be dropped).
		if s3QueueDrift(ctx, adminPw, s) {
			if !convergeS3QueuePath(ctx, adminPw, s, result) {
				continue
			}
		}
		convergeTableStoragePolicy(ctx, adminPw, s)
		out, err := execClient(ctx, adminPw, s)
		if err == nil {
			result.Created++
			continue
		}
		// The Code 253 stale-znode conflict is CREATE-only; an ALTER
		// has no replica conflict shape (dropping the node's znode
		// over a settings miss would be destructive). A failed ALTER
		// counts as failed and the next pass converges after the
		// CREATE lands.
		if strings.Contains(s, "CREATE") && isStaleReplicaConflict(out, err) {
			if applyConflictRetry(ctx, adminPw, s, hostname, result) {
				continue
			}
		}
		// Other failures log and continue — but they COUNT: the
		// step returns an error when any statement failed so the
		// step-level apiRetry re-runs the whole idempotent pass
		// (keeper race: the CREATEs failed
		// Code 999 — ZooKeeper connection refused while the
		// keeper rolled in the same refresh — and the step
		// "succeeded" on the one statement that did not need
		// keeper, never retrying against the recovered keeper).
		result.Failed++
		logger.GetLogger(ctx).Warn("WARNING: bootstrap DDL statement failed (continuing)",
			"err", err, "out", strings.TrimSpace(out))
	}
	return result, nil
}

// isDDLStatement answers whether one split statement is executable DDL.
// The pass runs CREATEs AND ALTERs: CREATE IF NOT EXISTS can never
// converge SETTINGS on an existing table, so the bundle carries
// idempotent ALTER ... MODIFY SETTING statements after the CREATEs —
// without them a settings change applied live (ALTER-first per the
// Code 342 contract) never reaches replicas the CREATE path skipped,
// and a churned fleet re-materializes with the OLD settings while the
// CREATE silently no-ops (the 2026-11-17 three-way split: writer-3 at
// free_entries=12, writer-2/reader-1 at =4, no node at the queue-gate
// fixes). The ALTERs close the loop: every pass re-asserts the
// declared settings until live matches.
// idempotent ALTER ... MODIFY SETTING statements after the CREATEs —
// without them a settings change applied live (ALTER-first per the
// Code 342 contract) never reaches replicas the CREATE path skipped,
// and a churned fleet re-materializes with the OLD settings while the
// CREATE silently no-ops (the 2026-11-17 three-way split: writer-3 at
// free_entries=12, writer-2/reader-1 at =4, no node at the queue-gate
// fixes). The ALTERs close the loop: every pass re-asserts the
// declared settings until live matches.
//
// DROP IF EXISTS rides the same pass (the reader-queue convergence):
// the staging pipeline's marker changed from writer,reader to writer,
// and the bundle carries a reader/backup-scoped DROP pair that
// removes the queue a reader materialized under the old tag (the
// reader-OOM class — readers claimed ingest files through the shared
// keeper tree and OOM'd on them). IF EXISTS makes it idempotent: a
// node without the table answers clean, and a writer never runs it
// (the role filter drops the pair before the server ever sees it).
// Deliberately NOT a blanket "DROP" — the verb is pinned to the
// IF EXISTS form so a hand-edited bundle can never express a
// destructive bare DROP.
func isDDLStatement(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, "DROP TABLE IF EXISTS") {
		return true
	}
	return strings.Contains(s, "CREATE") || strings.Contains(s, "ALTER")
}

// s3QueueDrift reports whether THIS statement is an S3Queue CREATE
// whose LIVE table (if any) carries a different declared keeper_path
// or a different S3 endpoint than the bundle declares. The probe is
// one system.tables lookup — cheap, and only for statements that
// declare at least one of the two (a bundle declaring neither never
// drifts and never probes). Values are extracted with the same
// literal-quote shape the bundle writes
// ('/clickhouse/s3queue/staging_synth',
// 'https://<bucket>.s3.<region>.amazonaws.com/<prefix>/*/*'); a
// statement whose table name cannot be parsed answers false (the
// convergence is opt-in and parse-verified, never guessed).
func s3QueueDrift(ctx context.Context, adminPw, stmt string) bool {
	if !strings.Contains(stmt, "S3Queue") || !strings.Contains(stmt, "CREATE") {
		return false
	}
	// The table name must parse for the probe to have a target; the
	// keeper_path is OPTIONAL (a URL-only drift still converges —
	// the URL is rendered from Terraform variables, so a bucket
	// rename moves the declared URL while the keeper_path stays).
	declaredPath, table, hasPath := s3QueueDeclaredPath(stmt)
	if table == "" {
		return false
	}
	declaredURL, hasURL := extractS3QueueEndpoint(stmt)
	if !hasPath && !hasURL {
		return false
	}
	out, err := execClient(ctx, adminPw,
		fmt.Sprintf("SELECT engine_full FROM system.tables WHERE database = 'default' AND name = '%s'", table))
	if err != nil || strings.TrimSpace(out) == "" {
		// No live table (or the probe failed): the CREATE is the
		// convergence itself — fall through and run it.
		return false
	}
	// Endpoint drift (the Terraform-rendered URL): the declared S3
	// URL is the first quoted literal in the S3Queue(...) engine
	// args; the live engine_full carries the live URL the same way.
	// A mismatch means the bucket/prefix/region moved under the
	// bundle — the queue must re-create, not no-op.
	if hasURL {
		liveURL, liveURLOk := extractS3QueueEndpoint(out)
		if !liveURLOk || liveURL != declaredURL {
			return true
		}
	}
	// Keeper-path drift: extract the live keeper_path from the
	// rendered engine_full (the SHOW CREATE output carries SETTINGS
	// keeper_path = '...').
	if hasPath {
		livePath, ok := extractQuotedSetting(out, "keeper_path")
		if ok && livePath != declaredPath {
			return true
		}
	}
	return false
}

// convergeS3QueuePath runs the one-time DROP + CREATE for a
// drifted S3Queue table. It reports whether the CREATE may proceed
// (the drop succeeded or was unnecessary); false means the drop
// failed and the caller logs the CREATE failure instead (the next
// pass retries — idempotent by construction).
func convergeS3QueuePath(ctx context.Context, adminPw, stmt string, result *DDLApplyResult) bool {
	// The table name parses from the CREATE header alone (URL-drift
	// convergence runs even when the bundle declares no keeper_path).
	_, table, _ := s3QueueDeclaredPath(stmt)
	if table == "" {
		return false
	}
	logger.GetLogger(ctx).Warn("schema bootstrap: S3Queue keeper_path/endpoint drift — re-creating the queue table on the declared URL and shared claim path",
		"table", table)
	out, err := execClient(ctx, adminPw, fmt.Sprintf("DROP TABLE IF EXISTS default.%s", table))
	if err != nil {
		result.Failed++
		logger.GetLogger(ctx).Warn("WARNING: S3Queue path-convergence drop failed (continuing)",
			"table", table, "err", err, "out", strings.TrimSpace(out))
		return false
	}
	return true
}

// s3QueueDeclaredPath extracts (declared keeper_path, table name)
// from a CREATE TABLE ... ENGINE = S3Queue(...) ... keeper_path =
// '...' statement. ok (the third return) is false when the statement
// declares no explicit path — but the table name is still parsed so
// URL-only drift can converge (the URL is rendered from Terraform
// variables; a bucket rename moves it while the keeper_path stays).
// The table name is "" only when it cannot be parsed at all.
func s3QueueDeclaredPath(stmt string) (path, table string, ok bool) {
	var hasPath bool
	path, hasPath = extractQuotedSetting(stmt, "keeper_path")
	if idx := strings.Index(stmt, "CREATE TABLE"); idx >= 0 {
		rest := stmt[idx+len("CREATE TABLE"):]
		// TrimSpace BEFORE the IF NOT EXISTS strip: the remainder
		// carries a leading space, and TrimPrefix is
		// whitespace-sensitive.
		rest = strings.TrimSpace(rest)
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "IF NOT EXISTS"))
		// The (possibly qualified) name runs to the first '(' or
		// whitespace-terminated token before it.
		rest = strings.TrimSpace(rest)
		end := len(rest)
		for i := 0; i < len(rest); i++ {
			if rest[i] == '(' || unicode.IsSpace(rune(rest[i])) {
				end = i
				break
			}
		}
		table = strings.Trim(rest[:end], "`\"")
		if table != "" {
			// Strip a database qualifier — the convergence drop runs
			// against default.<name> (this bundle's shape).
			if i := strings.LastIndex(table, "."); i >= 0 {
				table = table[i+1:]
			}
			return path, table, hasPath
		}
	}
	return "", "", hasPath
}

// extractQuotedSetting pulls a `name = 'value'` literal out of a DDL
// fragment (the bundle's SETTINGS shape). ok=false when absent.
// ClickHouse's non-interactive default (TabSeparated) escapes quotes inside
// system.tables.engine_full as \' — normalize first so live probes and raw
// DDL strings parse identically.
func extractQuotedSetting(stmt, name string) (string, bool) {
	stmt = strings.ReplaceAll(stmt, `\'`, `'`)
	idx := strings.Index(stmt, name)
	if idx < 0 {
		return "", false
	}
	rest := stmt[idx+len(name):]
	rest = strings.TrimLeft(rest, " \t\r\n")
	if !strings.HasPrefix(rest, "=") {
		return "", false
	}
	rest = strings.TrimLeft(rest[1:], " \t\r\n")
	if !strings.HasPrefix(rest, "'") {
		return "", false
	}
	end := strings.IndexByte(rest[1:], '\'')
	if end < 0 {
		return "", false
	}
	return rest[1 : end+1], true
}

// extractS3QueueEndpoint pulls the first quoted literal in the
// S3Queue(...) engine args — the S3 URL the queue polls. It anchors on
// "S3Queue(" so an embedded setting named before the engine args
// cannot shadow it (the URL is always the FIRST engine argument).
// Normalizes TabSeparated \' escapes from clickhouse-client output so
// live system.tables probes do not falsely report endpoint drift every tick.
// ok=false when no S3Queue( anchor or no quoted literal follows it.
func extractS3QueueEndpoint(stmt string) (string, bool) {
	stmt = strings.ReplaceAll(stmt, `\'`, `'`)
	idx := strings.Index(stmt, "S3Queue(")
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimLeft(stmt[idx+len("S3Queue("):], " \t\r\n")
	if !strings.HasPrefix(rest, "'") {
		return "", false
	}
	end := strings.IndexByte(rest[1:], '\'')
	if end < 0 {
		return "", false
	}
	return rest[1 : end+1], true
}

// isStaleReplicaConflict reports whether an exec failure is ClickHouse
// Code 253 REPLICA_ALREADY_EXISTS — the NORMAL churn case: this node's
// replica name matches a znode left by its predecessor (the
// recycled-ordinal generation), and CREATE refuses to attach over it.
func isStaleReplicaConflict(out string, err error) bool {
	return strings.Contains(out, "Code: 253") || strings.Contains(err.Error(), "Code: 253")
}

// reconcileStoragePolicyMetadata aligns on-disk table metadata (.sql files
// under /var/lib/clickhouse/store and /var/lib/clickhouse/metadata) with
// whether /etc/clickhouse-server/config.d/storage.xml enables CAS
// (<metadata_type>cas</metadata_type>) before clickhouse-server starts or
// restarts. This allows toggling cas_config.enabled in Terraform without
// leaving persistent disks (e.g. backup-0 or in-place refreshes) pinned to
// the previous storage_policy ('tiered' <-> 'tiered_cas').
func reconcileStoragePolicyMetadata(ctx context.Context) {
	storageXML, err := os.ReadFile(filepath.Join(ConfigDir, "storage.xml"))
	if err != nil {
		return
	}
	casActive := strings.Contains(string(storageXML), "<metadata_type>cas</metadata_type>")
	fromPolicy, toPolicy := "storage_policy = 'tiered_cas'", "storage_policy = 'tiered'"
	if casActive {
		fromPolicy, toPolicy = "storage_policy = 'tiered'", "storage_policy = 'tiered_cas'"
	}
	patterns := []string{
		"/var/lib/clickhouse/store/*/*/*.sql",
		"/var/lib/clickhouse/metadata/*/*.sql",
	}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, p := range matches {
			info, statErr := os.Lstat(p)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			raw, readErr := os.ReadFile(p)
			if readErr != nil || !strings.Contains(string(raw), fromPolicy) {
				continue
			}
			updated := strings.ReplaceAll(string(raw), fromPolicy, toPolicy)
			if writeErr := os.WriteFile(p, []byte(updated), info.Mode().Perm()); writeErr == nil {
				logger.GetLogger(ctx).Info("reconciled table metadata storage_policy on disk",
					"path", p, "cas_active", casActive)
			}
		}
	}
}

// convergeTableStoragePolicy detects when a CREATE TABLE statement in
// bootstrap-ddl.sql declares a different storage_policy ('tiered' vs
// 'tiered_cas') than an already-existing live table and converges the
// local table's metadata file + attachment so CREATE TABLE IF NOT EXISTS
// does not silently leave the table on the old storage policy.
func convergeTableStoragePolicy(ctx context.Context, adminPw, stmt string) {
	if !strings.Contains(stmt, "CREATE TABLE") || !strings.Contains(stmt, "storage_policy") {
		return
	}
	declaredPolicy, ok := extractQuotedSetting(stmt, "storage_policy")
	if !ok || declaredPolicy == "" {
		return
	}
	_, table, _ := s3QueueDeclaredPath(stmt)
	if table == "" {
		return
	}
	out, err := execClient(ctx, adminPw,
		fmt.Sprintf("SELECT storage_policy, metadata_path FROM system.tables WHERE database = 'default' AND name = '%s'", table))
	if err != nil || strings.TrimSpace(out) == "" {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(out), "\t", 2)
	livePolicy := strings.TrimSpace(parts[0])
	if livePolicy == "" || livePolicy == declaredPolicy {
		return
	}
	var metaPath string
	if len(parts) > 1 {
		metaPath = strings.TrimSpace(parts[1])
	}
	if metaPath == "" {
		return
	}
	raw, readErr := os.ReadFile(metaPath)
	if readErr != nil {
		return
	}
	oldToken := fmt.Sprintf("storage_policy = '%s'", livePolicy)
	newToken := fmt.Sprintf("storage_policy = '%s'", declaredPolicy)
	if !strings.Contains(string(raw), oldToken) {
		return
	}
	logger.GetLogger(ctx).Info("schema bootstrap: converging live table storage_policy",
		"table", table, "from", livePolicy, "to", declaredPolicy)
	if _, err := execClient(ctx, adminPw, fmt.Sprintf("DETACH TABLE default.%s", table)); err != nil {
		return
	}
	updated := strings.ReplaceAll(string(raw), oldToken, newToken)
	_ = os.WriteFile(metaPath, []byte(updated), 0640)
	_, _ = execClient(ctx, adminPw, fmt.Sprintf("ATTACH TABLE default.%s", table))
	_, _ = execClient(ctx, adminPw, fmt.Sprintf("SYSTEM RESTART REPLICA default.%s", table))
}

// applyConflictRetry runs the stale-znode drop-then-retry for one
// conflicting CREATE: SYSTEM DROP REPLICA '<name>' FROM ZKPATH '<path>'
// clears the node's OWN stale registration (parts/queue znodes
// included — safe on a fresh disk whose parts do not exist), and the
// retried CREATE registers clean. It reports whether the retry path
// was taken (a non-replicated CREATE answers false and the caller's
// failure accounting applies).
func applyConflictRetry(ctx context.Context, adminPw, stmt, hostname string, result *DDLApplyResult) bool {
	tablePath, replicaName := ddlReplicaIdentity(stmt, hostname)
	if tablePath == "" || replicaName == "" {
		return false
	}
	dropSQL := fmt.Sprintf("SYSTEM DROP REPLICA '%s' FROM ZKPATH '%s'", replicaName, tablePath)
	if dOut, dErr := execClient(ctx, adminPw, dropSQL); dErr != nil {
		logger.GetLogger(ctx).Warn("WARNING: bootstrap DDL stale-replica drop failed",
			"table", tablePath, "err", dErr, "out", strings.TrimSpace(dOut))
		result.Failed++
	} else if rOut, rErr := execClient(ctx, adminPw, stmt); rErr != nil {
		logger.GetLogger(ctx).Warn("WARNING: bootstrap DDL statement failed after drop (continuing)",
			"err", rErr, "out", strings.TrimSpace(rOut))
		result.Failed++
	} else {
		result.Created++
		logger.GetLogger(ctx).Info("schema bootstrap: re-created after dropping stale znode", "table", tablePath)
	}
	return true
}

// ddlReplicaIdentity extracts the keeper path and replica name from one
// CREATE ... <ReplicatedEngine>('<zk-path>', '{replica}' | 'name') and
// the boot's settled hostname. Empty path answers "not a replicated
// CREATE" (the queue table, the MVs) — the caller's conflict-retry only
// applies to replicated tables. The '{replica}' macro form substitutes
// the hostname (the macros config renders per node); a literal replica
// name is taken as-is.
func ddlReplicaIdentity(stmt, hostname string) (path, replica string) {
	// The engine line: <Engine>('/clickhouse/tables/01/events', '{replica}')
	open := strings.Index(stmt, "('/clickhouse/tables/")
	if open < 0 {
		return "", ""
	}
	rest := stmt[open+1:]
	end := strings.Index(rest, "')")
	if end < 0 {
		return "", ""
	}
	parts := strings.SplitN(rest[:end], ",", 2)
	if len(parts) != 2 {
		return "", ""
	}
	path = strings.Trim(strings.TrimSpace(parts[0]), "'\"")
	// The DDL's engine args carry CLICKHOUSE macros — the server
	// substitutes them at statement time, but the SYSTEM DROP REPLICA
	// this identity feeds takes a LITERAL path: an unresolved
	// {shard} reaches the server as "/clickhouse/tables/{shard}/..."
	// and fails Code 305 "does not look like a table path" (the
	// reader-0 the stale-znode drop silently
	// no-op'd on every {shard}-macro CREATE, leaving the Code 253
	// conflict unhealed). Substitute from the node's rendered macros
	// config — the same file the server itself substitutes from.
	path = strings.ReplaceAll(path, "{shard}", shardMacroValue())
	replica = strings.Trim(strings.TrimSpace(parts[1]), "'\"")
	if replica == "{replica}" {
		replica = hostname
	}
	return path, replica
}

// shardMacroPath is the macros-config seam (tests redirect).
var shardMacroPath = func() string {
	return filepath.Join(ConfigDir, "chop-generated-macros.xml")
}

// shardMacroValue reads the node's <shard> macro from the rendered
// macros config. Empty when the file or element is absent — the
// {shard} substitution then leaves the macro untouched (the drop
// fails loudly at Code 305, not silently on a wrong path).
func shardMacroValue() string {
	data, err := os.ReadFile(shardMacroPath())
	if err != nil {
		return ""
	}
	// Minimal XML walk — the rendered macros config is flat and
	// machine-generated; encoding/xml is heavier than the file needs.
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v := strings.TrimPrefix(line, "<shard>"); v != line {
			if v = strings.TrimSuffix(v, "</shard>"); v != "" {
				return v
			}
		}
	}
	return ""
}

// splitSQLStatements splits a DDL file into statements on top-level
// semicolons: line comments (-- to end-of-line) are stripped first,
// and semicolons inside parentheses never terminate a statement.
//
// Why a dedicated function (not strings.Split): the bootstrap bundle's
// DDL carries explanatory comments and CODEC/interval defaults whose
// parentheses would otherwise shred statements into server-side
// syntax errors (Code 62) — the exact class the naive split produced
// live. The depth counter treats parentheses as the only nesting
// signal: single quotes are not tracked because the bundle carries no
// string literals containing semicolons or parens; a future bundle
// needing them should extend this with a quote state.
//
// ROLE MARKERS: a "-- chinit:roles=w,reader" comment line tags every
// statement AFTER it until the next marker (or EOF) as belonging to
// those roles only. The filter drops statements whose role list
// excludes this node BEFORE they reach the server. The
// backup-0 the staging S3Queue + its MV materialized on the
// backup replica, whose IAM role is deliberately data-empty (no
// staging ListBucket) — its queue instance spewed Code 499
// AccessDenied on every poll cycle. An empty role list (or no marker)
// applies to every role: the replicated tables the backup still needs.
func splitSQLStatements(sql string) []taggedStatement {
	var statements []taggedStatement
	var current strings.Builder
	depth := 0
	appliesTo := map[string]bool{}
	for _, line := range strings.Split(sql, "\n") {
		// Role markers are directives, not SQL: consumed before the
		// comment strip so "-- chinit:roles=..." on a comment line
		// still re-tags the block that follows.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			// The marker tolerates comment indentation ("-- chinit:..."
			// or "-- chinit:..."): strip the dashes and whitespace,
			// then match the directive.
			body := strings.TrimSpace(strings.TrimLeft(trimmed, "-"))
			if roles := strings.TrimPrefix(body, "chinit:roles="); roles != body {
				appliesTo = map[string]bool{}
				for _, r := range strings.Split(roles, ",") {
					if r = strings.TrimSpace(r); r != "" {
						appliesTo[strings.ToLower(r)] = true
					}
				}
			}
		}
		// Strip line comments outside parens: a "--" inside a string
		// literal would corrupt, but the bundle's comments live at line
		// starts and after whitespace only (the file's own convention).
		if idx := strings.Index(line, "--"); idx >= 0 && depth == 0 {
			line = line[:idx]
		}
		for _, r := range line {
			switch r {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			case ';':
				if depth == 0 {
					if stmt := current.String(); strings.TrimSpace(stmt) != "" {
						statements = append(statements, taggedStatement{sql: stmt, roles: appliesTo})
					}
					current.Reset()
					continue
				}
			}
			current.WriteRune(r)
		}
		current.WriteRune('\n')
	}
	if strings.TrimSpace(current.String()) != "" {
		statements = append(statements, taggedStatement{sql: current.String(), roles: appliesTo})
	}
	return statements
}

// taggedStatement is one DDL statement plus the roles its
// chinit:roles marker allows (nil/empty = every role).
type taggedStatement struct {
	sql   string
	roles map[string]bool
}

// appliesToRole reports whether the statement may run on this role.
// An empty role set is the default: the statement is role-universal
// (the replicated tables every server node needs).
func (t taggedStatement) appliesToRole(role string) bool {
	if len(t.roles) == 0 {
		return true
	}
	return t.roles[strings.ToLower(strings.TrimSpace(role))]
}

// execClient runs one SQL statement as admin over the local client.
// Credentials travel via CLICKHOUSE_USER/CLICKHOUSE_PASSWORD in the child
// environment rather than process argv so they are not exposed in /proc.
func execClient(ctx context.Context, adminPw, query string) (string, error) {
	if execClientFn != nil {
		return execClientFn(ctx, adminPw, query)
	}
	cmd := exec.CommandContext(ctx, "clickhouse-client", "--query", query)
	if adminPw != "" {
		cmd.Env = append(os.Environ(), "CLICKHOUSE_USER=admin", "CLICKHOUSE_PASSWORD="+adminPw)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("clickhouse-client: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// execAdminQuery reads the admin password file when present and runs query
// via execClient. Falls back to an unauthenticated local call when the
// password file does not exist (passwordless setups).
func execAdminQuery(ctx context.Context, query string) (string, error) {
	var adminPw string
	if pw, err := os.ReadFile(bootstrapAdminPasswordPath()); err == nil {
		adminPw = strings.TrimSpace(string(pw))
	}
	return execClient(ctx, adminPw, query)
}

// execClientFn is the exec seam (tests substitute; production runs the
// local clickhouse-client).
var execClientFn func(ctx context.Context, adminPw, query string) (string, error)

// healthGate runs the cluster health check as a library call — the
// operator's health service over the chctl probe (the same code path
// `chctl health` exercised; the binary is gone, the library remains).// Non-Fatal BY DESIGN (the step's Criticality is BestEffort): a reader
// booting while a peer is down must still join; the WARNING + report
// make a degraded boot loud in the logs.
//
// Self-healing: a degraded verdict now triggers one schema-sync retry
// pass with a backoff (bounded attempts) before the final report.
// The zombie-reader: a reader scaled out during a keeper
// restart, every peer query timed out, the boot recorded "degraded"
// and the node served NLB reads with an EMPTY database for hours
// degraded is a transient condition at boot (peers restarting), not
// a terminal one; the retry converts it back to healthy when the
// transient clears, and the final report still tells the truth when
// it doesn't.
func (b *Boot) healthGate(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil
	}
	report, err := b.runHealthCheck(ctx)
	if report != nil {
		logger.GetLogger(ctx).Info("cluster health verdict", "report", report.FormatHuman())
	}
	// Refresh restarts the server under a live cluster: a transient
	// degradation is EXPECTED while the node rejoins, and the boot-path
	// retry loop costs up to ~5 minutes of wall clock inside a 300s SSM
	// command. One probe, one report, no gate — the hourly schema-sync
	// and the node's own replication are the recovery path, and the
	// refresher's roll ordering already paces the fleet.
	if b.Refresh {
		if err != nil {
			logger.GetLogger(ctx).Warn("WARNING: post-refresh cluster health check degraded (node still serves; investigate via RUNBOOK_CLUSTER_HEALTH.md)", "err", err)
		}
		return nil
	}
	if err != nil && b.Spec.Role != model.RoleBackup {
		return b.retryGate(ctx, err)
	}
	// Degraded reports come back with err == nil (Check's contract:
	// the report IS , never an error) — the retry must key
	// on the report's status or a zombie reader (empty schema, peers
	// unreachable at first probe) sails through the gate on its FIRST
	// degraded verdict without ever running the schema-sync heal (the
	// live 0.2.4 one verdict line, zero retries).
	if report != nil && !report.Healthy() && b.Spec.Role != model.RoleBackup && !b.Refresh {
		return b.retryGate(ctx, fmt.Errorf("health report degraded: %s", report.Status))
	}
	if err != nil {
		return fmt.Errorf("post-boot cluster health check reported degradation (boot continues; investigate via RUNBOOK_CLUSTER_HEALTH.md): %w", err)
	}
	return nil
}

// retryGate runs the health gate's bounded schema-sync + re-probe loop
// (shared by the error path and the degraded-report path).
func (b *Boot) retryGate(ctx context.Context, firstErr error) error {
	err := firstErr
	for attempt := 1; attempt <= healthRetryAttempts && ctx.Err() == nil; attempt++ {
		logger.GetLogger(ctx).Warn("health gate degraded: retrying schema-sync before the final verdict",
			"attempt", attempt, "attempts", healthRetryAttempts)
		if serr := b.gateSchemaSync(ctx); serr != nil {
			logger.GetLogger(ctx).Warn("WARNING: health-gate schema-sync retry failed", "err", serr)
		}
		select {
		case <-time.After(healthRetryBackoff):
		case <-ctx.Done():
		}
		report, rerr := b.runHealthCheck(ctx)
		if report != nil {
			logger.GetLogger(ctx).Warn("cluster health verdict (retry)",
				"attempt", attempt, "report", report.FormatHuman())
		}
		// Success is a HEALTHY report, not a nil error: the degraded
		// path must keep healing until the verdict clears or the
		// attempts run out.
		if rerr == nil && report != nil && report.Healthy() {
			return nil
		}
		if rerr != nil {
			err = rerr
		}
	}
	return fmt.Errorf("post-boot cluster health check still degraded after %d schema-sync retries (boot continues; investigate via RUNBOOK_CLUSTER_HEALTH.md): %w",
		healthRetryAttempts, err)
}

// healthRetryAttempts/backoff bound the health-gate self-healing loop:
// enough to ride out a fleet roll or keeper restart (~4 minutes of
// retries), short enough to never wedge an ASG boot.
const (
	healthRetryAttempts = 3
	healthRetryBackoff  = 90 * time.Second
)

// runHealthCheck is the health gate's single probe pass.
func (b *Boot) runHealthCheck(ctx context.Context) (*HealthReport, error) {
	// The admin password file is the credentials source (the boot
	// resolved it in retrieve_passwords); the typed request carries
	// the value, never an argv.
	pw, err := os.ReadFile(BootstrapDir + "/admin-password")
	if err != nil {
		return nil, fmt.Errorf("read admin password file: %w", err)
	}
	// The health check requires the inventory (the same CR-name →
	// address map the render uses) and refuses to run without it.
	hosts, note := b.clusterHostsWithSource()
	if len(hosts) == 0 {
		return nil, fmt.Errorf("post-boot cluster health check skipped: host inventory is empty")
	}
	if note != "" {
		logger.GetLogger(ctx).Warn(note)
	}
	// The keeper check takes ONE mntr address (the ensemble's quorum
	// math lives in the keeper health cron, not here). Pinning the
	// ensemble's entrypoint exercises the reachability+mntr path;
	// per-member liveness is the deadline for the dedicated gate.
	keeperAddress := ""
	if len(b.Spec.KeeperNodes) > 0 {
		keeperAddress = fmt.Sprintf("%s.%s:9181", b.Spec.KeeperNodes[0], b.Spec.DNSZoneName)
	}
	cr, err := b.LoadCHI(ctx)
	if err != nil {
		return nil, fmt.Errorf("post-boot health check could not load the spec: %w", err)
	}
	tables := operator.ExpectedTablesFromCR(cr)
	svc := NewHealthService(operator.NewHealthProbe(), keeper.NewProbe(), func() []domain.TableRef { return tables })
	return svc.Check(ctx, HealthRequest{
		ClusterName:   b.Spec.ClusterName,
		Hosts:         hosts,
		Credentials:   domain.Credentials{Username: "admin", Password: strings.TrimSpace(string(pw))},
		Timeout:       DefaultHealthTimeout,
		KeeperAddress: keeperAddress,
		CheckTables:   true,
		SelfName:      b.Identity.Hostname,
	})
}

// gateSchemaSync re-runs the schema propagation in-process (the same
// service the schema-sync unit/timer drives — never a shell-out, so
// the gate's retry inherits the library's own retry semantics).
func (b *Boot) gateSchemaSync(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper || b.Spec.Role == model.RoleBackup {
		return nil
	}
	pw, err := os.ReadFile(BootstrapDir + "/admin-password")
	if err != nil {
		return fmt.Errorf("read admin password file: %w", err)
	}
	hosts, _ := b.clusterHostsWithSource()
	cr, err := b.LoadCHI(ctx)
	if err != nil {
		return fmt.Errorf("schema-sync retry could not load the spec: %w", err)
	}
	svc := NewSchemaService(operator.NewSchemaClientFactory())
	// The retry is the boot's own heal: a reader that booted mid-churn
	// holds no replica registrations worth pruning, but a REPLACEMENT
	// must drop its predecessor's stale row before the create or the
	// sync re-attaches the dead registration (Code 253).
	return svc.SyncSchema(ctx, SchemaRequest{
		ClusterName:  b.Spec.ClusterName,
		Credentials:  domain.Credentials{Username: "admin", Password: strings.TrimSpace(string(pw))},
		NormalizedCR: cr,
		Hosts:        hosts,
	}, domain.SchemaSyncRequest{
		HostName:       b.Identity.Hostname,
		Retries:        DefaultSchemaSyncRetries,
		PruneStaleSelf: true,
	})
}

var serverPrometheusPath = func() string { return filepath.Join(ConfigDir, "server-prometheus.xml") }

// prometheusReload reloads the config so the metrics endpoint comes up
// without a restart, and verifies the endpoint answers.
//
// Reload must authenticate as admin: on clusters where the default user is
// password-protected or restricted to a readonly profile, an unauthenticated
// clickhouse-client call always fails with AUTHENTICATION_FAILED and falls
// back to `systemctl restart clickhouse-server`, which can stall shutdown
// under active replication or merges.
func (b *Boot) prometheusReload(ctx context.Context) error {
	if b.Spec.Role == model.RoleKeeper {
		return nil
	}
	if _, err := os.Stat(serverPrometheusPath()); err != nil {
		return fmt.Errorf("server-prometheus.xml not found — config should have been rendered by chctl (spec files)")
	}
	if _, err := execAdminQuery(ctx, "SYSTEM RELOAD CONFIG"); err != nil {
		// The reload is a convenience; a restart covers it when reload fails.
		_, _ = runCommand(ctx, "systemctl", "restart", "clickhouse-server")
	}
	return waitForHTTP(ctx, "http://localhost:9363/metrics", 10, 2)
}

// runCommandFn is the test seam for runCommand.
var runCommandFn func(ctx context.Context, name string, args ...string) (string, error)

// runCommand is the local exec helper for service management (the
// injectable Runner lives in the system package for host ops; these
// step-level calls go through the same recording-friendly surface in
// tests via runCommandFn).
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	if runCommandFn != nil {
		return runCommandFn(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// waitForHTTP polls a URL until it answers or attempts run out (the
// wait_for_service shape, bounded).
func waitForHTTP(ctx context.Context, url string, attempts int, delaySec int) error {
	for i := 0; i < attempts; i++ {
		out, err := runCommand(ctx, "curl", "-s", "-o", "/dev/null", url)
		_ = out
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(delaySec) * time.Second):
		}
	}
	return fmt.Errorf("%s did not become ready after %d attempts", url, attempts)
}

// providerSecrets adapts the chinit provider (a port.SecretSource,
// GetSecretValue) to chctl's users-renderer SecretsResolver
// (ResolveSecret). Without this adapter the renderer receives a nil
// resolver and PANICS the first time the spec declares a passwordFrom
// user (nil dereference in adaptUser) — the renderer resolves
// password references at render time and cannot skip them.
type providerSecrets struct {
	p port.Provider
}

// ResolveSecret fetches one secret value by name via the cloud
// provider's secret source (instance role on AWS, Key Vault on Azure).
// chinit's Provider interface embeds chctl's port.SecretSource, so the
// request shape is shared.
func (a providerSecrets) ResolveSecret(ctx context.Context, request port.SecretRequest) (string, error) {
	return a.p.GetSecretValue(ctx, request)
}
