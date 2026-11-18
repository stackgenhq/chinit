// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// nodeStatusItem is the status row: the "status/<hostname>" key keeps
// it out of the claim ("reader-0") and task ("task/<id>") key spaces —
// one table, three disjoint prefixes.
type nodeStatusItem struct {
	Hostname          string `dynamodbav:"hostname"`
	Role              string `dynamodbav:"role"`
	InstanceID        string `dynamodbav:"instance_id"`
	SpecHash          string `dynamodbav:"spec_hash"`
	ClickHouseVersion string `dynamodbav:"ch_version"`
	Health            string `dynamodbav:"health"`
	KeeperQuorum      bool   `dynamodbav:"keeper_quorum"`
	Cordoned          bool   `dynamodbav:"cordoned"`
	UpdatedAt         int64  `dynamodbav:"updated_at"`
	// LeaseUntil mirrors the claim's TTL: a status row whose owner is
	// gone ages out of the "live" view the same way its claim does.
	LeaseUntil int64 `dynamodbav:"lease_until"`
}

// statusKey is the row's partition key.
func statusKey(hostname string) string { return "status/" + hostname }

// statusRowTTL is how long a status row outlives its last publish
// (aligned with the lease window: a row whose node stopped renewing
// stops being fleet truth within the same window its claim expires).
const statusRowTTL = 15 * time.Minute

// DynamoDBStatusPublisher implements port.StatusPublisher over the
// claims table's primitives.
type DynamoDBStatusPublisher struct {
	region string
	client fleetTaskDB
}

// NewDynamoDBStatusPublisher builds the production publisher.
func NewDynamoDBStatusPublisher(region string) *DynamoDBStatusPublisher {
	return &DynamoDBStatusPublisher{region: region}
}

// WithClient injects a concrete API client (tests/conformance).
func (s *DynamoDBStatusPublisher) WithClient(db fleetTaskDB) *DynamoDBStatusPublisher {
	s.client = db
	return s
}

func (s *DynamoDBStatusPublisher) clientOrInit(ctx context.Context) (fleetTaskDB, error) {
	if s.client != nil {
		return s.client, nil
	}
	cfg, err := awsConfig(ctx, s.region)
	if err != nil {
		return nil, err
	}
	s.client = dynamodb.NewFromConfig(cfg)
	return s.client, nil
}

// PublishStatus implements port.StatusPublisher: an upsert keyed by
// the node's own hostname. The CAS refuses a foreign instance_id
// writing over a live row (one node can never overwrite another's
// status); a row whose TTL expired is freely reclaimable. The cordon
// flag is PRESERVED from the existing row — the toggle owns the flag,
// the publish owns the node's fields (a cordon racing a publish keeps
// both).
func (s *DynamoDBStatusPublisher) PublishStatus(ctx context.Context, req chinitport.NodeStatusPublish) (err error) {
	if req.Table == "" {
		return fmt.Errorf("status publish requires the claims table (NodeStatusPublish.Table is empty)")
	}
	if req.Status.Hostname == "" {
		return fmt.Errorf("status publish requires Hostname")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	// The pre-read preserves the cordon flag (the CAS alone cannot
	// express "keep one field" on a PutItem).
	existing, err := s.readStatus(ctx, client, req.Table, req.Status.Hostname)
	if err != nil {
		return err
	}
	if existing != nil && existing.InstanceID != req.Status.InstanceID && existing.LeaseUntil >= now {
		return fmt.Errorf("status publish: another instance holds the %s status row (identity moved?)", req.Status.Hostname)
	}
	cordoned := req.Status.Cordoned
	if existing != nil && existing.Cordoned {
		cordoned = true
	}
	item := nodeStatusItem{
		Hostname:          statusKey(req.Status.Hostname),
		Role:              req.Status.Role,
		InstanceID:        req.Status.InstanceID,
		SpecHash:          req.Status.SpecHash,
		ClickHouseVersion: req.Status.ClickHouseVersion,
		Health:            string(req.Status.Health),
		KeeperQuorum:      req.Status.KeeperQuorum,
		Cordoned:          cordoned,
		UpdatedAt:         now,
		LeaseUntil:        now + int64(statusRowTTL/time.Second),
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshal status item: %w", err)
	}
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           awsString(req.Table),
		Item:                av,
		ConditionExpression: awsString("attribute_not_exists(hostname) OR lease_until < :now OR instance_id = :me"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":now": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
			":me":  &dynamodbtypes.AttributeValueMemberS{Value: req.Status.InstanceID},
		},
	})
	if err == nil {
		return nil
	}
	if isConditionalCheckFailed(err) {
		// A foreign live row under OUR hostname: the identity lease
		// moved (a replacement claimed our name). Never wedge the
		// timer — the new owner's next publish overwrites the row.
		return fmt.Errorf("status publish: another instance holds the %s status row (identity moved?)", req.Status.Hostname)
	}
	return fmt.Errorf("publish status %s: %w", req.Status.Hostname, err)
}

// readStatus fetches one status row (nil when absent) — the publish
// path's cordon-preservation read.
func (s *DynamoDBStatusPublisher) readStatus(ctx context.Context, client fleetTaskDB, table, hostname string) (*nodeStatusItem, error) {
	out, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: awsString(table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: statusKey(hostname)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get status %s: %w", hostname, err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var row nodeStatusItem
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return nil, fmt.Errorf("unmarshal status %s: %w", hostname, err)
	}
	return &row, nil
}

// ListStatus implements port.StatusPublisher: one Scan over the
// "status/" prefix (the fleet's rows are tens at most).
func (s *DynamoDBStatusPublisher) ListStatus(ctx context.Context, req chinitport.NodeStatusList) ([]chinitport.NodeStatus, error) {
	if req.Table == "" {
		return nil, fmt.Errorf("status list requires the claims table (NodeStatusList.Table is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	var statuses []chinitport.NodeStatus
	var start map[string]dynamodbtypes.AttributeValue
	for {
		out, err := client.Scan(ctx, &dynamodb.ScanInput{
			TableName:        awsString(req.Table),
			FilterExpression: awsString("begins_with(hostname, :prefix) AND lease_until > :now"),
			ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
				":prefix": &dynamodbtypes.AttributeValueMemberS{Value: "status/"},
				":now":    &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("scan status rows in %s: %w", req.Table, err)
		}
		for _, item := range out.Items {
			var row nodeStatusItem
			if err := attributevalue.UnmarshalMap(item, &row); err != nil {
				return nil, fmt.Errorf("unmarshal status row in %s: %w", req.Table, err)
			}
			hostname := row.Hostname[len("status/"):]
			if req.Hostname != "" && hostname != req.Hostname {
				continue
			}
			statuses = append(statuses, chinitport.NodeStatus{
				Hostname:          hostname,
				Role:              row.Role,
				InstanceID:        row.InstanceID,
				SpecHash:          row.SpecHash,
				ClickHouseVersion: row.ClickHouseVersion,
				Health:            chinitport.NodeHealth(row.Health),
				KeeperQuorum:      row.KeeperQuorum,
				Cordoned:          row.Cordoned,
				UpdatedAt:         row.UpdatedAt,
			})
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		start = out.LastEvaluatedKey
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Hostname < statuses[j].Hostname })
	return statuses, nil
}

// SetCordon implements port.StatusPublisher: a targeted attribute
// update (never a whole-row replace — a cordon racing a lease-renew
// publish keeps both sides' fields). The row must exist; absent-row is
// an error naming the node (cordon before first publish is a wiring
// mistake, loud by design).
func (s *DynamoDBStatusPublisher) SetCordon(ctx context.Context, req chinitport.NodeCordonRequest) error {
	if req.Table == "" {
		return fmt.Errorf("cordon requires the claims table (NodeCordonRequest.Table is empty)")
	}
	if req.Hostname == "" {
		return fmt.Errorf("cordon requires Hostname")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return err
	}
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: awsString(req.Table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: statusKey(req.Hostname)},
		},
		UpdateExpression:    awsString("SET cordoned = :c, updated_at = :now"),
		ConditionExpression: awsString("attribute_exists(hostname)"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":c":   &dynamodbtypes.AttributeValueMemberBOOL{Value: req.Cordoned},
			":now": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Unix(), 10)},
		},
	})
	if err == nil {
		return nil
	}
	if isConditionalCheckFailed(err) {
		return fmt.Errorf("cordon %s: no status row (the node has never published — wait for its first lease-renew tick)", req.Hostname)
	}
	return fmt.Errorf("cordon %s: %w", req.Hostname, err)
}

// Compile-time capability check.
var _ chinitport.StatusPublisher = (*DynamoDBStatusPublisher)(nil)
