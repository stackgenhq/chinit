// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

// The healing ports' exec-backed adapters: the local clickhouse-client
// (HotPartitionStore, NodeQuiescer) — transport only, no policy. The
// policy lives in the domain services (diskrelief.go, fence.go); these
// adapters exist so the CLI commands can inject the same fake seam the
// runCommandWithEnv var already provides.

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// cliHotPartitionStore implements port.HotPartitionStore over clickhouse-client.
type cliHotPartitionStore struct {
	// queryContext bounds and authenticates every statement.
	queryContext context.Context
}

// hotPartitionQuery finds hot-disk relief candidates, biggest first,
// up to limit rows, excluding the current day's actively-ingesting
// partition.
const hotPartitionQuery = `
	SELECT database, table, partition, sum(bytes_on_disk)
	FROM system.parts
	WHERE active AND disk_name = 'default'
		AND database != 'system'
		AND partition != toString(today())
	GROUP BY database, table, partition
	HAVING sum(bytes_on_disk) > 5368709120
	ORDER BY sum(bytes_on_disk) DESC
	LIMIT %d`

// HotPartitions implements port.HotPartitionStore.
func (s *cliHotPartitionStore) HotPartitions(limit int) ([]port.HotPartition, error) {
	if limit <= 0 {
		return nil, nil
	}
	out, err := runCommandWithEnv(s.queryContext, clickhouseClientEnv(), "clickhouse-client",
		"--query", fmt.Sprintf(hotPartitionQuery, limit))
	if err != nil {
		return nil, fmt.Errorf("hot-partition query: %s", strings.TrimSpace(string(out)))
	}
	var partitions []port.HotPartition
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		bytes, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			continue
		}
		partitions = append(partitions, port.HotPartition{
			Database:    fields[0],
			Table:       fields[1],
			Partition:   fields[2],
			BytesOnDisk: bytes,
		})
	}
	return partitions, nil
}

// MoveToCold implements port.HotPartitionStore. The partition expr is
// a QUOTED string literal (the source's own syntax,
// ParserAlterQuery.cpp:2143: MOVE PARTITION '2019-09-01' TO VOLUME —
// the raw value parses as a Date column reference and errors). The
// statement is not replicated (different replicas may hold different
// policies), so the per-node timer is the correct scope.
func (s *cliHotPartitionStore) MoveToCold(p port.HotPartition) error {
	stmt := fmt.Sprintf("ALTER TABLE %s.%s MOVE PARTITION '%s' TO VOLUME 'cold'",
		p.Database, p.Table, strings.ReplaceAll(p.Partition, "'", "\\'"))
	out, err := runCommandWithEnv(s.queryContext, clickhouseClientEnv(), "clickhouse-client", "--query", stmt)
	if err != nil {
		return fmt.Errorf("move partition: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// cliNodeQuiescer is port.NodeQuiescer over clickhouse-client: the
// stop-serving pair the drain and the fence share.
type cliNodeQuiescer struct {
	// queryContext bounds and authenticates every statement.
	queryContext context.Context
}

// quiesceStatements is the stop-serving pair, in order.
var quiesceStatements = []string{
	"SYSTEM STOP REPLICATION QUEUES",
	"SYSTEM STOP DISTRIBUTED SENDS",
}

// Quiesce implements port.NodeQuiescer: best-effort by contract — a
// failed statement is returned to the caller (the drain logs and
// continues; the fence logs and continues), never swallowed here.
func (q *cliNodeQuiescer) Quiesce() error {
	var failures []string
	for _, stmt := range quiesceStatements {
		out, err := runCommandWithEnv(q.queryContext, clickhouseClientEnv(), "clickhouse-client", "--query", stmt)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", stmt, strings.TrimSpace(string(out))))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("quiesce: %s", strings.Join(failures, "; "))
	}
	return nil
}

// cliForceRestoreFlagger is port.ForceRestoreFlagger over
// clickhouse-keeper-client — the transport the live heal proved (the
// SQL surface rejects znode writes without allow_zookeeper_write,
// which the stack deliberately does not enable; the keeper-client
// talks to the Raft port directly, the operator-facing path).
type cliForceRestoreFlagger struct {
	// execContext bounds every keeper-client call.
	execContext context.Context
	// keeperHost is the ensemble endpoint the client connects to
	// (keeper-0's DNS name from the rendered zookeeper config).
	keeperHost string
	// keeperPort is the Raft client port (9181 on this stack).
	keeperPort int
	// replicaName is THIS node's {replica} identity (the settled
	// hostname) — the flag lives under the replica's own Keeper path.
	replicaName string
}

// SetForceRestoreFlag implements port.ForceRestoreFlagger: touch the
// replica's flags/force_restore_data znode (create-if-absent; an
// existing flag is the idempotent no-op — the attach thread consumes
// and removes it on the next restart).
//
// The path is REPLICA-SCOPED — the 2026-11-17 live proof: the flag is
// read by the attach thread at replica_path/flags/force_restore_data
// (StorageReplicatedMergeTree.cpp:558 reads getFlagsPath(), the
// replica's own flags/ znode), and the live A/B on backup-0 showed the
// TABLE-level path (/clickhouse/tables/01/<tbl>/flags/...) failing
// with "Coordination error: No node" (no flags/ znode exists there)
// while the table stayed readonly, and the REPLICA-level path
// (.../replicas/<self>/flags/force_restore_data) healing the table on
// the very next SYSTEM RESTART REPLICA (is_readonly 1 -> 0,
// last_queue_update moving off the epoch).
func (f *cliForceRestoreFlagger) SetForceRestoreFlag(table string) error {
	tableName := table
	if i := strings.LastIndex(table, "."); i >= 0 {
		tableName = table[i+1:]
	}
	replica := f.replicaName
	if replica == "" {
		// Never guess a path: an empty replica name would write the
		// flag somewhere no attach thread reads. The caller builds the
		// flagger from the settled identity; reaching here is a wiring
		// bug, and failing loudly beats a silently-ineffective heal.
		return fmt.Errorf("force-restore flag for %s: replica name is empty (state carries no settled identity)", table)
	}
	flag := fmt.Sprintf("/clickhouse/tables/01/%s/replicas/%s/flags/force_restore_data", tableName, replica)
	out, err := runCommand(f.execContext, "clickhouse-keeper-client",
		"--host", f.keeperHost,
		"--port", strconv.Itoa(f.keeperPort),
		"-q", "touch "+flag)
	if err != nil {
		return fmt.Errorf("keeper touch %s: %s", flag, strings.TrimSpace(string(out)))
	}
	return nil
}

// defaultKeeperRaftPort is the ensemble's Raft client port (the
// rendered zookeeper config's <port>; 9181 across this stack's
// golden AMIs).
const defaultKeeperRaftPort = 9181

// newForceRestoreFlagger builds the flagger from the node's spec and
// its SETTLED identity — nil when the spec carries no keeper (a
// keeper-less stack has no znodes to flag; the queue-heal pass reports
// the init-failed tables unhealed). The replica name is the state
// file's hostname (the {replica} identity the tables were created
// with); the flag is replica-scoped, so the flagger without it would
// touch a path no attach thread reads.
func newForceRestoreFlagger(ctx context.Context, st *nodeState) port.ForceRestoreFlagger {
	spec, err := loadInventorySpec()
	if err != nil || spec == nil {
		return nil
	}
	if len(spec.KeeperNodes) == 0 {
		return nil // keeper-less stack: no znodes to flag
	}
	if st.Identity.Hostname == "" {
		// No settled identity: the flag is replica-scoped, and a
		// flagger without the replica name can only guess paths.
		// Report unhealed rather than write blind (the queue-heal
		// pass's own deferred-verdict contract).
		return nil
	}
	host := spec.KeeperNodes[0]
	if host == "" {
		host = "keeper-0"
	}
	if spec.DNSZoneName != "" {
		host = host + "." + spec.DNSZoneName
	}
	return &cliForceRestoreFlagger{execContext: ctx, keeperHost: host, keeperPort: defaultKeeperRaftPort, replicaName: st.Identity.Hostname}
}
