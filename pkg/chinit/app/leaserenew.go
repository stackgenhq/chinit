// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
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
		// Backup: no lease to renew — run the peer convergences only
		// (the writer/reader claim listing still answers them).
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger.GetLogger(ctx).Info("lease-renew: extending lease",
		"hostname", st.Identity.Hostname,
		"lease_table", st.Identity.LeaseTable,
		"lease_seconds", ordinalLeaseRenewalSeconds)
	err = claimer.RenewLease(ctx, port.RenewRequest{
		Table:        st.Identity.LeaseTable,
		Hostname:     st.Identity.Hostname,
		InstanceID:   st.Identity.InstanceID,
		LeaseSeconds: ordinalLeaseRenewalSeconds,
	})
	if err != nil {
		// SELF-HEAL: a renew against a vanished/re-assigned claim row
		// failed forever (ConditionalCheckFailedException every 5-min
		// tick) while the node kept serving as writer-N — its identity
		// went unclaimed, the hosts convergence lost the peer, and
		// nothing recovered it short of a manual DynamoDB put. The
		// LeaseRecovery service owns the policy: exact re-claim of
		// the node's OWN name, fence on contested, fatal on renumber.
		logger.GetLogger(ctx).Warn("lease-renew: renewal failed — recovering the ordinal",
			"hostname", st.Identity.Hostname, "err", err)
		ordinal := claimOrdinalFromHostname(st.Identity.Hostname)
		if ordinal < 0 {
			logger.GetLogger(ctx).Warn("lease-renew: hostname carries no ordinal — cannot re-claim", "hostname", st.Identity.Hostname)
			return err
		}
		spec, serr := loadInventorySpec()
		if serr != nil || spec == nil {
			logger.GetLogger(ctx).Warn("lease-renew: spec unreadable — cannot re-claim", "err", serr)
			return err
		}
		rctx, rcancel := context.WithTimeout(context.Background(), 16*time.Minute)
		defer rcancel()
		prefix := st.Identity.Hostname[:len(st.Identity.Hostname)-len(fmt.Sprintf("%d", ordinal))]
		recovery := service.NewLeaseRecovery(claimer, st.Identity.LeaseTable)
		outcome, cerr := recovery.Recover(rctx, port.ClaimRequest{
			Table:           st.Identity.LeaseTable,
			Prefix:          prefix,
			Count:           ordinal + 1,
			InstanceID:      st.Identity.InstanceID,
			PrivateIP:       st.Identity.PrivateIP,
			LeaseSeconds:    ordinalLeaseRenewalSeconds,
			DeadlineSeconds: 900,
			ExactOrdinal:    true,
		}, ordinal)
		switch outcome {
		case service.OutcomeReclaimed:
			// fall through to the convergences
		case service.OutcomeContested:
			// Another LIVE instance holds this node's name: the fleet
			// has two nodes believing they are the same replica. The
			// fence quiesces this one and the ASG replaces it; the
			// replacement claims cleanly on its boot.
			logger.GetLogger(ctx).Error("lease-renew: own ordinal held by a live instance — fencing self",
				"hostname", st.Identity.Hostname, "err", cerr)
			return fail.Fatal(fenceSelf(ctx, st, provider))
		default:
			return fail.Fatal(cerr)
		}
	}
	logger.GetLogger(ctx).Info("lease-renew: lease extended", "hostname", st.Identity.Hostname)

	// The fleet-status rider: publish this node's status row on the
	// same tick (best-effort — the renewal already succeeded; a
	// publish failure retries next tick). This is the one write that
	// makes the node OBSERVABLE by the fleet: budget checks, rollout
	// gates, and fleet-status all read these rows.
	publishNodeStatus(ctx, st, provider, st.Identity.LeaseTable)

	// Converge /etc/hosts on the same live claims (best-effort — a
	// convergence failure never fails the renewal; the next tick
	// retries). Keepers have no lease and never reach this path.
	if st.Identity.Role != model.RoleKeeper {
		args := service.ConvergeHostsArgs{
			LeaseTable:  st.Identity.LeaseTable,
			DNSZoneName: st.dnsZone(),
		}
		// The spec prefixes come from the bundle the boot installed;
		// a missing/unparsable spec falls back to the shared
		// defaults (writer-/reader-).
		if spec, err := loadInventorySpec(); err == nil {
			args.WriterPrefix = spec.writerPrefix()
			args.ReaderPrefix = spec.readerPrefix()
			args.DNSZoneName = spec.DNSZoneName
			// The lease-less backup replica: the claim scan never
			// lists it, so its hosts line rides the hostname (the
			// address is resolved at convergence time — the
			// Code 198 peers resolving its
			// short name for part fetches died on DNS_ERROR).
			args.BackupReplicaHostname = spec.backupReplicaHostname()
		}
		if err := service.ConvergePeerHosts(ctx, provider, args); err != nil {
			logger.GetLogger(ctx).Warn("lease-renew: peer-hosts convergence failed", "err", err)
		}
		// DNS convergence on the same tick: self-upsert plus the
		// expired-slot record sweep (best-effort, same contract).
		// Keepers never reach this path; static nodes (backup) have no
		// lease and are excluded above.
		dnsArgs := service.ConvergeDNSArgs{
			LeaseTable:  st.Identity.LeaseTable,
			DNSZoneName: st.dnsZone(),
			Hostname:    st.Identity.Hostname,
			PrivateIP:   st.Identity.PrivateIP,
		}
		if spec, err := loadInventorySpec(); err == nil && spec.DNSZoneName != "" {
			dnsArgs.DNSZoneName = spec.DNSZoneName
		}
		if err := service.ConvergePeerDNS(ctx, provider, dnsArgs); err != nil {
			logger.GetLogger(ctx).Warn("lease-renew: peer-dns convergence failed", "err", err)
		}

		// remote_servers convergence on the same tick: the rendered
		// inventory is the LIST half of the residue healing (hosts +
		// DNS heal the RESOLUTION; this heals the membership). Same
		// best-effort contract — a failure keeps the current file
		// and the next tick retries. Server nodes only (keepers
		// render no CHI section; backup is excluded above).
		if st.Identity.Role != model.RoleKeeper && st.Identity.Role != model.RoleBackup {
			if spec, err := loadSpec(inventorySpecPath()); err == nil {
				boot := &service.Boot{
					Spec:     spec,
					Provider: provider,
					Meta: port.InstanceMetadata{
						Provider:   provider.Name(),
						Region:     st.Identity.Region,
						InstanceID: st.Identity.InstanceID,
						PrivateIP:  st.Identity.PrivateIP,
					},
					Identity: st.Identity,
					// StatePath stays empty: the convergence renders
					// CONTENT only — it never touches the state store.
				}
				if err := boot.ConvergeRemoteServers(ctx); err != nil {
					logger.GetLogger(ctx).Warn("lease-renew: remote-servers convergence failed", "err", err)
				}
			} else {
				logger.GetLogger(ctx).Warn("lease-renew: spec unreadable; remote-servers convergence skipped", "err", err)
			}
		}
	}
	return nil
}

// convergeStaticNodePeers runs the peer convergences for a lease-less
// role (backup): hosts + DNS, never the lease extension. The lease
// TABLE name comes from the inventory spec (the boot persisted no
// LeaseTable on a static identity), so the claims listing the hosts
// convergence reads is derived from the bundle — the same table name
// every lease-mode boot read. Best-effort per convergence (the timer
// retries in 5 minutes).
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
	spec, err := loadInventorySpec()
	if err != nil || spec == nil {
		logger.GetLogger(ctx).Warn("lease-renew: spec unreadable — static peer convergence skipped", "err", err)
		return nil
	}
	provider, err := buildProvider(providerNameOf(st), st.Identity.Region)
	if err != nil {
		return err
	}

	hostsArgs := service.ConvergeHostsArgs{
		LeaseTable:   spec.ClaimerTableName,
		WriterPrefix: spec.writerPrefix(),
		ReaderPrefix: spec.readerPrefix(),
		DNSZoneName:  spec.DNSZoneName,
		// The lease-less backup replica (Code 198): hostname only
		// the address resolves at convergence time.
		BackupReplicaHostname: spec.backupReplicaHostname(),
	}
	if err := service.ConvergePeerHosts(ctx, provider, hostsArgs); err != nil {
		logger.GetLogger(ctx).Warn("lease-renew: peer-hosts convergence failed", "err", err)
	}
	dnsArgs := service.ConvergeDNSArgs{
		LeaseTable:  spec.ClaimerTableName,
		DNSZoneName: spec.DNSZoneName,
		Hostname:    st.Identity.Hostname,
		PrivateIP:   st.Identity.PrivateIP,
	}
	if err := service.ConvergePeerDNS(ctx, provider, dnsArgs); err != nil {
		logger.GetLogger(ctx).Warn("lease-renew: peer-dns convergence failed", "err", err)
	}
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
