// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/apis/swversion"
	"github.com/altinity/clickhouse-operator/pkg/model/chi/schemer"
	"github.com/altinity/clickhouse-operator/pkg/model/clickhouse"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// SchemaRunner implements port.SchemaRunner over the operator's
// ClusterSchemer.
type SchemaRunner struct {
	// schemer is the operator client.
	schemer *schemer.ClusterSchemer
	// params are the cluster connection parameters (scheme, credentials,
	// port) for direct-SQL paths (DropReplica).
	params *clickhouse.ClusterConnectionParams
	// hosts maps cluster host names to operator Host values.
	hosts map[string]*v1.Host
	// clusterName is the ClickHouse cluster name from the factory
	// request; interpolated into the GC detection SQL only after being
	// confirmed to be one of the CR's declared clusters.
	clusterName string
}

// SchemaClientFactory implements port.SchemaClientFactory building
// operator-backed runners.
type SchemaClientFactory struct{}

// NewSchemaClientFactory builds the factory.
func NewSchemaClientFactory() *SchemaClientFactory {
	return &SchemaClientFactory{}
}

// defaultSchemaTimeout bounds one schema operation (the operator's
// HostSyncTables query timeout is 120s; creation is bounded the same way).
const defaultSchemaTimeout = 120 * time.Second

// NewClient builds a SchemaRunner bound to a cluster's inventory.
//
// Why the normalized CR: the operator's schemer names replicas by walking
// the CR's cluster/shard tree (GetShard dereferences the CR — hand-built
// Host values panic in its namer). The CR's normalized hosts carry the
// full runtime addressing; cloud addresses from the inventory override
// their FQDNs so connections go where the operator's pod-FQDNs cannot.
func (f *SchemaClientFactory) NewClient(ctx context.Context, request port.NewSchemaClientRequest) (port.SchemaRunner, error) {
	bootstrapChop()

	cr, ok := request.NormalizedCR.(*v1.ClickHouseInstallation)
	if !ok || cr == nil {
		return nil, fmt.Errorf("normalized CR is required (load the CHI spec first)")
	}
	if request.ClusterName == "" {
		return nil, fmt.Errorf("cluster name is empty")
	}
	if len(request.Hosts) == 0 {
		return nil, fmt.Errorf("host inventory is empty")
	}

	// Address overrides: cloud name=address pairs over the CR's FQDNs.
	addresses := make(map[string]string, len(request.Hosts))
	for _, h := range request.Hosts {
		if h.Name == "" || h.Address == "" {
			return nil, fmt.Errorf("host inventory entry requires Name and Address, got %+v", h)
		}
		addresses[h.Name] = h.Address
	}

	scheme := v1.ChSchemeHTTP
	if request.Secure {
		scheme = v1.ChSchemeHTTPS
	}
	httpPort := request.HTTPPort
	if httpPort == 0 {
		httpPort = 8123
	}

	params := clickhouse.NewClusterConnectionParams(
		scheme,
		request.Credentials.Username,
		request.Credentials.Password,
		"",
		httpPort,
	)

	// Cloud-address namer: every schemer ExecHost path resolves the exec
	// host through the namer's pod-FQDN pattern (chi-...svc.cluster.local,
	// unresolvable off-cluster); the wrapped namer translates FQDN output
	// to the cloud addresses from the inventory. Replica names and every
	// other form pass through operator-native (must match table paths).
	// BOTH embedded levels need the override: ExecHost resolves via the
	// embedded schemer.Cluster (whose own INameManager field is what
	// Names promotes), so setting only the schemer's embedded field
	// leaves the Cluster's default namer active — "no such host"
	//
	s := schemer.NewClusterSchemer(params, swversion.NewSoftWareVersion("24.3"))
	namer := newCloudNamer(addresses)
	s.INameManager = namer
	s.Cluster.INameManager = namer

	// Cluster-name gate: the GC detection SQL interpolates the cluster
	// name into clusterAllReplicas. ClickHouse has no parameters on
	// table-function names, so the defense is whitelist validation: only
	// a name the loaded CR itself declares may ever reach the SQL.
	// NOTE: cr.FindCluster can't be used here — it returns a typed-nil
	// *Cluster inside the ICluster interface, so "== nil" never fires.
	declared := false
	for _, cluster := range cr.GetSpecT().GetConfiguration().GetClusters() {
		if cluster != nil && cluster.Name == request.ClusterName {
			declared = true
			break
		}
	}
	if !declared {
		return nil, fmt.Errorf("cluster %q not declared by the CR's spec", request.ClusterName)
	}

	runner := &SchemaRunner{
		schemer:     s,
		params:      params,
		hosts:       make(map[string]*v1.Host),
		clusterName: request.ClusterName,
	}
	// Collect the CR's normalized hosts for the requested cluster and
	// re-point their FQDNs at the cloud addresses. Unmatched inventory
	// names are an error: a host the CR does not know cannot have
	// operator-consistent replica naming.
	errs := cr.WalkHosts(func(host *v1.Host) error {
		if host.Runtime.Address.ClusterName != request.ClusterName {
			return nil
		}
		address, found := addresses[host.Runtime.Address.HostName]
		if !found {
			return nil // inventory may be a subset (single-node ops)
		}
		host.Runtime.Address.FQDN = address
		runner.hosts[host.Runtime.Address.HostName] = host
		return nil
	})
	if err := firstErr(errs); err != nil {
		return nil, fmt.Errorf("walk hosts: %w", err)
	}
	if len(runner.hosts) == 0 {
		return nil, fmt.Errorf("no cluster %q hosts matched the inventory (CR has the cluster? names match?)",
			request.ClusterName)
	}
	return runner, nil
}

// SyncSchema creates all replicated and distributed objects on a host from
// cluster peers (HostCreateTables).
//
// Without it: a scaled-in reader serves queries with missing tables.
func (r *SchemaRunner) SyncSchema(ctx context.Context, request domain.SchemaSyncRequest) error {
	host, err := r.host(request.HostName)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	return r.schemer.HostCreateTables(ctx, host)
}

// DropReplica removes a dead replica's Keeper registration
// (HostDropReplica — SYSTEM DROP REPLICA).
//
// Why not schemer.HostDropReplica directly: it runs the SQL via ExecHost,
// which resolves the run-on host through the operator's pod-FQDN pattern
// (chi-...svc.cluster.local) — unresolvable off-cluster.
//
// Replica naming: SYSTEM DROP REPLICA takes the replica_path name that
// tables were CREATED with. On Kubernetes that is the operator's
// instance-hostname (chi-<chi>-<cluster>-<s>-<r>); on VM/EC2 stacks it is
// whatever the deployment's macros.xml declares — the host's cloud name
// (writer-0, reader-0) in the AWS stack. The inventory name IS the cloud
// name here, so it is used directly; no guessing between conventions.
// The DATABASE-replica shard index follows the operator's HostDropReplica
// (the run-on host's shard index).
func (r *SchemaRunner) DropReplica(ctx context.Context, request domain.ReplicaDropRequest) error {
	// hostToDrop validates inventory membership; the drop name is the
	// request's HostName (the cloud replica name — see the naming note
	// above).
	if _, err := r.host(request.HostName); err != nil {
		return err
	}
	hostToRunOn, err := r.host(request.RunOn)
	if err != nil {
		return err
	}

	replicaName := request.HostName
	shardIndex := hostToRunOn.Runtime.Address.ShardIndex

	cluster := clickhouse.NewCluster()
	cluster.ClusterConnectionParams = r.params
	cluster.SetHosts([]string{hostToRunOn.Runtime.Address.FQDN})
	opts := clickhouse.NewQueryOptions().SetRetry(false).SetLogQueries(true)
	if err := cluster.ExecAll(ctx, []string{fmt.Sprintf("SYSTEM DROP REPLICA '%s'", replicaName)}, opts); err != nil {
		return err
	}
	// Best-effort database replica drop: only applies when DatabaseReplicated
	// is used; Atomic databases ignore or reject it after the table replica
	// drop has already succeeded.
	_ = cluster.ExecAll(ctx, []string{fmt.Sprintf("SYSTEM DROP DATABASE REPLICA '%d|%s'", shardIndex, replicaName)}, opts)
	return nil
}

// SyncReplica re-queues replication on a restarted node
// (HostSyncTables — SYSTEM SYNC REPLICA).
//
// Without it: a restarted node can lag behind with empty replication queues.
func (r *SchemaRunner) SyncReplica(ctx context.Context, request domain.ReplicaSyncRequest) error {
	host, err := r.host(request.HostName)
	if err != nil {
		return err
	}
	return r.schemer.HostSyncTables(ctx, host)
}

// FindStuckQueueTables returns every table whose replication queue holds
// an entry past the retry threshold.
func (r *SchemaRunner) FindStuckQueueTables(ctx context.Context, request domain.QueueHealRequest) ([]string, error) {
	if err := r.healTarget(request); err != nil {
		return nil, err
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return nil, err
	}
	tries := request.StuckTries
	if tries <= 0 {
		tries = defaultStuckQueueTries
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, fmt.Sprintf(`
		SELECT DISTINCT database, table
		FROM system.replication_queue
		WHERE num_tries > %d
	`, tries))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	defer result.Close()

	var dbs, tbls []string
	if err := result.UnzipColumnsAsStrings(&dbs, &tbls); err != nil {
		return nil, err
	}
	tables := make([]string, 0, len(dbs))
	for i := range dbs {
		tables = append(tables, dbs[i]+"."+tbls[i])
	}
	return tables, nil
}

// CountRunningMerges returns the node's in-flight merge count used by
// the queue-heal busy-skip gate.
func (r *SchemaRunner) CountRunningMerges(ctx context.Context, request domain.QueueHealRequest) (int, error) {
	if err := r.healTarget(request); err != nil {
		return 0, err
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, "SELECT count() FROM system.merges")
	if err != nil {
		return 0, err
	}
	if result == nil {
		return 0, nil
	}
	defer result.Close()

	var counts []string
	if err := result.UnzipColumnsAsStrings(&counts); err != nil {
		return 0, err
	}
	if len(counts) == 0 {
		return 0, nil
	}
	n, err := strconv.Atoi(counts[0])
	if err != nil {
		return 0, fmt.Errorf("merge count %q: %w", counts[0], err)
	}
	return n, nil
}

// RestartReplica re-initializes a table's replication queue from Keeper
// (SYSTEM RESTART REPLICA) — the stuck-queue heal. The restart drops the
// in-memory queue state and re-reads the log from Keeper; parts already
// written are untouched (the queue's block is the retry loop, not data).
//
// Without it: a NO_REPLICA_HAS_PART entry blocks every merge behind it
// forever — parts climb to the insert cliff while merges sit idle, and
// the parts alarm pages a scale-out that cannot help (merge debt is not
// solved by more replicas).
func (r *SchemaRunner) RestartReplica(ctx context.Context, request domain.QueueHealRequest) error {
	if err := r.healTarget(request); err != nil {
		return err
	}
	if request.Table == "" {
		return fmt.Errorf("queue-heal restart: -table is empty")
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return err
	}
	opts := clickhouse.NewQueryOptions().SetRetry(false).SetLogQueries(true)
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	return cluster.ExecAll(ctx, []string{
		fmt.Sprintf("SYSTEM RESTART REPLICA %s", request.Table),
	}, opts)
}

// FindOrphanMergeTables returns every table with orphan-merge queue
// entries: a queued MERGE_PARTS whose source part no live replica
// holds (NO_REPLICA_HAS_PART, StorageReplicatedMergeTree.cpp:2865).
// The graceful path (fetch-merged-part-instead + enqueuePartForCheck,
// :2913) only runs when a sibling merge covers the missing part; the
// throw-path entries exponential-postpone (Queue.cpp:1667, capped at
// max_postpone_time_for_failed_replicated_merges_ms) and retry
// forever, each pass blocking merge assignment evaluation on the
// table. The heal is SYNC REPLICA — re-enqueue the part checks whose
// PartCheckThread resolves or deletes the lost-part entries
// (ReplicatedMergeTreePartCheckThread.cpp:180-199: "consider the part
// lost and delete the entry from the queue").
func (r *SchemaRunner) FindOrphanMergeTables(ctx context.Context, request domain.QueueHealRequest) ([]string, error) {
	if err := r.healTarget(request); err != nil {
		return nil, err
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, `
		SELECT DISTINCT database, table
		FROM system.replication_queue
		WHERE last_exception LIKE '%No active replica has part%'
	`)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	defer result.Close()

	var dbs, tbls []string
	if err := result.UnzipColumnsAsStrings(&dbs, &tbls); err != nil {
		return nil, err
	}
	tables := make([]string, 0, len(dbs))
	for i := range dbs {
		tables = append(tables, dbs[i]+"."+tbls[i])
	}
	return tables, nil
}

// defaultStuckQueueTries is the num_tries threshold a replication
// queue entry must exceed to count as stuck (the live incident's
// entries were past one million; a healthy op's exception-retry loop
// cycles orders of magnitude below this).
const defaultStuckQueueTries = 1000

// FindReadonlyTables returns every table on the node whose replica is
// in read-only mode (Keeper lost quorum or the node lost its Keeper
// session — writes to those tables fail; the fleet-wide read-only
// failure class). The restore itself runs via RestartReplica-shaped
// SYSTEM RESTORE REPLICA.
func (r *SchemaRunner) FindReadonlyTables(ctx context.Context, request domain.QueueHealRequest) ([]string, error) {
	if err := r.healTarget(request); err != nil {
		return nil, err
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, `
		SELECT database, table
		FROM system.replicas
		WHERE is_readonly = 1
			AND last_queue_update != toDateTime(0)
	`)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	defer result.Close()

	var dbs, tbls []string
	if err := result.UnzipColumnsAsStrings(&dbs, &tbls); err != nil {
		return nil, err
	}
	tables := make([]string, 0, len(dbs))
	for i := range dbs {
		tables = append(tables, dbs[i]+"."+tbls[i])
	}
	return tables, nil
}

// FindInitFailedReadonlyTables returns read-only tables whose
// replication queue never initialized (last_queue_update at the epoch),
// which are recovered by setting the flags/force_restore_data znode and
// restarting the replica.
func (r *SchemaRunner) FindInitFailedReadonlyTables(ctx context.Context, request domain.QueueHealRequest) ([]string, error) {
	if err := r.healTarget(request); err != nil {
		return nil, err
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, `
		SELECT database, table
		FROM system.replicas
		WHERE is_readonly = 1
			AND last_queue_update = toDateTime(0)
	`)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	defer result.Close()

	var dbs, tbls []string
	if err := result.UnzipColumnsAsStrings(&dbs, &tbls); err != nil {
		return nil, err
	}
	tables := make([]string, 0, len(dbs))
	for i := range dbs {
		tables = append(tables, dbs[i]+"."+tbls[i])
	}
	return tables, nil
}

// RestoreReplica re-creates a read-only table's replica metadata in
// Keeper (SYSTEM RESTORE REPLICA — verified in the 26.8 source:
// restoreMetadataInZooKeeper). The readonly-heal: a table flipped
// read-only by a lost Keeper session stays read-only until the
// metadata znode is re-created; the operator's answer is pod
// recreation, the EC2 analog is this statement (the node keeps its
// data — the parts catalog re-attaches to the restored metadata).
func (r *SchemaRunner) RestoreReplica(ctx context.Context, request domain.QueueHealRequest) error {
	if err := r.healTarget(request); err != nil {
		return err
	}
	if request.Table == "" {
		return fmt.Errorf("queue-heal restore: -table is empty")
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return err
	}
	opts := clickhouse.NewQueryOptions().SetRetry(false).SetLogQueries(true)
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	return cluster.ExecAll(ctx, []string{
		fmt.Sprintf("SYSTEM RESTORE REPLICA %s", request.Table),
	}, opts)
}

// ReplicaKeeperPathExists reports whether the table's replica is
// registered under its replicas/ path in Keeper. When present, the
// replica is recovered with SYSTEM RESTART REPLICA; when absent,
// SYSTEM RESTORE REPLICA re-creates the metadata.
func (r *SchemaRunner) ReplicaKeeperPathExists(ctx context.Context, request domain.QueueHealRequest) (bool, error) {
	if err := r.healTarget(request); err != nil {
		return false, err
	}
	if request.Table == "" {
		return false, fmt.Errorf("queue-heal keeper-path probe: -table is empty")
	}
	cluster, err := r.healCluster(request)
	if err != nil {
		return false, err
	}
	tableName := request.Table
	if i := strings.LastIndex(tableName, "."); i >= 0 {
		tableName = tableName[i+1:]
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, fmt.Sprintf(`
		SELECT count()
		FROM system.zookeeper
		WHERE path = %s
			AND name = %s
	`, keeperPathNameLiteral("/clickhouse/tables/01/"+tableName+"/replicas"), keeperPathNameLiteral(request.RunOn)))
	if err != nil {
		return false, err
	}
	if result == nil {
		return false, nil
	}
	defer result.Close()
	n, err := result.Int()
	if err != nil {
		return false, err
	}
	for result.Rows != nil && result.Rows.Next() {
	}
	return n > 0, nil
}

// keeperPathNameLiteral renders a bare name as a quoted SQL literal
// ('table' — no injection surface; the name arrives from
// system.replicas rows, never user input).
func keeperPathNameLiteral(name string) string {
	return "'" + strings.ReplaceAll(name, "'", "''") + "'"
}

// healTarget resolves the queue-heal run-on host (self-healing runs on
// the node's own queue; the inventory membership check keeps the
// cloud-name form honest).
func (r *SchemaRunner) healTarget(request domain.QueueHealRequest) error {
	if request.RunOn == "" {
		return fmt.Errorf("queue-heal: -on is empty")
	}
	_, err := r.host(request.RunOn)
	return err
}

// healCluster builds the single-host connection the queue-heal queries
// ride (the same pattern as FindStaleReplicas: the operation runs
// against the named host, inventory-resolved).
func (r *SchemaRunner) healCluster(request domain.QueueHealRequest) (*clickhouse.Cluster, error) {
	host, err := r.host(request.RunOn)
	if err != nil {
		return nil, err
	}
	cluster := clickhouse.NewCluster()
	cluster.ClusterConnectionParams = r.params
	cluster.SetHosts([]string{host.Runtime.Address.FQDN})
	return cluster, nil
}

// sqlFindStaleReplicas builds the dead-replica detection query: host
// names registered in this peer's system.replicas but absent from live
// cluster membership. skip_unavailable_shards = 1 tolerates down peers
// without it a single unreachable node fails the whole sweep; with it a
// fresh cluster returns zero rows and the GC is a no-op (exit 0, zero
// drops).
//
// No FORMAT clause: the operator's connection pins the response format
// via the driver's default_format URL parameter, and an inline FORMAT
// would strip the column header the driver's rows scanner expects. The
// TSV shape of the CLI output comes from Go printing, not the SQL. The
// LIKE gate from the original bash also moved into Go (the service
// applies the pattern); the SQL stays a plain NOT IN over the fan-out.
// The only interpolated value is the cluster name, whitelisted against
// the CR in NewClient before it ever reaches this string.
//
// Column name: ClickHouse 26.8 REMOVED system.replicas' host_name
// column (renamed replica_name — the 2026-11-17 live finding: every
// hourly GC pass failed UNKNOWN_IDENTIFIER and dead replicas
// accumulated unchecked, their queue/parts znodes driving keeper to
// 760k nodes while live replicas retried fetches against the dead
// IPs). The query takes the column as a parameter;
// findStaleReplicas probes once which name this server carries
// (older LTS branches keep host_name) and caches the answer.
func (r *SchemaRunner) sqlFindStaleReplicas(column string) string {
	if column == "replica_name" {
		// ClickHouse 26.8+: system.replicas has one row per local table where
		// replica_name is THIS node's own replica identity; all registered
		// replicas in Keeper (including dead/inactive peers) live in the
		// replica_is_active Map(String, UInt8) column (StorageSystemReplicas.cpp).
		// Expanding mapKeys(replica_is_active) surfaces every peer registered in
		// Keeper whose ephemeral is_active znode is 0 and that is absent from live
		// clusterAllReplicas membership.
		return fmt.Sprintf(`
		SELECT DISTINCT arrayJoin(mapKeys(replica_is_active)) AS replica_name
		FROM system.replicas
		WHERE replica_is_active[replica_name] = 0
			AND replica_name NOT IN (
				SELECT replica_name
				FROM clusterAllReplicas('%s', system.replicas)
				SETTINGS skip_unavailable_shards = 1
			)
	`, r.clusterName)
	}
	return fmt.Sprintf(`
		SELECT DISTINCT %[1]s
		FROM system.replicas
		WHERE %[1]s NOT IN (
			SELECT %[1]s
			FROM clusterAllReplicas('%s', system.replicas)
			SETTINGS skip_unavailable_shards = 1
		)
	`, column, r.clusterName)
}

// FindStaleReplicas returns host names registered in system.replicas on
// the run-on peer but absent from live cluster membership (the
// dead-replica GC detection step).
//
// Without it: stale znodes accumulate after scale-in or spot reclaim and
// DDL/re-join reliability decays; detection lived in bash that had to be
// scheduled and parsed separately from the drop it feeds.
func (r *SchemaRunner) FindStaleReplicas(ctx context.Context, request domain.ReplicaGcRequest) ([]string, error) {
	hostToRunOn, err := r.host(request.RunOn)
	if err != nil {
		return nil, err
	}

	column, err := r.staleReplicaColumn(ctx, hostToRunOn)
	if err != nil {
		return nil, err
	}

	cluster := clickhouse.NewCluster()
	cluster.ClusterConnectionParams = r.params
	cluster.SetHosts([]string{hostToRunOn.Runtime.Address.FQDN})
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, r.sqlFindStaleReplicas(column))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	defer result.Close()

	var hosts []string
	if err := result.UnzipColumnsAsStrings(&hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// staleReplicaColumn resolves which system.replicas column this server
// carries: 26.8+ names it replica_name; older branches kept host_name.
// The probe is one trivial SELECT over system.columns — cheap enough
// to run per detection pass, and never cached across runners (a server
// version can change under a long-lived process only via restart,
// which rebuilds the runner).
func (r *SchemaRunner) staleReplicaColumn(ctx context.Context, host *v1.Host) (string, error) {
	cluster := clickhouse.NewCluster()
	cluster.ClusterConnectionParams = r.params
	cluster.SetHosts([]string{host.Runtime.Address.FQDN})
	ctx, cancel := context.WithTimeout(ctx, defaultSchemaTimeout)
	defer cancel()
	result, err := cluster.QueryAny(ctx, `
		SELECT name FROM system.columns
		WHERE database = 'system' AND table = 'replicas'
			AND name IN ('replica_name', 'host_name')
		ORDER BY name = 'replica_name' DESC
		LIMIT 1
	`)
	if err != nil {
		// The probe itself failed (transport): report — a guessed
		// column name turns the whole sweep into an UNKNOWN_IDENTIFIER
		// failure, which is exactly the blind this fixes.
		return "", fmt.Errorf("stale-replica column probe: %w", err)
	}
	if result == nil {
		return "", fmt.Errorf("stale-replica column probe: no result")
	}
	defer result.Close()
	var names []string
	if err := result.UnzipColumnsAsStrings(&names); err != nil {
		return "", fmt.Errorf("stale-replica column probe: %w", err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("system.replicas carries neither replica_name nor host_name (unsupported server)")
	}
	return names[0], nil
}

// host resolves a host by CR name (e.g. "0-0") or by the cloud name the
// inventory carries ("writer-0") — the runner keys by CR name but the
// inventory names map onto them, so both forms address the same Host.
func (r *SchemaRunner) host(name string) (*v1.Host, error) {
	if host, ok := r.hosts[name]; ok {
		return host, nil
	}
	for _, host := range r.hosts {
		if host.Runtime.Address.FQDN == name || strings.Split(host.Runtime.Address.FQDN, ".")[0] == name {
			return host, nil
		}
	}
	// The miss must name what IS valid: an operator reading this from a
	// node bootstrap log has no other way to learn the inventory's
	// names (— "host \"ip-10-0-1-53\" not in inventory"
	// gave no clue what the valid names were). CR names and cloud short
	// names both address the same host, so both are listed.
	return nil, fmt.Errorf("host %q not in inventory (available: %s)", name, strings.Join(r.inventoryNames(), ", "))
}

// inventoryNames lists every name that host would accept, sorted
// (deterministic for logs and tests) — the CR name plus the cloud short
// name (first FQDN label) of every host in the inventory.
func (r *SchemaRunner) inventoryNames() []string {
	names := make([]string, 0, 2*len(r.hosts))
	for name, host := range r.hosts {
		names = append(names, name)
		if short := strings.Split(host.Runtime.Address.FQDN, ".")[0]; short != "" && short != name {
			names = append(names, short)
		}
	}
	sort.Strings(names)
	return names
}
