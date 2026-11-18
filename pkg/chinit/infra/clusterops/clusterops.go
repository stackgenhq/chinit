// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clusterops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/keeper"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/operator"
)

// BootstrapDir is where the boot's bundle lands the specs — the same
// path chinit's service.BootstrapDir names. Repeated locally so this
// package has no chinit-service dependency (callers pass spec paths
// explicitly; the constant documents the default).
const BootstrapDir = "/var/lib/clickhouse-bootstrap"

// Ops is the cluster-operations port. Production binds the
// in-process services; tests bind fakes through the same interface.
type Ops interface {
	// SyncSchema creates every spec object on a host from cluster
	// peers (HostCreateTables + retry passes
	// + the optional stale-self prune).
	SyncSchema(ctx context.Context, req SyncSchemaRequest) error
	// SyncReplica re-queues replication on a restarted node.
	SyncReplica(ctx context.Context, req SyncReplicaRequest) error
	// QueueHeal detects and unsticks a node's blocked replication
	// queue (merge-idle plus stuck entries → SYSTEM RESTART REPLICA).
	QueueHeal(ctx context.Context, req QueueHealRequest) (*QueueHealResult, error)
	// DropReplica removes a dead replica's registration via a live
	// peer (HostDropReplica).
	DropReplica(ctx context.Context, req DropReplicaRequest) error
	// GcReplica detects stale registrations on a peer and drops them
	// in one pass.
	GcReplica(ctx context.Context, req GcReplicaRequest) (*GcResult, error)
	// KeeperMntr fetches one keeper's mntr payload.
	KeeperMntr(ctx context.Context, req MntrRequest) (string, error)
	// KeeperMntrTyped returns the parsed mntr result (the quorum
	// fields + the verbatim payload) — the rollout coordinator's
	// ensemble gate reads the typed shape, the journal keeps the
	// string variant.
	KeeperMntrTyped(ctx context.Context, req MntrRequest) (service.MntrResult, error)
	// Health fans out the post-boot health checks.
	Health(ctx context.Context, req HealthRequest) (*HealthReport, error)
}

// ClusterRequest is the shared per-operation binding: which cluster,
// which spec, which hosts, which credentials.
type ClusterRequest struct {
	// ClusterName is the ClickHouse cluster name (remote_servers).
	ClusterName string
	// SpecPath is the CHI spec file (chi.yaml) the loader reads.
	SpecPath string
	// Hosts is the inventory (Name=Address pairs, the CR slot map).
	Hosts []Host
	// Credentials connect to the target hosts.
	Credentials domain.Credentials
	// HTTPPort is the HTTP interface port (0 = 8123).
	HTTPPort int
	// Secure marks TLS connections.
	Secure bool
	// NormalizedCR, when set, skips the spec load (the boot's LoadCHI
	// seam already produced it).
	NormalizedCR any
}

// Host is one inventory entry (the CR-name → address pair the CLI's
// -host flags carried).
type Host struct {
	Name    string
	Address string
}

// SyncSchemaRequest is the schema sync request: create every spec object
// on HostName, with full-pass retries and the optional stale-self
// prune.
type SyncSchemaRequest struct {
	Cluster ClusterRequest
	// HostName is the node whose schema is created (cloud short name).
	HostName string
	// Retries bounds the full create passes (0 = service default).
	Retries int
	// PruneStaleSelf reclaims the target's own stale registration.
	PruneStaleSelf bool
}

// SyncReplicaRequest is the replica sync request: SYSTEM SYNC REPLICA on
// every replicated table of HostName.
type SyncReplicaRequest struct {
	Cluster ClusterRequest
	// HostName is the node that re-queues replication.
	HostName string
}

// QueueHealRequest is the queue-heal request: detect and unstick the
// blocked replication queue of RunOn (self-healing — each node's timer
// heals its own queue).
type QueueHealRequest struct {
	Cluster ClusterRequest
	// RunOn is the node whose queue is inspected and healed.
	RunOn string
	// StuckTries is the num_tries threshold (0 = the service default).
	StuckTries int
	// BusyMergesSkip leaves a node with running merges alone.
	BusyMergesSkip bool
	// ForceRestore is the optional keeper znode capability for the
	// init-FAILED readonly heal (nil = the pass reports but cannot
	// heal that class).
	ForceRestore port.ForceRestoreFlagger
}

// QueueHealResult is the projected outcome: the stuck tables found,
// the tables restarted, and the skip evidence.
type QueueHealResult struct {
	StuckTables      []string
	Restarted        []string
	MergesRunning    int
	SkippedReason    string
	ReadonlyTables   []string
	InitFailedTables []string
	Restored         []string
	OrphanTables     []string
	OrphanSynced     []string
	// FailedTables lists tables whose restart/sync failed this pass
	// (contained per-table; the next pass retries).
	FailedTables []string
}

// DropReplicaRequest asks a peer to drop a replica: remove HostName's
// registration, executed against RunOn.
type DropReplicaRequest struct {
	Cluster ClusterRequest
	// HostName is the replica being removed.
	HostName string
	// RunOn is the peer that executes SYSTEM DROP REPLICA.
	RunOn string
}

// GcReplicaRequest detects stale registrations
// on RunOn and drop the ones matching the pattern, excluding
// ExcludeHosts.
type GcReplicaRequest struct {
	Cluster ClusterRequest
	// RunOn is the peer that executes detection + drop.
	RunOn string
	// NamePattern gates drop eligibility ("" = reader-*).
	NamePattern string
	// ExcludeHosts never drop, even when stale.
	ExcludeHosts []string
}

// GcResult is the sweep outcome projected for the caller.
type GcResult struct {
	Stale   []string
	Dropped []string
	Failed  []string
}

// MntrRequest is the keeper mntr probe.
type MntrRequest struct {
	// Address is host:port of the keeper's 4lw endpoint.
	Address string
	// Timeout bounds the probe (0 = 5s).
	Timeout time.Duration
}

// HealthRequest is the post-boot health fan-out.
type HealthRequest struct {
	Cluster ClusterRequest
	// KeeperAddress, when non-empty, runs the keeper check.
	KeeperAddress string
	// CheckTables enables the tables-present check (spec loaded).
	CheckTables bool
	// SelfName runs the self-registration check when non-empty.
	SelfName string
	// Timeout bounds one probe (0 = 5s).
	Timeout time.Duration
}

// HealthReport is the projected outcome: the healthy verdict and the
// human format the boot log prints.
type HealthReport struct {
	Healthy bool
	Human   string
}

// ops binds the in-process services.
type ops struct {
	schema *service.SchemaService
	mntr   *service.KeeperMntrService
	loader *operator.SpecLoader
}

// New builds the production Ops over the in-process library services.
func New() Ops {
	return &ops{
		schema: service.NewSchemaService(operator.NewSchemaClientFactory()),
		mntr:   service.NewKeeperMntrService(keeper.NewProbe()),
		loader: operator.NewSpecLoader(),
	}
}

// loadCR reads + normalizes the CHI spec (or takes the caller's
// already-loaded CR).
func (o *ops) loadCR(ctx context.Context, c ClusterRequest) (any, error) {
	if c.NormalizedCR != nil {
		return c.NormalizedCR, nil
	}
	if c.SpecPath == "" {
		return nil, fmt.Errorf("cluster request needs a spec path or a normalized CR")
	}
	data, err := os.ReadFile(c.SpecPath)
	if err != nil {
		return nil, fmt.Errorf("read spec %s: %w", c.SpecPath, err)
	}
	cr, err := o.loader.LoadCHI(ctx, port.LoadCHIRequest{
		Spec: domain.SpecReference{Name: c.ClusterName, Data: data},
	})
	if err != nil {
		return nil, fmt.Errorf("load spec %s: %w", c.SpecPath, err)
	}
	return cr, nil
}

// hosts projects the inventory onto the domain HostIdentity slice.
func (r ClusterRequest) hosts() []domain.HostIdentity {
	out := make([]domain.HostIdentity, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		out = append(out, domain.HostIdentity{Name: h.Name, Address: h.Address})
	}
	return out
}

// schemaRequest builds the service binding (CR load included).
func (o *ops) schemaRequest(ctx context.Context, c ClusterRequest) (service.SchemaRequest, error) {
	cr, err := o.loadCR(ctx, c)
	if err != nil {
		return service.SchemaRequest{}, err
	}
	return service.SchemaRequest{
		ClusterName:  c.ClusterName,
		Credentials:  c.Credentials,
		Hosts:        c.hosts(),
		HTTPPort:     c.HTTPPort,
		Secure:       c.Secure,
		NormalizedCR: cr,
	}, nil
}

// SyncSchema implements Ops.
func (o *ops) SyncSchema(ctx context.Context, req SyncSchemaRequest) error {
	base, err := o.schemaRequest(ctx, req.Cluster)
	if err != nil {
		return err
	}
	return o.schema.SyncSchema(ctx, base, domain.SchemaSyncRequest{
		HostName:       req.HostName,
		Retries:        req.Retries,
		PruneStaleSelf: req.PruneStaleSelf,
	})
}

// SyncReplica implements Ops.
func (o *ops) SyncReplica(ctx context.Context, req SyncReplicaRequest) error {
	base, err := o.schemaRequest(ctx, req.Cluster)
	if err != nil {
		return err
	}
	return o.schema.SyncReplica(ctx, base, domain.ReplicaSyncRequest{HostName: req.HostName})
}

// QueueHeal implements Ops.
func (o *ops) QueueHeal(ctx context.Context, req QueueHealRequest) (*QueueHealResult, error) {
	base, err := o.schemaRequest(ctx, req.Cluster)
	if err != nil {
		return nil, err
	}
	svc := o.schema.WithForceRestoreFlagger(req.ForceRestore)
	heal, err := svc.QueueHeal(ctx, base, domain.QueueHealRequest{
		RunOn:          req.RunOn,
		StuckTries:     req.StuckTries,
		BusyMergesSkip: req.BusyMergesSkip,
	})
	if err != nil {
		return nil, err
	}
	return &QueueHealResult{
		StuckTables:      heal.StuckTables,
		Restarted:        heal.Restarted,
		MergesRunning:    heal.MergesRunning,
		SkippedReason:    heal.SkippedReason,
		ReadonlyTables:   heal.ReadonlyTables,
		InitFailedTables: heal.InitFailedTables,
		Restored:         heal.Restored,
		OrphanTables:     heal.OrphanTables,
		OrphanSynced:     heal.OrphanSynced,
		FailedTables:     heal.FailedTables,
	}, nil
}

// DropReplica implements Ops.
func (o *ops) DropReplica(ctx context.Context, req DropReplicaRequest) error {
	base, err := o.schemaRequest(ctx, req.Cluster)
	if err != nil {
		return err
	}
	return o.schema.DropReplica(ctx, base, domain.ReplicaDropRequest{
		HostName: req.HostName,
		RunOn:    req.RunOn,
	})
}

// GcReplica implements Ops.
func (o *ops) GcReplica(ctx context.Context, req GcReplicaRequest) (*GcResult, error) {
	base, err := o.schemaRequest(ctx, req.Cluster)
	if err != nil {
		return nil, err
	}
	res, err := o.schema.GcReplica(ctx, base, domain.ReplicaGcRequest{
		RunOn:        req.RunOn,
		NamePattern:  req.NamePattern,
		ExcludeHosts: req.ExcludeHosts,
	})
	if err != nil {
		return nil, err
	}
	return &GcResult{Stale: res.Stale, Dropped: res.Dropped, Failed: res.Failed}, nil
}

// KeeperMntr implements Ops: the mntr payload as sorted key=value
// lines (the CLI's default output shape — the health cron greps it).
func (o *ops) KeeperMntr(ctx context.Context, req MntrRequest) (string, error) {
	res, err := o.mntr.Mntr(ctx, service.MntrRequest{
		KeeperAddress: req.Address,
	})
	if err != nil {
		return "", err
	}
	return res.FormatLines(), nil
}

// KeeperMntrTyped implements Ops: the parsed result.
func (o *ops) KeeperMntrTyped(ctx context.Context, req MntrRequest) (service.MntrResult, error) {
	return o.mntr.Mntr(ctx, service.MntrRequest{
		KeeperAddress: req.Address,
	})
}

// Health implements Ops.
func (o *ops) Health(ctx context.Context, req HealthRequest) (*HealthReport, error) {
	cr, err := o.loadCR(ctx, req.Cluster)
	if err != nil {
		return nil, err
	}
	var expectedTables func() []domain.TableRef
	if req.CheckTables {
		// The tables-present check needs the spec's declared tables;
		// the loaded CR carries them.
		tables := operator.ExpectedTablesFromCR(cr)
		expectedTables = func() []domain.TableRef { return tables }
	}
	svc := service.NewHealthService(operator.NewHealthProbe(), keeper.NewProbe(), expectedTables)
	report, err := svc.Check(ctx, service.HealthRequest{
		ClusterName:   req.Cluster.ClusterName,
		Hosts:         req.Cluster.hosts(),
		Credentials:   req.Cluster.Credentials,
		HTTPPort:      req.Cluster.HTTPPort,
		Secure:        req.Cluster.Secure,
		Timeout:       req.Timeout,
		KeeperAddress: req.KeeperAddress,
		CheckTables:   req.CheckTables,
		SelfName:      req.SelfName,
	})
	if err != nil {
		return nil, err
	}
	return &HealthReport{Healthy: report.Healthy(), Human: report.FormatHuman()}, nil
}

// DefaultSpecPath is the bundle's chi.yaml location (the caller's
// shorthand).
func DefaultSpecPath() string { return filepath.Join(BootstrapDir, "chi.yaml") }
