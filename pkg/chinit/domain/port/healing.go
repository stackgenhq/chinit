// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

// The disk-pressure and self-fence ports: the domain services that heal
// a node past its volume ceiling and retire an identity-contested
// duplicate. Each is a narrow seam the app layer binds to exec-backed
// adapters (the local clickhouse-client, the ASG API) — the services own
// the POLICY, the adapters own the transport.

// HotPartition is one disk-pressure relief candidate: a partition whose
// active parts hold bytes on the hot ('default') disk.
type HotPartition struct {
	// Database and Table name the replicated table the partition belongs to.
	Database, Table string
	// Partition is the partition key as rendered by system.parts
	// (the MOVE PARTITION operand verbatim).
	Partition string
	// BytesOnDisk is the partition's hot-disk footprint.
	BytesOnDisk uint64
}

// HotPartitionStore lists disk-pressure relief candidates and moves
// them to the cold volume — the SQL surface of the relief policy. The
// app layer binds this to clickhouse-client; tests bind it to a fake.
type HotPartitionStore interface {
	// HotPartitions returns the largest hot-disk partitions, biggest
	// first, up to limit rows. Only partitions with hot-disk bytes
	// qualify: a partition already TTL-moved to cold carries nothing
	// to relieve.
	HotPartitions(limit int) ([]HotPartition, error)
	// MoveToCold moves one partition to the cold volume (S3) — the
	// same operation the TTL mover performs, triggered by pressure
	// instead of time.
	MoveToCold(p HotPartition) error
}

// ForceRestoreFlagger creates the flags/force_restore_data znode on a
// replica's keeper path — the init-FAILED readonly heal (the attach
// thread refused on TOO_MANY_UNEXPECTED_DATA_PARTS; the flag makes
// the next RESTART REPLICA skip the parts sanity check — the source
// contract, StorageReplicatedMergeTree.cpp:558 — detaching the
// unexpected parts and syncing from the log instead of staying
// readonly). Bound to clickhouse-keeper-client in the app layer (the
// SQL surface rejects znode writes without allow_zookeeper_write).
type ForceRestoreFlagger interface {
	// SetForceRestoreFlag touches the znode for the named table's
	// replica (the run-on host's own replica path).
	SetForceRestoreFlag(table string) error
}

// NodeQuiescer stops a node's replication serving: no fetch, merge or
// distributed send starts after it runs. The self-fence's first act —
// the duplicate must stop taking cluster work before it exits.
type NodeQuiescer interface {
	// Quiesce issues the stop-serving statements (SYSTEM STOP
	// REPLICATION QUEUES + STOP DISTRIBUTED SENDS). Best-effort by
	// contract: the node is exiting regardless, and a failed statement
	// must not block the fence.
	Quiesce() error
}
