// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// This file pins the spec-sync cloud adapters: the per-role refresh
// lock (the config-refresher Lambda's lock table, minus the table —
// the row rides the claims table under a refresh-<role> key) and the
// role-spec S3 read (the absent-key benign answer).

// fakeLockDynamoDB records Put/Delete traffic and answers the
// conditional-put semantics the real table enforces.
type fakeLockDynamoDB struct {
	tables  []string
	items   map[string]claimItem
	failPut bool
}

func (f *fakeLockDynamoDB) PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.tables = append(f.tables, *in.TableName)
	if f.failPut {
		return nil, fmt.Errorf("transport down")
	}
	hostname := (*in.Item["hostname"].(*dynamodbtypes.AttributeValueMemberS)).Value
	var incoming claimItem
	incoming.Hostname = hostname
	if id, ok := in.Item["instance_id"].(*dynamodbtypes.AttributeValueMemberS); ok {
		incoming.InstanceID = id.Value
	}
	if ip, ok := in.Item["ip"].(*dynamodbtypes.AttributeValueMemberS); ok {
		incoming.IP = ip.Value
	}
	if lu, ok := in.Item["lease_until"].(*dynamodbtypes.AttributeValueMemberN); ok {
		_, _ = fmt.Sscanf(lu.Value, "%d", &incoming.LeaseUntil)
	}
	existing, taken := f.items[hostname]
	if taken {
		holderIsMe := existing.InstanceID == incoming.InstanceID
		expired := existing.LeaseUntil < time.Now().Unix()
		if !holderIsMe && !expired {
			return nil, &dynamodbtypes.ConditionalCheckFailedException{
				Message: awsString(fmt.Sprintf("ConditionalCheckFailed for %s", hostname)),
			}
		}
	}
	f.items[hostname] = incoming
	return &dynamodb.PutItemOutput{}, nil
}

func (f *fakeLockDynamoDB) GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeLockDynamoDB) DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	f.tables = append(f.tables, *in.TableName)
	hostname := (*in.Key["hostname"].(*dynamodbtypes.AttributeValueMemberS)).Value
	delete(f.items, hostname)
	return &dynamodb.DeleteItemOutput{}, nil
}

var _ = Describe("DynamoDBRefreshLock", func() {
	req := chinitport.RefreshLockRequest{
		Table:   "example-claims",
		Key:     "refresh-writer",
		Holder:  "writer-0",
		Minutes: 30,
	}

	It("acquires when no row exists and releases on the holder's exit", func() {
		client := &fakeLockDynamoDB{items: map[string]claimItem{}}
		lock := NewDynamoDBRefreshLock("us-east-1").WithClient(client)

		ok, err := lock.AcquireRefreshLock(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(), "an absent row must be acquirable")
		Expect(client.tables).To(ContainElement("example-claims"))

		Expect(lock.ReleaseRefreshLock(context.Background(), req)).To(Succeed())
		_, present := client.items["refresh-writer"]
		Expect(present).To(BeFalse(), "the holder's exit frees the slot")
	})

	It("answers false when a peer holds the lock", func() {
		client := &fakeLockDynamoDB{items: map[string]claimItem{
			"refresh-writer": {Hostname: "refresh-writer", InstanceID: "writer-1", LeaseUntil: time.Now().Unix() + 600},
		}}
		lock := NewDynamoDBRefreshLock("us-east-1").WithClient(client)

		ok, err := lock.AcquireRefreshLock(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "a foreign LIVE row is the benign taken answer, never an error")
	})

	It("lets the holder re-acquire its own row (a crashed converge retry)", func() {
		client := &fakeLockDynamoDB{items: map[string]claimItem{
			"refresh-writer": {Hostname: "refresh-writer", InstanceID: "writer-0", LeaseUntil: time.Now().Unix() + 600},
		}}
		lock := NewDynamoDBRefreshLock("us-east-1").WithClient(client)

		ok, err := lock.AcquireRefreshLock(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(), "the holder re-taking its own lock is the retry path")
	})

	It("reclaims an expired row (the TTL dead-holder safety net)", func() {
		client := &fakeLockDynamoDB{items: map[string]claimItem{
			"refresh-writer": {Hostname: "refresh-writer", InstanceID: "writer-1", LeaseUntil: time.Now().Unix() - 600},
		}}
		lock := NewDynamoDBRefreshLock("us-east-1").WithClient(client)

		ok, err := lock.AcquireRefreshLock(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
	})

	It("errors on a transport failure (the timer retries)", func() {
		client := &fakeLockDynamoDB{items: map[string]claimItem{}, failPut: true}
		lock := NewDynamoDBRefreshLock("us-east-1").WithClient(client)

		_, err := lock.AcquireRefreshLock(context.Background(), req)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("refresh lock acquire"))
	})

	It("refuses an empty table (the bogus-PutItem guard)", func() {
		lock := NewDynamoDBRefreshLock("us-east-1")
		_, err := lock.AcquireRefreshLock(context.Background(), chinitport.RefreshLockRequest{Key: "refresh-writer", Holder: "writer-0"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("claims table"))
	})
})

var _ = Describe("S3RoleSpecSource.FetchRoleSpec", func() {
	req := chinitport.RoleSpecRequest{
		Bucket: "data-bucket",
		Prefix: "bootstrap/chinit",
		Role:   "writer",
	}

	It("answers the spec bytes for a present key", func() {
		src := NewS3RoleSpecSource("us-east-1").WithClient(&fakeS3{
			keys: map[string]string{"bootstrap/chinit/chinit-writer.yaml": "cluster_name: example\n"},
		})

		content, exists, err := src.FetchRoleSpec(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeTrue())
		Expect(string(content)).To(ContainSubstring("cluster_name: example"))
	})

	It("answers absent (not an error) when the key is missing", func() {
		src := NewS3RoleSpecSource("us-east-1").WithClient(&fakeS3{keys: map[string]string{}})

		content, exists, err := src.FetchRoleSpec(context.Background(), req)
		Expect(err).NotTo(HaveOccurred())
		Expect(exists).To(BeFalse(), "a role with no uploaded spec skips benignly")
		Expect(content).To(BeEmpty())
	})

	It("errors on a non-NoSuchKey failure", func() {
		src := NewS3RoleSpecSource("us-east-1").WithClient(&errS3{})

		_, _, err := src.FetchRoleSpec(context.Background(), req)
		Expect(err).To(HaveOccurred())
		Expect(strings.Contains(err.Error(), "NoSuchKey")).To(BeFalse())
	})

	It("refuses an empty bucket (the request names the bundle, never env)", func() {
		src := NewS3RoleSpecSource("us-east-1").WithClient(&fakeS3{})

		_, _, err := src.FetchRoleSpec(context.Background(), chinitport.RoleSpecRequest{Prefix: "p", Role: "writer"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("bundle bucket"))
	})
})

// errS3 answers every GetObject with a transport error.
type errS3 struct{}

func (*errS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return nil, fmt.Errorf("AccessDenied on %s", *in.Key)
}
