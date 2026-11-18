// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	route53types "github.com/aws/aws-sdk-go-v2/service/route53/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// This file pins the A-record DELETE flow: Route53's value-exact DELETE
// semantics (a valueless DELETE is rejected and the
// record lingers after scale-in) and the stale-holder
// fallback — a drain whose identity IP no longer matches the record's
// value (a previous holder's drain never released the name) must sweep
// the LIVE value set by name instead of leaving the dead record behind.

// errDeleteFailed is the canned ChangeResourceRecordSets failure.
var errDeleteFailed = errors.New("InvalidChangeBatch: not found")

// recordSet is one fake record: a name (its map key) and its live values.
type recordSet struct {
	values []string
}

// fakeRoute53 serves ListResourceRecordSets from a canned record set
// map and records every ChangeResourceRecordSets call. deleteFailValues
// names the values whose DELETE fails (the stale-IP mismatch).
type fakeRoute53 struct {
	records          map[string]recordSet
	changes          []string // "action:<name>:<value>" per call
	deleteFailValues map[string]bool
	listErr          error
}

func (f *fakeRoute53) ListHostedZonesByName(ctx context.Context, in *route53.ListHostedZonesByNameInput, opts ...func(*route53.Options)) (*route53.ListHostedZonesByNameOutput, error) {
	return nil, fmt.Errorf("not implemented")
}

func (f *fakeRoute53) ListResourceRecordSets(ctx context.Context, in *route53.ListResourceRecordSetsInput, opts ...func(*route53.Options)) (*route53.ListResourceRecordSetsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	sets := []route53types.ResourceRecordSet{}
	for name, rs := range f.records {
		rrs := make([]route53types.ResourceRecord, 0, len(rs.values))
		for _, v := range rs.values {
			rrs = append(rrs, route53types.ResourceRecord{Value: aws.String(v)})
		}
		sets = append(sets, route53types.ResourceRecordSet{
			Name:            aws.String(name),
			Type:            route53types.RRTypeA,
			TTL:             aws.Int64(60),
			ResourceRecords: rrs,
		})
	}
	return &route53.ListResourceRecordSetsOutput{ResourceRecordSets: sets}, nil
}

func (f *fakeRoute53) ChangeResourceRecordSets(_ context.Context, in *route53.ChangeResourceRecordSetsInput, opts ...func(*route53.Options)) (*route53.ChangeResourceRecordSetsOutput, error) {
	change := in.ChangeBatch.Changes[0]
	name := aws.ToString(change.ResourceRecordSet.Name)
	value := aws.ToString(change.ResourceRecordSet.ResourceRecords[0].Value)
	f.changes = append(f.changes, fmt.Sprintf("%s:%s:%s", change.Action, name, value))
	if change.Action == route53types.ChangeActionDelete && f.deleteFailValues[value] {
		return nil, errDeleteFailed
	}
	if change.Action == route53types.ChangeActionDelete {
		f.removeValue(name, value)
	}
	return &route53.ChangeResourceRecordSetsOutput{}, nil
}

// removeValue drops one value from the fake's live set (the delete's
// side effect the fallback's re-list then observes).
func (f *fakeRoute53) removeValue(name, value string) {
	rs, ok := f.records[name]
	if !ok {
		return
	}
	kept := rs.values[:0]
	for _, v := range rs.values {
		if v != value {
			kept = append(kept, v)
		}
	}
	rs.values = kept
	f.records[name] = rs
}

// newRoute53Registrar builds the registrar over a fake client.
func newRoute53Registrar(fake *fakeRoute53) *Route53Registrar {
	return NewRoute53Registrar("us-west-2").WithClient(fake)
}

// deleteRequest is the canonical drain delete request the specs mutate.
func deleteRequest() chinitport.ARecordRequest {
	return chinitport.ARecordRequest{
		ZoneID:     "Z07073102YAGFP019SAW",
		ZoneName:   "example.clickhouse.internal",
		RecordName: "writer-1",
		TTL:        60,
	}
}

var _ = Describe("Route53 DeleteARecord", func() {
	Context("with the caller's exact IP", func() {
		It("deletes the matching value", func() {
			fake := &fakeRoute53{records: map[string]recordSet{
				"writer-1.example.clickhouse.internal.": {values: []string{"10.0.1.43"}},
			}}
			req := deleteRequest()
			req.IP = "10.0.1.43"

			Expect(newRoute53Registrar(fake).DeleteARecord(context.Background(), req)).To(Succeed())
			Expect(fake.changes).To(HaveLen(1))
			Expect(fake.changes[0]).To(ContainSubstring("DELETE:writer-1.example.clickhouse.internal.:10.0.1.43"))
		})
	})

	Context("without an IP", func() {
		It("lists the live values and deletes each", func() {
			fake := &fakeRoute53{records: map[string]recordSet{
				"writer-1.example.clickhouse.internal.": {values: []string{"10.0.1.236"}},
			}}

			Expect(newRoute53Registrar(fake).DeleteARecord(context.Background(), deleteRequest())).To(Succeed())
			Expect(fake.changes).To(HaveLen(1))
			Expect(fake.changes[0]).To(ContainSubstring(":10.0.1.236"))
		})

		It("no-ops when the record is already gone", func() {
			fake := &fakeRoute53{records: map[string]recordSet{}}

			Expect(newRoute53Registrar(fake).DeleteARecord(context.Background(), deleteRequest())).To(Succeed())
			Expect(fake.changes).To(BeEmpty())
		})
	})

	Context("with a stale requested IP (the previous-holder record)", func() {
		It("falls back to the live value set and releases the dead record", func() {
			// The live record still names the PREVIOUS holder's IP;
			// the drain's identity IP never matches it.
			fake := &fakeRoute53{
				records: map[string]recordSet{
					"writer-1.example.clickhouse.internal.": {values: []string{"10.0.1.236"}},
				},
				deleteFailValues: map[string]bool{"10.0.1.43": true},
			}
			req := deleteRequest()
			req.IP = "10.0.1.43"

			Expect(newRoute53Registrar(fake).DeleteARecord(context.Background(), req)).To(Succeed())
			Expect(fake.changes).To(HaveLen(2),
				"the failed identity-IP delete plus one fallback delete of the live value")
			Expect(fake.changes[0]).To(ContainSubstring(":10.0.1.43"))
			Expect(fake.changes[1]).To(ContainSubstring(":10.0.1.236"))
		})

		It("returns the original error when the fallback list also fails", func() {
			fake := &fakeRoute53{
				records: map[string]recordSet{
					"writer-1.example.clickhouse.internal.": {values: []string{"10.0.1.236"}},
				},
				deleteFailValues: map[string]bool{"10.0.1.43": true},
				listErr:          errors.New("route53 outage"),
			}
			req := deleteRequest()
			req.IP = "10.0.1.43"

			err := newRoute53Registrar(fake).DeleteARecord(context.Background(), req)
			Expect(err).To(MatchError(errDeleteFailed),
				"the primary delete failure is the truth when the fallback cannot even list")
		})
	})

	Context("with ExactOnly (the drain/refresh of a replaced node)", func() {
		It("never sweeps the replacement's live value on a failed exact delete", func() {
			// The 2026-11-17 replacement race: the terminating node's
			// requested-IP delete fails because a live replacement
			// re-registered the record under its own IP. The fallback
			// used to sweep that live value — deleting a healthy
			// node's record. ExactOnly must surface the error and
			// leave the record alone.
			fake := &fakeRoute53{
				records: map[string]recordSet{
					"writer-1.example.clickhouse.internal.": {values: []string{"10.0.1.249"}},
				},
				deleteFailValues: map[string]bool{"10.0.1.43": true},
			}
			req := deleteRequest()
			req.IP = "10.0.1.43"
			req.ExactOnly = true

			err := newRoute53Registrar(fake).DeleteARecord(context.Background(), req)
			Expect(err).To(MatchError(errDeleteFailed),
				"the exact-only delete reports the failed value instead of sweeping live values")
			Expect(fake.changes).To(HaveLen(1),
				"only the failed requested-value delete ran; the live value was never touched")
			Expect(fake.changes[0]).To(ContainSubstring(":10.0.1.43"))
			Expect(strings.Join(fake.changes, " ")).NotTo(ContainSubstring(":10.0.1.249"),
				"the replacement's value survives the drain")
			Expect(fake.records["writer-1.example.clickhouse.internal."].values).To(ContainElement("10.0.1.249"),
				"the record still resolves the replacement")
		})

		It("still deletes its own exact value when it matches (the normal drain)", func() {
			fake := &fakeRoute53{
				records: map[string]recordSet{
					"writer-1.example.clickhouse.internal.": {values: []string{"10.0.1.43"}},
				},
			}
			req := deleteRequest()
			req.IP = "10.0.1.43"
			req.ExactOnly = true

			Expect(newRoute53Registrar(fake).DeleteARecord(context.Background(), req)).To(Succeed())
			Expect(fake.changes).To(HaveLen(1))
			Expect(fake.changes[0]).To(ContainSubstring("DELETE:writer-1.example.clickhouse.internal.:10.0.1.43"))
		})
	})
})
