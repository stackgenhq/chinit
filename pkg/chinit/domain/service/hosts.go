// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// ConvergeHostsArgs is the input to ConvergePeerHosts: everything the
// hosts sweep needs that is not in the provider/lease table (the spec
// prefixes come from chinit.yaml, the lease table + DNS zone from the
// settled identity).
type ConvergeHostsArgs struct {
	// LeaseTable is the node's own lease table (the claim scan reads
	// the same table the boot read).
	LeaseTable string
	// WriterPrefix/ReaderPrefix are the hostname prefixes the claims
	// must match to earn a hosts line (defaults writer-/reader-).
	WriterPrefix string
	ReaderPrefix string
	// DNSZoneName is the FQDN suffix the line's third field carries
	// (writer-0 writer-0.zone) so FQDN resolution benefits too.
	DNSZoneName string
	// BackupReplicaHostname appends the LEASE-LESS backup replica to
	// the hosts file. The backup is a CR member and a part-fetch
	// source, but it holds no ordinal lease — the claim scan never
	// sees it, so without this field every peer resolving its short
	// name for a fetch died on DNS_ERROR Code 198 (the
	// CloudWatch 14 errors/node/hour, queues stalled). The
	// ADDRESS is derived at convergence time by resolving the FQDN
	// through the private zone — never carried in the spec, so a
	// re-provisioned backup converges without a bundle re-render.
	BackupReplicaHostname string
}

// ConvergePeerHosts updates /etc/hosts on server nodes from the active
// ordinal lease claims so short hostnames ({replica} macros) resolve
// reliably across peer churn.
func ConvergePeerHosts(ctx context.Context, provider port.Provider, args ConvergeHostsArgs) error {
	lister := port.AsLeaseLister(provider)
	if lister == nil || args.LeaseTable == "" {
		// Same degradation as the boot step: a provider without the
		// lease-listing capability (or a state file without the
		// table) keeps whatever DNS gives; never a fatal error.
		logger.GetLogger(ctx).Warn("peer-hosts: no lease-listing capability; short names resolve via DNS")
		return nil
	}
	claims, err := lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: args.LeaseTable})
	if err != nil {
		// Best-effort: a listing outage must not abort the renewal.
		logger.GetLogger(ctx).Warn("peer-hosts: live-claims listing failed; keeping current hosts file", "err", err)
		return nil
	}
	return writePeerHostsFile(ctx, claims, args)
}

// writePeerHostsFile is the sweep-and-append core shared by the boot
// step and the periodic convergence: stale writer/reader lines are
// swept first (a re-run must converge on the CURRENT claims, not
// preserve whatever an older boot wrote — the old contains-check kept
// a wrong entry forever), then one line per live claim.
//
// The hosts path is a seam the tests redirect (never /var/lib
// privileges in the suite). PeerHostsPath exports the seam for the
// app-layer specs (the lease-renew timer drives the convergence
// its specs must redirect the same path the service writes).
var peerHostsPath = func() string { return "/etc/hosts" }

// PeerHostsPath returns the current hosts-file path (test seam).
func PeerHostsPath() func() string { return peerHostsPath }

// SetPeerHostsPath redirects the hosts-file path (test seam).
func SetPeerHostsPath(f func() string) { peerHostsPath = f }

func writePeerHostsFile(ctx context.Context, claims []port.Claim, args ConvergeHostsArgs) error {
	writerPrefix := args.WriterPrefix
	if writerPrefix == "" {
		writerPrefix = "writer-"
	}
	readerPrefix := args.ReaderPrefix
	if readerPrefix == "" {
		readerPrefix = "reader-"
	}

	if existing, err := os.ReadFile(peerHostsPath()); err == nil {
		var kept []string
		for _, line := range strings.Split(string(existing), "\n") {
			stale := false
			sweepPrefixes := []string{writerPrefix, readerPrefix}
			// The backup line is swept with the claims (its IP is
			// re-resolved below each convergence, so a stale IP from
			// a re-provisioned backup never lingers).
			if args.BackupReplicaHostname != "" {
				sweepPrefixes = append(sweepPrefixes, args.BackupReplicaHostname)
			}
			for _, prefix := range sweepPrefixes {
				for _, field := range strings.Fields(line) {
					if strings.HasPrefix(field, prefix) {
						stale = true
						break
					}
				}
				if stale {
					break
				}
			}
			if !stale {
				kept = append(kept, line)
			}
		}
		if err := os.WriteFile(peerHostsPath(), []byte(strings.Join(kept, "\n")+"\n"), 0644); err != nil {
			return fmt.Errorf("rewrite %s: %w", peerHostsPath(), err)
		}
	} else {
		return fmt.Errorf("read %s: %w", peerHostsPath(), err)
	}

	var b strings.Builder
	entries := 0
	// The lease-less backup replica first: same hosts-file line shape
	// as a claim (IP + short name + FQDN). It is a CR member and a
	// part-fetch source — the claim scan cannot carry it. The ADDRESS
	// is resolved HERE from the private zone (the VPC resolver answers
	// the Route53 record Terraform manages), never rendered into the
	// spec: a re-provisioned backup with a new IP converges on the
	// next tick without a bundle re-render. An empty answer (backup
	// down or record gone) writes no line — same contract as a claim
	// row with no IP.
	if args.BackupReplicaHostname != "" {
		var fqdn string
		if args.DNSZoneName != "" {
			fqdn = args.BackupReplicaHostname + "." + args.DNSZoneName
		} else {
			fqdn = args.BackupReplicaHostname
		}
		if addrs, rerr := net.LookupHost(fqdn); rerr == nil && len(addrs) > 0 {
			if args.DNSZoneName != "" {
				fmt.Fprintf(&b, "%s %s %s\n", addrs[0], args.BackupReplicaHostname, fqdn)
			} else {
				fmt.Fprintf(&b, "%s %s\n", addrs[0], args.BackupReplicaHostname)
			}
			entries++
		} else if rerr != nil {
			logger.GetLogger(ctx).Warn("peer-hosts: backup replica FQDN did not resolve; no hosts line written",
				"fqdn", fqdn, "err", rerr)
		}
	}
	for _, claim := range claims {
		writer := strings.HasPrefix(claim.Hostname, writerPrefix)
		reader := strings.HasPrefix(claim.Hostname, readerPrefix)
		if !writer && !reader {
			continue // foreign row (keeper-N): not fetch sources
		}
		if claim.IP == "" {
			continue
		}
		entries++
		if args.DNSZoneName != "" {
			fmt.Fprintf(&b, "%s %s %s.%s\n", claim.IP, claim.Hostname, claim.Hostname, args.DNSZoneName)
		} else {
			fmt.Fprintf(&b, "%s %s\n", claim.IP, claim.Hostname)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(peerHostsPath(), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", peerHostsPath(), err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("append %s: %w", peerHostsPath(), err)
	}
	_ = f.Close()
	logger.GetLogger(ctx).Info("peer-hosts: converged", "entries", entries)
	return nil
}
