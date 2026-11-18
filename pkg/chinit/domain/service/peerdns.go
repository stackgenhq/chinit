// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// dnsRecordTTLSeconds is the A-record TTL the convergence writes: one
// renewal cycle (5 minutes) is long enough to keep resolver caches off
// Route53, short enough that a replaced IP heals quickly.
const dnsRecordTTLSeconds = 60

// ConvergeDNSArgs is the input to the DNS convergence: everything the
// sweep needs that is not in the provider (the lease table + private
// zone come from the settled identity, the node's own name/IP from the
// same identity record).
type ConvergeDNSArgs struct {
	// LeaseTable is the node's own lease table (the claim scan reads
	// the same table the boot read).
	LeaseTable string
	// DNSZoneName is the private zone whose writer-*/reader-* records
	// are converged.
	DNSZoneName string
	// Hostname/PrivateIP identify the converging node's own record
	// the self-heal upsert keeps the renewal authoritative for the
	// node's name even when a prior drain missed it.
	Hostname  string
	PrivateIP string
	// WriterPrefix/ReaderPrefix are the hostname prefixes for ASG roles
	// (defaults to writer-/reader- when empty).
	WriterPrefix string
	ReaderPrefix string
	// WriterCeiling/ReaderCeiling bound the full reserved ordinal space
	// (writer_max / reader_max) so scaling in the highest-ordinal node
	// (e.g. writer-1 or reader-1 terminating, leaving only writer-0)
	// still sweeps the retired high-water ordinal's stale A record.
	WriterCeiling int
	ReaderCeiling int
}

// dnsConverger is the DNS half of the lease-renew convergence: it owns
// the private zone's server-role A records on every renewal tick.
//
// Why a type and not a function: the convergence has TWO failure-
// independent halves (self-heal and the expired-slot sweep) sharing
// one zone resolution and one claims listing; a struct keeps that
// shared state (zone, dns, lister) from being re-derived in every
// leaf call, and keeps the constructor as the single place the
// capability/zone guards live.
type dnsConverger struct {
	// dns is the private-zone registrar capability (nil-able: clouds
	// without private-zone DNS keep their records as-is).
	dns port.DNSRegistrar
	// lister enumerates the active ordinal leases (nil-able same as
	// dns).
	lister port.LeaseLister
	// zone is the resolved private-zone id + name the records live in.
	zone dnsZone
}

// dnsZone names the target of one convergence pass.
type dnsZone struct {
	// ID is the provider's zone handle (Route53 hosted-zone id).
	ID string
	// Name is the zone's DNS name (record-name suffixing).
	Name string
}

// ConvergePeerDNS converges the private zone's server-role A records:
// one upsert for SELF, and a delete for every ordinal name whose lease
// expired (a dead holder's record).
//
// Why periodic, not drain-only: the drain's DNS release is best-effort
// by design — an instance terminated by an EC2 health check never runs
// its drain (SSM cannot dispatch to a stopping instance), and the
// record lingers pointing at a dead IP. Peers resolve the name on
// every clusterAllReplicas fan-out / schema-sync / remote_servers
// connection, so ONE stale record wedges the whole fleet's
// coordination (
// writer-1.example.clickhouse.internal kept a dead IP for hours
// after its holder died mid-churn; every schema-sync pass hung to the
// 60s query timeout on it, fresh writers stayed table-less, and the
// ingest/merge load pinned one writer while the ASG scaled into
// nodes that could not share the work).
//
// The expired-slot delete is safe against the replacement race: a
// replacement claims the ordinal BEFORE it boots, so the record for a
// live claim is never a delete candidate; a record whose lease
// expired has no live holder by definition.
//
// Failure semantics: best-effort, never fatal — the same contract as
// the hosts convergence. Guards and outages log a warning; the next
// tick retries.
func ConvergePeerDNS(ctx context.Context, provider port.Provider, args ConvergeDNSArgs) error {
	c, ok := newDNSConverger(ctx, provider, args)
	if !ok {
		return nil // guard logged
	}
	c.selfHeal(ctx, args)
	return c.sweep(ctx, args)
}

func SweepExpiredSlotRecords(ctx context.Context, provider port.Provider, args ConvergeDNSArgs) error {
	c, ok := newDNSConverger(ctx, provider, args)
	if !ok {
		return nil // guard logged
	}
	return c.sweep(ctx, args)
}

// newDNSConverger builds the converger after the capability and zone
// guards. A provider without the DNS/lease capabilities (or a zone
// that does not resolve) is the documented degradation, not an
// error: the zone's records stay as-is.
func newDNSConverger(ctx context.Context, provider port.Provider, args ConvergeDNSArgs) (*dnsConverger, bool) {
	dns := port.AsDNSRegistrar(provider)
	lister := port.AsLeaseLister(provider)
	if dns == nil || lister == nil || args.LeaseTable == "" || args.Hostname == "" {
		logger.GetLogger(ctx).Warn("peer-dns: no DNS/lease capability; zone records stay as-is")
		return nil, false
	}
	zoneID, err := dns.ResolveZoneID(ctx, args.DNSZoneName)
	if err != nil || zoneID == "" {
		logger.GetLogger(ctx).Warn("peer-dns: zone resolution failed; zone records stay as-is",
			"zone", args.DNSZoneName, "err", err)
		return nil, false
	}
	return &dnsConverger{
		dns:    dns,
		lister: lister,
		zone:   dnsZone{ID: zoneID, Name: args.DNSZoneName},
	}, true
}

// sweep lists claims and releases the expired-slot records.
// The self-heal ordering contract differs per entrypoint: the periodic
// convergence self-heals FIRST (the node's own record must stay
// correct even when the lease table is unreachable); the shutdown
// sweep never self-heals (see SweepExpiredSlotRecords).
func (c *dnsConverger) sweep(ctx context.Context, args ConvergeDNSArgs) error {
	claims, err := c.activeClaims(ctx, args.LeaseTable)
	if err != nil {
		// Best-effort: a listing outage must not abort the renewal.
		logger.GetLogger(ctx).Warn("peer-dns: live-claims listing failed; expired-slot sweep skipped", "err", err)
		return nil
	}
	if released := c.sweepExpiredSlots(ctx, claims, args); released > 0 {
		logger.GetLogger(ctx).Info("peer-dns: released stale records", "count", released)
	}
	return nil
}

// activeClaims lists leases through the provider's lease
// capability.
func (c *dnsConverger) activeClaims(ctx context.Context, table string) ([]port.Claim, error) {
	return c.lister.ListActiveClaims(ctx, port.ListClaimsRequest{Table: table})
}

// selfHeal re-asserts the node's OWN record: the renewal is
// authoritative for the node's name even when the boot's upsert or
// the drain's delete missed (best-effort — an outage logs and the
// next tick retries).
func (c *dnsConverger) selfHeal(ctx context.Context, args ConvergeDNSArgs) {
	if args.PrivateIP == "" {
		return
	}
	if err := c.dns.UpsertARecord(ctx, port.ARecordRequest{
		ZoneID:     c.zone.ID,
		ZoneName:   c.zone.Name,
		RecordName: args.Hostname,
		IP:         args.PrivateIP,
		TTL:        dnsRecordTTLSeconds,
	}); err != nil {
		logger.GetLogger(ctx).Warn("peer-dns: self upsert failed", "hostname", args.Hostname, "err", err)
	}
}

// sweepExpiredSlots deletes the A record of every ordinal name in the
// claimed/reserved ordinal space whose lease is NOT live (a dead
// holder's record), returning how many records it released.
func (c *dnsConverger) sweepExpiredSlots(ctx context.Context, claims []port.Claim, args ...ConvergeDNSArgs) int {
	slots := claimOrdinalSpace(claims, args...)
	if len(slots) == 0 {
		return 0
	}
	released := 0
	for _, slot := range slots {
		if slot.isLive(claims) {
			continue
		}
		if c.deleteRecord(ctx, slot.hostname) != nil {
			continue // per-slot failure logged; the sweep continues
		}
		released++
	}
	return released
}

// deleteRecord removes one ordinal name's A record; the request
// carries no IP so the provider lists value set (the exact
// Route53 DELETE semantics).
func (c *dnsConverger) deleteRecord(ctx context.Context, name string) error {
	if err := c.dns.DeleteARecord(ctx, port.ARecordRequest{
		ZoneID:     c.zone.ID,
		ZoneName:   c.zone.Name,
		RecordName: name,
		TTL:        dnsRecordTTLSeconds,
	}); err != nil {
		logger.GetLogger(ctx).Warn("peer-dns: expired-slot delete failed", "name", name, "err", err)
		return err
	}
	return nil
}

// ConvergeDNSOpts is a domain collection of optional ConvergeDNSArgs.
type ConvergeDNSOpts []ConvergeDNSArgs

// writerPrefix returns the configured writer hostname prefix or the default.
func (a ConvergeDNSArgs) writerPrefix() string {
	if a.WriterPrefix != "" {
		return a.WriterPrefix
	}
	return "writer-"
}

// readerPrefix returns the configured reader hostname prefix or the default.
func (a ConvergeDNSArgs) readerPrefix() string {
	if a.ReaderPrefix != "" {
		return a.ReaderPrefix
	}
	return "reader-"
}

// seedCeilings populates the initial role maxima from configured
// WriterCeiling and ReaderCeiling bounds.
func (opts ConvergeDNSOpts) seedCeilings(maxima map[string]int, seen map[string]bool) {
	if len(opts) == 0 {
		return
	}
	a := opts[0]
	if a.WriterCeiling > 0 {
		wp := a.writerPrefix()
		maxima[wp] = a.WriterCeiling - 1
		seen[wp] = true
	}
	if a.ReaderCeiling > 0 {
		rp := a.readerPrefix()
		maxima[rp] = a.ReaderCeiling - 1
		seen[rp] = true
	}
}

// slotName is one ordinal-space name (writer-3): a value type that
// carries its own liveness test against the claims list.
type slotName struct {
	hostname string
}

// slotNames is a domain collection of slotName entries.
type slotNames []slotName

// isLive reports whether the slot's ordinal holds a live claim.
func (s slotName) isLive(claims []port.Claim) bool {
	for _, claim := range claims {
		if claim.Hostname == s.hostname {
			return true
		}
	}
	return false
}

// claimOrdinalSpace enumerates the ordinal names claims and role
// ceilings cover: for every writer-/reader- prefix seen in the claims
// or configured via WriterCeiling/ReaderCeiling, all ordinals 0..max
// are candidates — a vacant ordinal inside the claimed or ceiling range
// is the dead-holder record the sweep exists to remove (including when
// the highest-ordinal writer or reader scales in).
func claimOrdinalSpace(claims []port.Claim, optArgs ...ConvergeDNSArgs) slotNames {
	maxima, seen := map[string]int{}, map[string]bool{}
	ConvergeDNSOpts(optArgs).seedCeilings(maxima, seen)
	for _, claim := range claims {
		prefix, ordinal, ok := splitOrdinalClaim(claim.Hostname)
		if !ok {
			continue // foreign row (backup-0, keeper-N): no ordinal space
		}
		if !seen[prefix] || ordinal > maxima[prefix] {
			maxima[prefix] = ordinal
			seen[prefix] = true
		}
	}
	var slots slotNames
	for prefix, maxOrdinal := range maxima {
		for i := 0; i <= maxOrdinal; i++ {
			slots = append(slots, slotName{hostname: fmt.Sprintf("%s%d", prefix, i)})
		}
	}
	return slots
}

// ordinalClaimPrefixes are the hostname prefixes that carry a claimable
// ordinal (ASG roles). Keepers/backup are static-identity roles: their
// records are the boot's upsert alone and never sweep candidates.
var ordinalClaimPrefixes = []string{"writer-", "reader-"}

// splitOrdinalClaim splits a claim hostname into its prefix and
// ordinal (writer-3 -> writer-, 3). Names without a recognized prefix
// or a non-numeric suffix answer ok=false.
func splitOrdinalClaim(name string) (string, int, bool) {
	for _, prefix := range ordinalClaimPrefixes {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		ordinal, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err == nil && ordinal >= 0 {
			return prefix, ordinal, true
		}
	}
	return "", -1, false
}
