// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// selfClaimFakeDynamoDB records every condition expression so the specs
// can assert the self-claim disjunct is present. It answers PutItem as
// if the condition failed for every ordinal except the one the caller
// flags as "owned by us".
type selfClaimFakeDynamoDB struct {
	ownedOrdinal int // the ordinal this instance holds; -1 = none
	putAttempts  int
	conditions   []string
}

func (f *selfClaimFakeDynamoDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putAttempts++
	f.conditions = append(f.conditions, *in.ConditionExpression)
	hostname := in.Item["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	if f.ownedOrdinal >= 0 && hostname == hostnameFromOrdinal("writer-", f.ownedOrdinal) {
		// Own lease: the instance_id = :me disjunct lets our put succeed.
		if contains(*in.ConditionExpression, "instance_id = :me") {
			return &dynamodb.PutItemOutput{}, nil
		}
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	return nil, &dynamodbtypes.ConditionalCheckFailedException{}
}

func (f *selfClaimFakeDynamoDB) UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *selfClaimFakeDynamoDB) DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	return &dynamodb.DeleteItemOutput{}, nil
}

func hostnameFromOrdinal(prefix string, n int) string {
	if n == 0 {
		return prefix + "0"
	}
	suffix := ""
	for x := n; x > 0; x /= 10 {
		suffix = string(rune('0'+x%10)) + suffix
	}
	return prefix + suffix
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

var _ = Describe("DynamoDBClaimer self-claim", func() {
	It("re-claims the ordinal this instance already holds instead of skipping it", func() {
		fake := &selfClaimFakeDynamoDB{ownedOrdinal: 1}
		claimer := NewDynamoDBClaimer("us-east-1").WithClient(fake)
		got, err := claimer.Claim(context.Background(), chinitport.ClaimRequest{
			Table:           "node-claims",
			Prefix:          "writer-",
			Count:           3,
			InstanceID:      "i-self",
			LeaseSeconds:    900,
			DeadlineSeconds: 5,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(1), "the node must re-claim ITS OWN lease rather than drifting to another ordinal")
		for _, cond := range fake.conditions {
			Expect(cond).To(ContainSubstring("instance_id = :me"),
				"every put must carry the self-claim disjunct so a refresh re-claim cannot steal/move the lease")
		}
	})

	It("keeps walking when the self-held ordinal is taken but OTHER ordinals are claimed by others", func() {
		fake := &selfClaimFakeDynamoDB{ownedOrdinal: -1} // we hold nothing
		claimer := NewDynamoDBClaimer("us-east-1").WithClient(fake)
		_, err := claimer.Claim(context.Background(), chinitport.ClaimRequest{
			Table:           "node-claims",
			Prefix:          "writer-",
			Count:           2,
			InstanceID:      "i-self",
			LeaseSeconds:    900,
			DeadlineSeconds: 2,
		})
		// All ordinals refuse the write — the claim must fail, not silently succeed.
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("DynamoDBClaimer ExactOrdinal (the re-claim path)", func() {
	It("re-takes the caller's own ordinal and never attempts another hostname", func() {
		fake := &selfClaimFakeDynamoDB{ownedOrdinal: 2}
		claimer := NewDynamoDBClaimer("us-east-1").WithClient(fake)
		got, err := claimer.Claim(context.Background(), chinitport.ClaimRequest{
			Table:           "node-claims",
			Prefix:          "writer-",
			Count:           3, // ordinal 2 — the caller's own
			InstanceID:      "i-self",
			LeaseSeconds:    900,
			DeadlineSeconds: 5,
			ExactOrdinal:    true,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(2), "the exact re-claim returns the caller's own ordinal")
		Expect(fake.putAttempts).To(Equal(1),
			"the exact shape attempts exactly ONE hostname — never walks lower ordinals")
	})

	It("answers a distinct held-by-another error instead of walking or waiting out the window", func() {
		fake := &selfClaimFakeDynamoDB{ownedOrdinal: -1} // nothing is ours
		claimer := NewDynamoDBClaimer("us-east-1").WithClient(fake)
		_, err := claimer.Claim(context.Background(), chinitport.ClaimRequest{
			Table:           "node-claims",
			Prefix:          "writer-",
			Count:           1, // ordinal 0, held by a live replacement
			InstanceID:      "i-self",
			LeaseSeconds:    900,
			DeadlineSeconds: 2,
			ExactOrdinal:    true,
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("held by another live instance"),
			"the caller must know its own name is held to fence itself — not the sweep's window-expired message")
		Expect(fake.putAttempts).To(Equal(1),
			"one attempt at the exact hostname; no sweep, no wait-and-retry loop")
	})
})
