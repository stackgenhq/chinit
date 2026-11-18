// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws_test

import (
	"context"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/aws"
)

// statusStoreFakeDynamoDB: the same conditional surface as the task
// store's fake, applied to the status/ key space. A condition that
// does not match is a ConditionalCheckFailed.
type statusStoreFakeDynamoDB struct {
	items map[string]map[string]dynamodbtypes.AttributeValue
}

func newStatusStoreFake() *statusStoreFakeDynamoDB {
	return &statusStoreFakeDynamoDB{items: map[string]map[string]dynamodbtypes.AttributeValue{}}
}

func (f *statusStoreFakeDynamoDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	hostname := in.Item["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	if row, live := f.items[hostname]; live {
		// The publish's condition: absent, expired, or the same
		// instance. A foreign LIVE row is the identity-moved refusal.
		now := attrN(in.ExpressionAttributeValues, ":now")
		leaseUntil := attrN(row, "lease_until")
		instanceID := row["instance_id"].(*dynamodbtypes.AttributeValueMemberS).Value
		me := in.ExpressionAttributeValues[":me"].(*dynamodbtypes.AttributeValueMemberS).Value
		if leaseUntil < now || instanceID == me {
			f.items[hostname] = in.Item
			return &dynamodb.PutItemOutput{}, nil
		}
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	f.items[hostname] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *statusStoreFakeDynamoDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	hostname := in.Key["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	if row, ok := f.items[hostname]; ok {
		return &dynamodb.GetItemOutput{Item: row}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (f *statusStoreFakeDynamoDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	hostname := in.Key["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	row, ok := f.items[hostname]
	if !ok {
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	if *in.ConditionExpression == "attribute_exists(hostname)" {
		// The cordon toggle: cordoned + updated_at.
		cordoned := in.ExpressionAttributeValues[":c"].(*dynamodbtypes.AttributeValueMemberBOOL).Value
		now := in.ExpressionAttributeValues[":now"].(*dynamodbtypes.AttributeValueMemberN).Value
		row["cordoned"] = &dynamodbtypes.AttributeValueMemberBOOL{Value: cordoned}
		row["updated_at"] = &dynamodbtypes.AttributeValueMemberN{Value: now}
		return &dynamodb.UpdateItemOutput{}, nil
	}
	return nil, &dynamodbtypes.ConditionalCheckFailedException{}
}

func (f *statusStoreFakeDynamoDB) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	out := &dynamodb.ScanOutput{Items: []map[string]dynamodbtypes.AttributeValue{}}
	now := attrN(in.ExpressionAttributeValues, ":now")
	for _, row := range f.items {
		hostname := row["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
		if len(hostname) > 7 && hostname[:7] == "status/" {
			if attrN(row, "lease_until") > now {
				out.Items = append(out.Items, row)
			}
		}
	}
	return out, nil
}

var _ = Describe("DynamoDBStatusPublisher", func() {
	var (
		store *aws.DynamoDBStatusPublisher
		fake  *statusStoreFakeDynamoDB
		ctx   context.Context
	)
	const table = "node-claims"

	BeforeEach(func() {
		fake = newStatusStoreFake()
		store = aws.NewDynamoDBStatusPublisher("us-east-1").WithClient(fake)
		ctx = context.Background()
	})

	It("publishes keyed under the status/ prefix", func() {
		err := store.PublishStatus(ctx, chinitport.NodeStatusPublish{
			Table: table,
			Status: chinitport.NodeStatus{
				Hostname: "writer-0", Role: "writer", InstanceID: "i-0",
				Health: chinitport.NodeHealthy, ClickHouseVersion: "24.8.7.53",
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.items).To(HaveKey("status/writer-0"), "the status/ prefix keeps the row out of the claim and task key spaces")
	})

	It("refuses a foreign instance over a live row", func() {
		publish := chinitport.NodeStatusPublish{
			Table:  table,
			Status: chinitport.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Health: chinitport.NodeHealthy},
		}
		Expect(store.PublishStatus(ctx, publish)).To(Succeed())
		publish.Status.InstanceID = "i-1"
		err := store.PublishStatus(ctx, publish)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("identity moved"))
	})

	It("lists the fleet and applies the hostname filter", func() {
		for _, host := range []string{"writer-0", "reader-1"} {
			Expect(store.PublishStatus(ctx, chinitport.NodeStatusPublish{
				Table:  table,
				Status: chinitport.NodeStatus{Hostname: host, InstanceID: "i-" + host, Health: chinitport.NodeHealthy},
			})).To(Succeed())
		}
		all, err := store.ListStatus(ctx, chinitport.NodeStatusList{Table: table})
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(2))
		one, err := store.ListStatus(ctx, chinitport.NodeStatusList{Table: table, Hostname: "reader-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(one).To(HaveLen(1))
		Expect(one[0].Hostname).To(Equal("reader-1"))
	})

	It("cordons via a targeted update that survives a subsequent publish", func() {
		Expect(store.PublishStatus(ctx, chinitport.NodeStatusPublish{
			Table:  table,
			Status: chinitport.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Health: chinitport.NodeHealthy},
		})).To(Succeed())
		Expect(store.SetCordon(ctx, chinitport.NodeCordonRequest{Table: table, Hostname: "writer-0", Cordoned: true})).To(Succeed())
		rows, _ := store.ListStatus(ctx, chinitport.NodeStatusList{Table: table})
		Expect(rows[0].Cordoned).To(BeTrue())
		// The node's own publish keeps the operator's flag.
		Expect(store.PublishStatus(ctx, chinitport.NodeStatusPublish{
			Table:  table,
			Status: chinitport.NodeStatus{Hostname: "writer-0", InstanceID: "i-0", Health: chinitport.NodeHealthy},
		})).To(Succeed())
		rows, _ = store.ListStatus(ctx, chinitport.NodeStatusList{Table: table})
		Expect(rows[0].Cordoned).To(BeTrue(), "the publish upserts the node's fields; the cordon flag rides through")
	})

	It("errors naming the node on cordon-before-publish", func() {
		err := store.SetCordon(ctx, chinitport.NodeCordonRequest{Table: table, Hostname: "writer-9", Cordoned: true})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("writer-9"))
	})

	It("guards the empty table", func() {
		err := store.PublishStatus(ctx, chinitport.NodeStatusPublish{
			Table:  "",
			Status: chinitport.NodeStatus{Hostname: "writer-0"},
		})
		Expect(err).To(HaveOccurred())
	})
})

// silence the unused helpers the task-store fake owns.
var (
	_ = attributevalue.MarshalMap
	_ = strconv.Itoa
	_ = time.Now
)
