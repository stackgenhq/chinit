// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// This file pins the periodic DNS convergence — the lease-renew timer's
// third half (after the lease renewal and the /etc/hosts convergence).
// A writer terminated by an EC2 health check can never run its drain
// (SSM cannot dispatch to a stopping instance); without the
// convergence its A record keeps pointing at a dead IP — every
// clusterAllReplicas fan-out and schema-sync pass would hang to the
// 60s query timeout on the name. The convergence heals the zone within
// one renewal cycle (5 minutes).

// dnsClaimsProvider is a provider fake carrying both capabilities the
// DNS convergence needs: the lease lister (embedded, the same shape
// inventory_test.go's listerProvider uses) and the DNS registrar. The
// base Provider surface is stubbed the same way the other suite fakes
// stub theirs (Name/Metadata/GetSecretValue/FetchBundle).
type dnsClaimsProvider struct {
	*fakeLeaseLister
	*fakeDNSRegistrar
}

func (p *dnsClaimsProvider) Name() string { return "aws" }

func (p *dnsClaimsProvider) Metadata(context.Context) (port.InstanceMetadata, error) {
	return port.InstanceMetadata{}, nil
}

func (p *dnsClaimsProvider) GetSecretValue(context.Context, port.SecretRequest) (string, error) {
	return "", nil
}

func (p *dnsClaimsProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}

// fakeDNSRegistrar records the A-record operations.
type fakeDNSRegistrar struct {
	zoneID  string
	upserts []port.ARecordRequest
	deletes []port.ARecordRequest
	// deleteFailNames names records whose delete fails (a Route53
	// outage on one name must not stop the sweep of the others).
	deleteFailNames map[string]bool
	// resolveErr drives the zone-resolution degradation.
	resolveErr error
}

func (d *fakeDNSRegistrar) ResolveZoneID(_ context.Context, zoneName string) (string, error) {
	if d.resolveErr != nil {
		return "", d.resolveErr
	}
	return d.zoneID, nil
}

func (d *fakeDNSRegistrar) UpsertARecord(_ context.Context, req port.ARecordRequest) error {
	d.upserts = append(d.upserts, req)
	return nil
}

func (d *fakeDNSRegistrar) DeleteARecord(_ context.Context, req port.ARecordRequest) error {
	d.deletes = append(d.deletes, req)
	if d.deleteFailNames[req.RecordName] {
		return errors.New("route53 delete refused")
	}
	return nil
}

// convergeDNSArgs is the canonical convergence input the specs mutate.
func convergeDNSArgs() ConvergeDNSArgs {
	return ConvergeDNSArgs{
		LeaseTable:  "example-node-claims",
		DNSZoneName: "example.clickhouse.internal",
		Hostname:    "writer-2",
		PrivateIP:   "10.0.1.218",
	}
}

var _ = Describe("ConvergePeerDNS", func() {
	Context("with live claims and a resolvable zone", func() {
		It("self-heals its own record and releases expired-slot records", func() {
			lister := &fakeLeaseLister{claims: []port.Claim{
				{Hostname: "writer-0", IP: "10.0.1.45"},
				{Hostname: "writer-2", IP: "10.0.1.218"},
				{Hostname: "writer-3", IP: "10.0.1.24"},
			}}
			dns := &fakeDNSRegistrar{zoneID: "Z07073102YAGFP019SAW"}
			provider := &dnsClaimsProvider{lister, dns}

			Expect(ConvergePeerDNS(context.Background(), provider, convergeDNSArgs())).To(Succeed())

			// Self-heal: exactly one upsert, for the node's own name.
			Expect(dns.upserts).To(HaveLen(1))
			Expect(dns.upserts[0].RecordName).To(Equal("writer-2"))
			Expect(dns.upserts[0].IP).To(Equal("10.0.1.218"))

			// The expired slot: writer-1 is inside the claimed ordinal
			// space (writer-0..3) but holds no live claim — its record
			// is the dead holder's and must be released.
			Expect(dns.deletes).To(HaveLen(1))
			Expect(dns.deletes[0].RecordName).To(Equal("writer-1"))
			Expect(dns.deletes[0].IP).To(BeEmpty(),
				"the sweep delete carries no IP — the provider lists the live value set")
		})

		It("sweeps every vacant ordinal inside the claimed space", func() {
			lister := &fakeLeaseLister{claims: []port.Claim{
				{Hostname: "writer-0", IP: "10.0.1.45"},
				{Hostname: "writer-3", IP: "10.0.1.24"},
			}}
			dns := &fakeDNSRegistrar{zoneID: "Z07073102YAGFP019SAW"}
			provider := &dnsClaimsProvider{lister, dns}

			Expect(ConvergePeerDNS(context.Background(), provider, convergeDNSArgs())).To(Succeed())

			deleted := map[string]bool{}
			for _, req := range dns.deletes {
				deleted[req.RecordName] = true
			}
			Expect(deleted).To(Equal(map[string]bool{
				"writer-1": true,
				"writer-2": true,
			}),
				"the sweep deletes the vacant ordinals between the claimed bounds")
		})
	})

	Context("degradation", func() {
		It("keeps the zone untouched when the zone does not resolve", func() {
			lister := &fakeLeaseLister{claims: []port.Claim{{Hostname: "writer-0", IP: "10.0.1.45"}}}
			dns := &fakeDNSRegistrar{resolveErr: errors.New("no such zone")}
			provider := &dnsClaimsProvider{lister, dns}

			Expect(ConvergePeerDNS(context.Background(), provider, convergeDNSArgs())).To(Succeed())
			Expect(dns.upserts).To(BeEmpty())
			Expect(dns.deletes).To(BeEmpty())
		})

		It("self-heals but skips the sweep when the claims listing fails", func() {
			lister := &fakeLeaseLister{err: errBoom}
			dns := &fakeDNSRegistrar{zoneID: "Z07073102YAGFP019SAW"}
			provider := &dnsClaimsProvider{lister, dns}

			Expect(ConvergePeerDNS(context.Background(), provider, convergeDNSArgs())).To(Succeed())
			Expect(dns.upserts).To(HaveLen(1),
				"the self-heal runs before the claims listing")
			Expect(dns.deletes).To(BeEmpty())
		})

		It("no-ops without a lease table (static-identity node)", func() {
			lister := &fakeLeaseLister{claims: []port.Claim{{Hostname: "writer-0", IP: "10.0.1.45"}}}
			dns := &fakeDNSRegistrar{zoneID: "Z07073102YAGFP019SAW"}
			provider := &dnsClaimsProvider{lister, dns}
			args := convergeDNSArgs()
			args.LeaseTable = ""

			Expect(ConvergePeerDNS(context.Background(), provider, args)).To(Succeed())
			Expect(dns.upserts).To(BeEmpty())
			Expect(dns.deletes).To(BeEmpty())
		})
	})

	Context("per-slot failure isolation", func() {
		It("continues the sweep when one delete fails", func() {
			lister := &fakeLeaseLister{claims: []port.Claim{
				{Hostname: "writer-0", IP: "10.0.1.45"},
				{Hostname: "writer-3", IP: "10.0.1.24"},
			}}
			dns := &fakeDNSRegistrar{
				zoneID:          "Z07073102YAGFP019SAW",
				deleteFailNames: map[string]bool{"writer-1": true},
			}
			provider := &dnsClaimsProvider{lister, dns}

			Expect(ConvergePeerDNS(context.Background(), provider, convergeDNSArgs())).To(Succeed())

			deleted := map[string]bool{}
			for _, req := range dns.deletes {
				deleted[req.RecordName] = true
			}
			Expect(deleted).To(Equal(map[string]bool{
				"writer-1": true,
				"writer-2": true,
			}),
				"a failing delete must not stop the sweep of the remaining expired slots")
		})
	})

	Context("the sweep-only entrypoint (the shutdown drain's parting sweep)", func() {
		It("releases expired slots without the self-heal upsert", func() {
			lister := &fakeLeaseLister{claims: []port.Claim{
				{Hostname: "writer-0", IP: "10.0.1.45"},
				{Hostname: "writer-2", IP: "10.0.1.218"},
				{Hostname: "writer-3", IP: "10.0.1.24"},
			}}
			dns := &fakeDNSRegistrar{zoneID: "Z07073102YAGFP019SAW"}
			provider := &dnsClaimsProvider{lister, dns}

			Expect(SweepExpiredSlotRecords(context.Background(), provider, convergeDNSArgs())).To(Succeed())

			// NO upsert: the shutdown drain runs this AFTER releasing
			// its own record — a self-heal would re-create it.
			Expect(dns.upserts).To(BeEmpty())
			// The expired slot still sweeps.
			Expect(dns.deletes).To(HaveLen(1))
			Expect(dns.deletes[0].RecordName).To(Equal("writer-1"))
		})
	})
})

var _ = Describe("claimOrdinalSpace", func() {
	It("ignores foreign roles and non-ordinal names", func() {
		claims := []port.Claim{
			{Hostname: "keeper-0", IP: "10.0.1.20"},
			{Hostname: "backup-0", IP: "10.0.1.30"},
			{Hostname: "writer-0", IP: "10.0.1.45"},
			{Hostname: "reader-1", IP: "10.0.1.207"},
		}
		slots := claimOrdinalSpace(claims)
		names := map[string]bool{}
		for _, slot := range slots {
			names[slot.hostname] = true
		}
		Expect(names).To(Equal(map[string]bool{
			"writer-0": true,
			"reader-0": true,
			"reader-1": true,
		}),
			"keeper/backup have no ASG ordinal; reader-1's space includes vacant reader-0")
	})

	It("is empty with no ASG-role claims", func() {
		Expect(claimOrdinalSpace([]port.Claim{{Hostname: "keeper-0"}})).To(BeEmpty())
	})

	It("sweeps retired high-water ordinals up to WriterCeiling and ReaderCeiling when the highest ordinal scales in", func() {
		// Scale-in scenario: writer-1 (of writer_max=2) and reader-0 (of reader_max=2)
		// scaled in or terminated; only writer-0 and reader-1 remain live.
		lister := &fakeLeaseLister{claims: []port.Claim{
			{Hostname: "writer-0", IP: "10.0.1.254"},
			{Hostname: "reader-1", IP: "10.0.1.233"},
		}}
		dns := &fakeDNSRegistrar{zoneID: "Z07073102YAGFP019SAW"}
		provider := &dnsClaimsProvider{lister, dns}

		args := convergeDNSArgs()
		args.Hostname = "writer-0"
		args.PrivateIP = "10.0.1.254"
		args.WriterCeiling = 2
		args.ReaderCeiling = 2

		Expect(ConvergePeerDNS(context.Background(), provider, args)).To(Succeed())

		deleted := map[string]bool{}
		for _, req := range dns.deletes {
			deleted[req.RecordName] = true
		}
		Expect(deleted).To(Equal(map[string]bool{
			"writer-1": true,
			"reader-0": true,
		}), "both writer-1 (above max live writer ordinal 0) and reader-0 must be swept")
	})
})
