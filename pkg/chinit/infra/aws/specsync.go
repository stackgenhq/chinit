// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// S3RoleSpecSource reads one role spec from the bundle prefix — the
// config-refresher Lambda's S3 GET, without the Lambda (the node
// pulls its own spec on the timer; the apply-time env snapshot is
// gone by construction: the request carries the bundle's bucket/
// prefix from the spec the node already trusts).
type S3RoleSpecSource struct {
	region string
	client S3API
}

// NewS3RoleSpecSource builds the production reader.
func NewS3RoleSpecSource(region string) *S3RoleSpecSource {
	return &S3RoleSpecSource{region: region}
}

// WithClient injects a concrete API client (tests).
func (s *S3RoleSpecSource) WithClient(c S3API) *S3RoleSpecSource {
	s.client = c
	return s
}

func (s *S3RoleSpecSource) clientOrInit(ctx context.Context) (S3API, error) {
	if s.client != nil {
		return s.client, nil
	}
	cfg, err := awsConfig(ctx, s.region)
	if err != nil {
		return nil, err
	}
	s.client = s3.NewFromConfig(cfg)
	return s.client, nil
}

// FetchRoleSpec implements port.RoleSpecSource. An absent key is the
// benign answer (Exists=false): a role whose spec the stack never
// uploaded — the backup with backups disabled — must skip, not error
// (the timer would otherwise page on a healthy fleet).
func (s *S3RoleSpecSource) FetchRoleSpec(ctx context.Context, req port.RoleSpecRequest) ([]byte, bool, error) {
	if req.Bucket == "" {
		return nil, false, fmt.Errorf("role-spec fetch requires a bundle bucket (the spec's config_bundle.bucket is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return nil, false, err
	}
	key := strings.TrimPrefix(req.Prefix+"/", "/") + "chinit-" + req.Role + ".yaml"
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsString(req.Bucket),
		Key:    awsString(key),
	})
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("fetch s3://%s/%s: %w", req.Bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, false, fmt.Errorf("read s3://%s/%s: %w", req.Bucket, key, err)
	}
	if len(body) == 0 {
		return nil, false, fmt.Errorf("s3://%s/%s is empty", req.Bucket, key)
	}
	return body, true, nil
}

// lockDB is the SDK surface the lock needs (the claimer + cooldown
// client surface; the fake seam is one type for both).
type lockDB interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// DynamoDBRefreshLock holds the lock state.
type DynamoDBRefreshLock struct {
	region string
	client lockDB
}

// NewDynamoDBRefreshLock builds the production lock.
func NewDynamoDBRefreshLock(region string) *DynamoDBRefreshLock {
	l := &DynamoDBRefreshLock{region: region}
	if region != "" {
		cfg, err := awsConfig(context.Background(), region)
		if err == nil {
			l.client = dynamodb.NewFromConfig(cfg)
		}
	}
	return l
}

// WithClient is the conformance-suite injection seam (the same
// Put/Get/Delete surface the claimer holds).
func (l *DynamoDBRefreshLock) WithClient(db lockDB) *DynamoDBRefreshLock {
	l.client = db
	return l
}

func (l *DynamoDBRefreshLock) clientOrInit(ctx context.Context) (lockDB, error) {
	if l.client != nil {
		return l.client, nil
	}
	cfg, err := awsConfig(ctx, l.region)
	if err != nil {
		return nil, err
	}
	l.client = dynamodb.NewFromConfig(cfg)
	return l.client, nil
}

// AcquireRefreshLock implements port.RefreshLock. False is the benign
// "a peer holds it" answer; only a transport error is an error.
func (l *DynamoDBRefreshLock) AcquireRefreshLock(ctx context.Context, req port.RefreshLockRequest) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("refresh lock requires the claims table (RefreshLockRequest.Table is empty — check claimer_table_name in the chinit spec)")
	}
	client, err := l.clientOrInit(ctx)
	if err != nil {
		return false, err
	}
	minutes := req.Minutes
	if minutes <= 0 {
		minutes = 30
	}
	now := time.Now().Unix()
	expiry := now + int64(minutes)*60
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: awsString(req.Table),
		Item: map[string]ddbtypes.AttributeValue{
			"hostname":    &ddbtypes.AttributeValueMemberS{Value: req.Key},
			"instance_id": &ddbtypes.AttributeValueMemberS{Value: req.Holder},
			"ip":          &ddbtypes.AttributeValueMemberS{Value: "refresh-lock"},
			"lease_until": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(expiry, 10)},
		},
		// Same shape as the ordinal claim: not-exists, expired, or the
		// holder's own row. A foreign LIVE row is ConditionalCheckFailed
		// — the expected "taken" answer, never an error.
		ConditionExpression: awsString("attribute_not_exists(hostname) OR lease_until < :now OR instance_id = :me"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":now": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
			":me":  &ddbtypes.AttributeValueMemberS{Value: req.Holder},
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	return false, fmt.Errorf("refresh lock acquire %s/%s: %w", req.Table, req.Key, err)
}

// ReleaseRefreshLock implements port.RefreshLock: the holder's exit
// path deletes the row; a row another holder owns is a no-op.
func (l *DynamoDBRefreshLock) ReleaseRefreshLock(ctx context.Context, req port.RefreshLockRequest) error {
	if req.Table == "" {
		return nil
	}
	client, err := l.clientOrInit(ctx)
	if err != nil {
		return err
	}
	_, err = client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: awsString(req.Table),
		Key: map[string]ddbtypes.AttributeValue{
			"hostname": &ddbtypes.AttributeValueMemberS{Value: req.Key},
		},
		// Ownership guard: only the holder's exit frees the slot (a
		// late release from a crashed converge cannot yank the lock
		// from the peer that took it after the TTL lapsed).
		ConditionExpression: awsString("instance_id = :me"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":me": &ddbtypes.AttributeValueMemberS{Value: req.Holder},
		},
	})
	if err != nil && !isConditionalCheckFailed(err) {
		return fmt.Errorf("refresh lock release %s/%s: %w", req.Table, req.Key, err)
	}
	return nil
}
