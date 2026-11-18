// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// ordinalLeaseRenewalSeconds keeps the RENEWAL's extension short
// (15 min): renewal runs every 5 minutes, so the claim never holds
// more than 15 min past a dead renewer — fast detection of a wedged
// lease-renew (the timer failing) while the BOOT claim carries the
// reboot-surviving window (ordinalLeaseSeconds, in the domain
// service). The renewal's job is liveness; the boot claim's job is
// identity stability through churn.
const ordinalLeaseRenewalSeconds = 900

type leaseRenewCoordinator struct {
	st       *nodeState
	provider port.Provider
	spec     *model.BootSpec
}

func runLeaseRenew(args []string) error {
	statePath, err := statePathFlag(args)
	if err != nil {
		return err
	}
	st, err := loadNodeState(statePath)
	if err != nil {
		return err
	}
	if st.Identity.LeaseTable == "" && st.Identity.Role != model.RoleBackup {
		return fail.Fatal(fmt.Errorf("node holds no lease (identity has no lease table)"))
	}
	if st.Identity.LeaseTable == "" {
		return convergeStaticNodePeers(st)
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	claimer := port.AsOrdinalClaimer(provider)
	if claimer == nil {
		return fail.Fatal(fmt.Errorf("cloud %s has no lease capability", provider.Name()))
	}
	spec, _ := loadSpec(inventorySpecPath())
	coord := &leaseRenewCoordinator{st: st, provider: provider, spec: spec}
	return coord.run(claimer)
}

func (c *leaseRenewCoordinator) run(claimer port.OrdinalClaimer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.extendOrRecover(ctx, claimer); err != nil {
		return err
	}
	publishNodeStatus(ctx, c.st, c.provider, c.st.Identity.LeaseTable)
	if c.st.Identity.Role != model.RoleKeeper {
		c.convergePeers(ctx, c.st.Identity.LeaseTable, true)
	}
	return nil
}

func (c *leaseRenewCoordinator) extendOrRecover(ctx context.Context, claimer port.OrdinalClaimer) error {
	logger.GetLogger(ctx).Info("lease-renew: extending lease",
		"hostname", c.st.Identity.Hostname,
		"lease_table", c.st.Identity.LeaseTable,
		"lease_seconds", ordinalLeaseRenewalSeconds)
	err := claimer.RenewLease(ctx, port.RenewRequest{
		Table:        c.st.Identity.LeaseTable,
		Hostname:     c.st.Identity.Hostname,
		InstanceID:   c.st.Identity.InstanceID,
		LeaseSeconds: ordinalLeaseRenewalSeconds,
	})
	if err == nil {
		logger.GetLogger(ctx).Info("lease-renew: lease extended", "hostname", c.st.Identity.Hostname)
		return nil
	}
	return c.recoverOrdinal(ctx, claimer, err)
}

func (c *leaseRenewCoordinator) recoverOrdinal(ctx context.Context, claimer port.OrdinalClaimer, renewErr error) error {
	logger.GetLogger(ctx).Warn("lease-renew: renewal failed — recovering the ordinal",
		"hostname", c.st.Identity.Hostname, "err", renewErr)
	ordinal := claimOrdinalFromHostname(c.st.Identity.Hostname)
	if ordinal < 0 {
		logger.GetLogger(ctx).Warn("lease-renew: hostname carries no ordinal — cannot re-claim", "hostname", c.st.Identity.Hostname)
		return renewErr
	}
	inv, serr := loadInventorySpec()
	if serr != nil || inv == nil {
		logger.GetLogger(ctx).Warn("lease-renew: spec unreadable — cannot re-claim", "err", serr)
		return renewErr
	}
	rctx, rcancel := context.WithTimeout(context.Background(), 16*time.Minute)
	defer rcancel()
	prefix := c.st.Identity.Hostname[:len(c.st.Identity.Hostname)-len(fmt.Sprintf("%d", ordinal))]
	recovery := service.NewLeaseRecovery(claimer, c.st.Identity.LeaseTable)
	outcome, cerr := recovery.Recover(rctx, port.ClaimRequest{
		Table:           c.st.Identity.LeaseTable,
		Prefix:          prefix,
		Count:           ordinal + 1,
		InstanceID:      c.st.Identity.InstanceID,
		PrivateIP:       c.st.Identity.PrivateIP,
		LeaseSeconds:    ordinalLeaseRenewalSeconds,
		DeadlineSeconds: 900,
		ExactOrdinal:    true,
	}, ordinal)
	switch outcome {
	case service.OutcomeReclaimed:
		logger.GetLogger(ctx).Info("lease-renew: lease extended", "hostname", c.st.Identity.Hostname)
		return nil
	case service.OutcomeContested:
		logger.GetLogger(ctx).Error("lease-renew: own ordinal held by a live instance — fencing self",
			"hostname", c.st.Identity.Hostname, "err", cerr)
		return fail.Fatal(fenceSelf(ctx, c.st, c.provider))
	default:
		return fail.Fatal(cerr)
	}
}

func (c *leaseRenewCoordinator) convergePeers(ctx context.Context, leaseTable string, upsertSelfDNS bool) {
	c.convergeHostsAndDNS(ctx, leaseTable, upsertSelfDNS)
	if c.st.Identity.Role == model.RoleKeeper || c.st.Identity.Role == model.RoleBackup {
		return
	}
	if c.spec == nil {
		logger.GetLogger(ctx).Warn("lease-renew: spec unreadable; remote-servers convergence skipped")
		return
	}
	boot := &service.Boot{
		Spec:     c.spec,
		Provider: c.provider,
		Meta: port.InstanceMetadata{
			Provider:   c.provider.Name(),
			Region:     c.st.Identity.Region,
			InstanceID: c.st.Identity.InstanceID,
			PrivateIP:  c.st.Identity.PrivateIP,
		},
		Identity: c.st.Identity,
	}
	if err := boot.ConvergeRemoteServers(ctx); err != nil {
		logger.GetLogger(ctx).Warn("lease-renew: remote-servers convergence failed", "err", err)
	}
}

func (c *leaseRenewCoordinator) convergeHostsAndDNS(ctx context.Context, leaseTable string, upsertSelfDNS bool) {
	args := service.ConvergeHostsArgs{
		LeaseTable:  leaseTable,
		DNSZoneName: c.st.dnsZone(),
	}
	if inv, err := loadInventorySpec(); err == nil && inv != nil {
		args.WriterPrefix = inv.writerPrefix()
		args.ReaderPrefix = inv.readerPrefix()
		if inv.DNSZoneName != "" {
			args.DNSZoneName = inv.DNSZoneName
		}
		args.BackupReplicaHostname = inv.backupReplicaHostname()
	}

	prevHosts, _ := os.ReadFile("/etc/hosts")
	if err := service.ConvergePeerHosts(ctx, c.provider, args); err != nil {
		logger.GetLogger(ctx).Warn("lease-renew: peer-hosts convergence failed", "err", err)
	} else if curHosts, rerr := os.ReadFile("/etc/hosts"); rerr == nil && !bytes.Equal(curHosts, prevHosts) {
		service.DropDNSCache(ctx)
	}

	dnsArgs := service.ConvergeDNSArgs{
		LeaseTable:  leaseTable,
		DNSZoneName: args.DNSZoneName,
	}
	if upsertSelfDNS {
		dnsArgs.Hostname = c.st.Identity.Hostname
		dnsArgs.PrivateIP = c.st.Identity.PrivateIP
	}
	if err := service.ConvergePeerDNS(ctx, c.provider, dnsArgs); err != nil {
		logger.GetLogger(ctx).Warn("lease-renew: peer-dns convergence failed", "err", err)
	}
}

// fenceSelf retires an identity-contested node via the SelfFence
// service: another LIVE instance holds this node's own ordinal, so
// the fleet has two nodes answering one name (the split-brain). The
// service quiesces the server, then marks the instance unhealthy so
// the ASG replaces it. The DNS record is left for the converger: it
// belongs to the live holder.
func fenceSelf(ctx context.Context, st *nodeState, provider port.Provider) error {
	fence := service.NewSelfFenceService(
		&cliNodeQuiescer{queryContext: ctx},
		port.AsInstanceFencer(provider),
	)
	result, err := fence.Fence(ctx, st.Identity.Hostname, st.Identity.InstanceID)
	if err != nil {
		var capErr *service.FenceCapabilityError
		if errors.As(err, &capErr) {
			// The node is quiesced but the cloud cannot mark it
			// unhealthy: the loud fallback so a human sees it.
			logger.GetLogger(ctx).Error("fence: capability absent — manual replacement needed",
				"hostname", st.Identity.Hostname, "err", err)
		}
		return fmt.Errorf("fence: %w", err)
	}
	_ = result // the journal lines carry the outcome
	return nil
}

func convergeStaticNodePeers(st *nodeState) error {
	ctx := context.Background()
	inv, err := loadInventorySpec()
	if err != nil || inv == nil {
		logger.GetLogger(ctx).Warn("lease-renew: spec unreadable — static peer convergence skipped", "err", err)
		return nil
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}
	coord := &leaseRenewCoordinator{st: st, provider: provider}
	coord.convergeHostsAndDNS(ctx, inv.ClaimerTableName, false)
	return nil
}

// providerNameOf answers the cloud the boot detected. The identity
// record's dedicated field arrives with the state-file schema bump;
// until then the detected cloud comes from the environment, defaulting
// to aws.
func providerNameOf(*nodeState) string {
	if os.Getenv("CHINIT_CLOUD") != "" {
		return os.Getenv("CHINIT_CLOUD")
	}
	return "aws"
}

// buildProvider constructs the subcommands' provider from the
// registry (the boot's provider instance died with its process). A
// package-level var so the drain's SSM-invocation specs inject a
// fake — production always builds the real registry provider.
//
// The state's REGION threads through: the boot resolves it from IMDS
// and persists it in the identity, but subcommands run later (timers,
// SSM) with no IMDS probe — without it the AWS clients fail
// "Missing Region" on their first call (
// the reader's live-claims scan fell back to the static 12-slot list
// and every vacant-peer DNS lookup died).
var buildProvider = func(cloud string, region string) (port.Provider, error) {
	return registry().Build(cloud, port.Config{Region: region})
}
