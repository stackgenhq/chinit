// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	chops "github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/operator"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// publishNodeStatus writes the node's status row alongside the lease
// renewal tick, including local ClickHouse version/health and Keeper
// quorum reachability.
func publishNodeStatus(ctx context.Context, st *nodeState, provider port.Provider, table string) {
	publisher := port.AsStatusPublisher(provider)
	if publisher == nil {
		logger.GetLogger(ctx).Warn("lease-renew: cloud lacks the status-publish capability", "cloud", provider.Name())
		return
	}
	version, health := probeLocalServer(ctx)
	status := port.NodeStatus{
		Hostname:          st.Identity.Hostname,
		Role:              string(st.Identity.Role),
		InstanceID:        st.Identity.InstanceID,
		SpecHash:          st.SpecHash,
		ClickHouseVersion: version,
		Health:            health,
		KeeperQuorum:      probeKeeperQuorum(ctx, st),
		UpdatedAt:         time.Now().Unix(),
	}
	if err := publisher.PublishStatus(ctx, port.NodeStatusPublish{Table: table, Status: status}); err != nil {
		logger.GetLogger(ctx).Warn("lease-renew: status publish failed", "err", err)
	}
}

// probeKeeperQuorum answers whether the node's keeper ensemble is
// reachable and answering mntr: keepers probe LOCALHOST (the ensemble
// member itself — a keeper's own mntr up IS its vote), server nodes
// probe the ensemble's entrypoint (keeper-0's DNS name, the same
// address the boot health gate uses). A probe failure answers false,
// never an error: the false row is the finding (the fleet-wide
// readonly case every consumer needs to distinguish from node
// failure).
//
// The ops seam: production uses the package's clusterOps port; the
// specs inject a recording fake through probeKeeperQuorumWith.
func probeKeeperQuorum(ctx context.Context, st *nodeState) bool {
	return probeKeeperQuorumWith(ctx, st, clusterOps)
}

// probeKeeperQuorumWith is probeKeeperQuorum over an injected ops
// port (the specs' seam — the same var pattern as clusterOps itself).
func probeKeeperQuorumWith(ctx context.Context, st *nodeState, ops chops.Ops) bool {
	address := "localhost:9181"
	if st.Identity.Role != model.RoleKeeper {
		spec, err := loadInventorySpec()
		if err != nil || spec == nil {
			return false // no spec = no known keeper address
		}
		if len(spec.KeeperNodes) == 0 {
			return false
		}
		address = fmt.Sprintf("%s.%s:9181", spec.KeeperNodes[0], spec.DNSZoneName)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := ops.KeeperMntr(pctx, chops.MntrRequest{
		Address: address,
		Timeout: 5 * time.Second,
	})
	return err == nil
}

// probeLocalServer answers the node's clickhouse version and health
// through the operator-library client (infra/operator's HealthProbe —
// the same ClusterConnectionParams machinery the schemer and health
// paths use; no chinit-side SQL client exists by design). A failed
// probe is ("" , degraded) — never an error, never a guess: the
// degraded row IS the finding.
func probeLocalServer(ctx context.Context) (string, port.NodeHealth) {
	probe := operator.NewHealthProbe()
	creds := domain.Credentials{Username: "admin", Password: adminPassword()}
	version, err := probe.QueryVersion(ctx, port.HealthPingRequest{
		Host: domain.HostIdentity{
			Name:    "localhost",
			Address: "127.0.0.1",
		},
		Credentials: creds,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		return "", port.NodeDegraded
	}
	return version, port.NodeHealthy
}

// runFleetStatus is `chinit fleet-status [-json] [-state FILE]`: the
// fleet's status table (the operator's Status-object equivalent at
// the VM level). The exit code carries the fleet verdict for scripts:
// 0 all healthy, 2 any degraded/cordoned.
func runFleetStatus(args []string) error {
	fs := flag.NewFlagSet("chinit fleet-status", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	jsonOut := fs.Bool("json", false, "print the rows as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadNodeState(*statePath)
	if err != nil {
		return err
	}
	table := fleetTaskTable(st)
	if table == "" {
		return fmt.Errorf("fleet-status: no lease table in identity or spec")
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	publisher := port.AsStatusPublisher(provider)
	if publisher == nil {
		return fmt.Errorf("fleet-status: cloud %s lacks the status capability", provider.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := publisher.ListStatus(ctx, port.NodeStatusList{Table: table})
	if err != nil {
		return fmt.Errorf("fleet-status: %w", err)
	}
	if *jsonOut {
		fmt.Printf("[\n")
		for i, row := range rows {
			sep := ","
			if i == len(rows)-1 {
				sep = ""
			}
			fmt.Printf("  {\"hostname\": %q, \"role\": %q, \"instance_id\": %q, \"spec_hash\": %q, \"ch_version\": %q, \"health\": %q, \"keeper_quorum\": %t, \"cordoned\": %t, \"updated_at\": %d}%s\n",
				row.Hostname, row.Role, row.InstanceID, row.SpecHash, row.ClickHouseVersion, row.Health, row.KeeperQuorum, row.Cordoned, row.UpdatedAt, sep)
		}
		fmt.Printf("]\n")
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("fleet-status: no status rows (no node has published yet)")
		return nil
	}
	fmt.Printf("%-16s %-8s %-8s %-10s %-12s %-8s %-7s %s\n",
		"HOSTNAME", "ROLE", "HEALTH", "CH-VERSION", "SPEC-HASH-8", "CORDONED", "KEEPER", "AGE")
	degraded := 0
	for _, row := range rows {
		age := time.Since(time.Unix(row.UpdatedAt, 0)).Round(time.Second)
		specHash := ""
		if len(row.SpecHash) >= 8 {
			specHash = row.SpecHash[:8]
		}
		fmt.Printf("%-16s %-8s %-8s %-10s %-12s %-8t %-7t %s\n",
			row.Hostname, row.Role, row.Health, row.ClickHouseVersion, specHash, row.Cordoned, row.KeeperQuorum, age)
		if row.Health != port.NodeHealthy || row.Cordoned {
			degraded++
		}
	}
	if degraded > 0 {
		return exitCodeErrorf(2, "fleet-status: %d node(s) degraded or cordoned", degraded)
	}
	fmt.Printf("fleet-status: %d node(s), all healthy\n", len(rows))
	return nil
}

// runCordon is `chinit cordon [-state FILE] <hostname>`: flag a node
// for maintenance (excluded from task dispatch and GC victim lists;
// its lease keeps renewing — identity is preserved).
func runCordon(args []string) error {
	return runCordonToggle(args, true)
}

// runUncordon is `chinit uncordon [-state FILE] <hostname>`.
func runUncordon(args []string) error {
	return runCordonToggle(args, false)
}

// runCordonToggle is the shared cordon path. The toggle targets
// another node's row (the operator acts from any node); the status
// row must exist (the target published at least once).
func runCordonToggle(args []string, cordoned bool) error {
	fs := flag.NewFlagSet("chinit cordon", flag.ContinueOnError)
	statePath := fs.String("state", service.ChinitStatePath, "state file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("cordon requires exactly one hostname argument")
	}
	target := fs.Arg(0)
	st, err := loadNodeState(*statePath)
	if err != nil {
		return err
	}
	table := fleetTaskTable(st)
	if table == "" {
		return fmt.Errorf("cordon: no lease table in identity or spec")
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	publisher := port.AsStatusPublisher(provider)
	if publisher == nil {
		return fmt.Errorf("cordon: cloud %s lacks the status capability", provider.Name())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := publisher.SetCordon(ctx, port.NodeCordonRequest{Table: table, Hostname: target, Cordoned: cordoned}); err != nil {
		return fmt.Errorf("cordon: %w", err)
	}
	verb := "cordoned"
	if !cordoned {
		verb = "uncordoned"
	}
	logger.GetLogger(ctx).Info("cordon: flag set", "target", target, "cordoned", cordoned)
	fmt.Printf("%s: %s\n", target, verb)
	return nil
}

// exitCodeErrorf builds the coded error (the degraded-fleet verdict).
func exitCodeErrorf(code int, format string, a ...interface{}) error {
	return &exitCodeError{Code: code, Err: fmt.Errorf(format, a...)}
}

// statusDisruptionGate is the DisruptionGate implementation: the fleet
// view (status rows) folded through the pure budget logic. The rows
// come from the same publisher the lease-renew tick maintains — the
// gate is only as fresh as the 5-minute status cadence, which bounds
// every voluntary path it protects.
type statusDisruptionGate struct {
	provider port.Provider
	table    string
}

// CheckDisruption implements port.DisruptionGate.
func (g *statusDisruptionGate) CheckDisruption(ctx context.Context, req port.DisruptionCheckRequest) (port.DisruptionVerdict, error) {
	publisher := port.AsStatusPublisher(g.provider)
	if publisher == nil {
		return port.DisruptionVerdict{
			Allowed: false,
			Reason:  "cloud lacks the status capability (the budget cannot be evaluated — defer)",
		}, nil
	}
	rows, err := publisher.ListStatus(ctx, port.NodeStatusList{Table: g.table})
	if err != nil {
		return port.DisruptionVerdict{}, err
	}
	writers, readers := service.CountHealthyByRole(rows)
	verdict := service.CheckDisruptionBudget(service.BudgetInput{
		HealthyWriters: writers,
		HealthyReaders: readers,
		TargetRole:     model.Role(req.Role),
	})
	return port.DisruptionVerdict{Allowed: verdict.Allowed, Reason: verdict.Reason}, nil
}
