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

// fleetTaskDB is the DynamoDB surface the task store needs. Kept
// separate from DynamoDBAPI (the claimer's seam) and lockDB (the
// refresh lock's) so each fake contract stays independently evolvable;
// a real client satisfies all three.
type fleetTaskDB interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Scan(ctx context.Context, in *dynamodb.ScanInput, opts ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// fleetTaskItem is the task row. The hostname key space is shared with
// the claims: task rows key as "task/<task-id>" so the claim rows
// ("reader-0", "writer-1") and the task rows never collide — one
// table, two disjoint key prefixes, the same conditional-write
// primitives.
type fleetTaskItem struct {
	// Hostname is the partition key ("task/<task-id>").
	Hostname string `dynamodbav:"hostname"`
	// TaskID is the logical id (TaskType-target-epoch at submit).
	TaskID string `dynamodbav:"task_id"`
	// Type is the motion (refresh/rollout/reseed).
	Type string `dynamodbav:"type"`
	// Target is the executing node's cluster hostname.
	Target string `dynamodbav:"target"`
	// Phase is the state-machine position.
	Phase string `dynamodbav:"phase"`
	// Epoch is the fencing token.
	Epoch int64 `dynamodbav:"epoch"`
	// InstanceID carries the claiming executor's identity (the CAS
	// disjunct for heartbeats: only the holder extends).
	InstanceID string `dynamodbav:"instance_id"`
	// IP carries the executor's private IP (diagnostics parity with
	// the claim rows; never a secret).
	IP string `dynamodbav:"ip"`
	// LeaseUntil doubles as the executing heartbeat deadline: the claim
	// and every heartbeat extend it; the sweep reads it.
	LeaseUntil int64 `dynamodbav:"lease_until"`
	// HeartbeatSeconds is the cadence the claim installed.
	HeartbeatSeconds int `dynamodbav:"heartbeat_seconds"`
	// Retries counts re-dispatches.
	Retries int `dynamodbav:"retries"`
	// MaxRetries is the re-dispatch budget.
	MaxRetries int `dynamodbav:"max_retries"`
	// Payload is the opaque, non-secret executor argument.
	Payload string `dynamodbav:"payload"`
	// Reason is the terminal note.
	Reason string `dynamodbav:"reason,omitempty"`
	// UpdatedAt is the last write time.
	UpdatedAt int64 `dynamodbav:"updated_at"`
}

// DynamoDBFleetTaskStore implements port.FleetTaskStore over the claims
// table's own primitives: conditional Put for submit, conditional
// Update for every transition (the epoch is the fence), Scan for the
// sweep and the poll.
type DynamoDBFleetTaskStore struct {
	region string
	client fleetTaskDB
}

// NewDynamoDBFleetTaskStore builds the production store.
func NewDynamoDBFleetTaskStore(region string) *DynamoDBFleetTaskStore {
	return &DynamoDBFleetTaskStore{region: region}
}

// WithClient injects a concrete API client (tests/conformance).
func (s *DynamoDBFleetTaskStore) WithClient(db fleetTaskDB) *DynamoDBFleetTaskStore {
	s.client = db
	return s
}

func (s *DynamoDBFleetTaskStore) clientOrInit(ctx context.Context) (fleetTaskDB, error) {
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

// taskKey is the row's partition key: the "task/" prefix keeps task
// rows out of the claim rows' key space.
func taskKey(taskID string) string { return "task/" + taskID }

// fleetTaskMaxRetries resolves the submit's budget: zero applies the
// default; -1 is the explicit zero-retry budget (fail on first
// staleness).
func fleetTaskMaxRetries(req int) int {
	switch {
	case req > 0:
		return req
	case req == -1:
		return 0
	default:
		return chinitport.FleetTaskDefaultMaxRetries
	}
}

// SubmitTask implements port.FleetTaskStore. The condition refuses a
// live duplicate (phase != done/failed): the CAS is the atomicity
// primitive — two coordinators submitting the same id serialize, one
// wins, the other takes the refused-duplicate answer.
func (s *DynamoDBFleetTaskStore) SubmitTask(ctx context.Context, req chinitport.FleetTaskSubmit) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task submit requires the claims table (FleetTaskSubmit.Table is empty — check claimer_table_name in the chinit spec)")
	}
	if req.TaskID == "" || req.Target == "" {
		return false, fmt.Errorf("fleet task submit requires TaskID and Target")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	item := fleetTaskItem{
		Hostname:         taskKey(req.TaskID),
		TaskID:           req.TaskID,
		Type:             string(req.Type),
		Target:           req.Target,
		Phase:            string(chinitport.TaskPending),
		Epoch:            1,
		InstanceID:       "",
		IP:               "",
		LeaseUntil:       now,
		HeartbeatSeconds: chinitport.FleetTaskDefaultHeartbeatSeconds,
		Retries:          0,
		MaxRetries:       fleetTaskMaxRetries(req.MaxRetries),
		Payload:          req.Payload,
		UpdatedAt:        now,
	}
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return false, fmt.Errorf("marshal fleet task item: %w", err)
	}
	// A live row with the same TaskID (phase not done/failed) is the
	// "already queued" refusal. A terminal row is replaced with a
	// fresh epoch — the resubmit path.
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           awsString(req.Table),
		Item:                av,
		ConditionExpression: awsString("attribute_not_exists(hostname) OR phase IN (:done, :failed)"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":done":   &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskDone)},
			":failed": &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskFailed)},
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	return false, fmt.Errorf("submit fleet task %s: %w", req.TaskID, err)
}

// getTask reads one row (the transition paths re-read before every
// CAS write — a stale coordinator never acts on a cached row).
func (s *DynamoDBFleetTaskStore) getTask(ctx context.Context, client fleetTaskDB, table, taskID string) (*fleetTaskItem, error) {
	out, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: awsString(table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: taskKey(taskID)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("get fleet task %s: %w", taskID, err)
	}
	if out.Item == nil {
		return nil, fmt.Errorf("fleet task %s not found", taskID)
	}
	var row fleetTaskItem
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return nil, fmt.Errorf("unmarshal fleet task %s: %w", taskID, err)
	}
	return &row, nil
}

// ClaimTask implements port.FleetTaskStore: the target's own pending
// row moves to executing under the claimed epoch. The CAS refuses a
// foreign executor, a non-pending row, and — the partition race — a
// claim that races the sweep's re-dispatch (the epoch condition).
func (s *DynamoDBFleetTaskStore) ClaimTask(ctx context.Context, req chinitport.FleetTaskClaim) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task claim requires the claims table (FleetTaskClaim.Table is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return false, err
	}
	row, err := s.getTask(ctx, client, req.Table, req.TaskID)
	if err != nil {
		return false, err
	}
	if row.Target != req.Executor {
		// A foreign claim is the benign refusal: the row belongs to
		// another node's poll path.
		return false, nil
	}
	heartbeat := req.HeartbeatSeconds
	if heartbeat <= 0 {
		heartbeat = chinitport.FleetTaskDefaultHeartbeatSeconds
	}
	now := time.Now().Unix()
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: awsString(req.Table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: taskKey(req.TaskID)},
		},
		UpdateExpression:    awsString("SET phase = :exec, epoch = :epoch, instance_id = :me, lease_until = :lease, heartbeat_seconds = :hb, updated_at = :now"),
		ConditionExpression: awsString("phase = :pending AND target = :target"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":pending": &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskPending)},
			":exec":    &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskExecuting)},
			":target":  &dynamodbtypes.AttributeValueMemberS{Value: row.Target},
			":epoch":   &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(req.Epoch, 10)},
			":me":      &dynamodbtypes.AttributeValueMemberS{Value: req.Executor},
			":lease":   &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now+int64(heartbeat), 10)},
			":hb":      &dynamodbtypes.AttributeValueMemberN{Value: strconv.Itoa(heartbeat)},
			":now":     &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		// Another poll won the claim, or the sweep re-dispatched (epoch
		// moved): the next tick sees the winner's phase. Benign.
		return false, nil
	}
	return false, fmt.Errorf("claim fleet task %s: %w", req.TaskID, err)
}

// HeartbeatTask implements port.FleetTaskStore: only the row's current
// executor at the current epoch can extend the lease (the instance_id
// disjunct covers the executor's own re-claim after a retry that
// re-issued the same epoch).
func (s *DynamoDBFleetTaskStore) HeartbeatTask(ctx context.Context, req chinitport.FleetTaskHeartbeat) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task heartbeat requires the claims table (FleetTaskHeartbeat.Table is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: awsString(req.Table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: taskKey(req.TaskID)},
		},
		UpdateExpression:    awsString("SET lease_until = :lease, updated_at = :now"),
		ConditionExpression: awsString("phase = :exec AND target = :me AND epoch = :epoch"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":exec":  &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskExecuting)},
			":me":    &dynamodbtypes.AttributeValueMemberS{Value: req.Executor},
			":epoch": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(req.Epoch, 10)},
			":lease": &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now+chinitport.FleetTaskDefaultHeartbeatSeconds, 10)},
			":now":   &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		// The epoch moved (re-dispatch) or the row is terminal: the
		// executor must abort — its task was taken away.
		return false, nil
	}
	return false, fmt.Errorf("heartbeat fleet task %s: %w", req.TaskID, err)
}

// terminalWrite is the shared CAS for Complete/Fail: phase must be
// executing (or ack), the executor must be the target, the epoch must
// match. A stale executor's verdict is refused, never an error.
func (s *DynamoDBFleetTaskStore) terminalWrite(ctx context.Context, req chinitport.FleetTaskVerdict, phase chinitport.FleetTaskPhase) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task verdict requires the claims table (FleetTaskVerdict.Table is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: awsString(req.Table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: taskKey(req.TaskID)},
		},
		UpdateExpression:    awsString("SET phase = :phase, reason = :reason, updated_at = :now"),
		ConditionExpression: awsString("phase IN (:exec, :ack) AND target = :me AND epoch = :epoch"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":phase":  &dynamodbtypes.AttributeValueMemberS{Value: string(phase)},
			":exec":   &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskExecuting)},
			":ack":    &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskAck)},
			":me":     &dynamodbtypes.AttributeValueMemberS{Value: req.Executor},
			":epoch":  &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(req.Epoch, 10)},
			":reason": &dynamodbtypes.AttributeValueMemberS{Value: req.Reason},
			":now":    &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		return false, nil
	}
	return false, fmt.Errorf("fleet task %s write %s: %w", req.TaskID, phase, err)
}

// CompleteTask implements port.FleetTaskStore.
func (s *DynamoDBFleetTaskStore) CompleteTask(ctx context.Context, req chinitport.FleetTaskVerdict) (bool, error) {
	return s.terminalWrite(ctx, req, chinitport.TaskDone)
}

// FailTask implements port.FleetTaskStore.
func (s *DynamoDBFleetTaskStore) FailTask(ctx context.Context, req chinitport.FleetTaskVerdict) (bool, error) {
	return s.terminalWrite(ctx, req, chinitport.TaskFailed)
}

// RetryTask implements port.FleetTaskStore: the sweep's re-dispatch.
// Epoch+1 fences the old executor's in-flight writes; retries+1
// counts against the budget; the row returns to pending so the target
// node's next poll claims it.
func (s *DynamoDBFleetTaskStore) RetryTask(ctx context.Context, req chinitport.FleetTaskVerdict) (bool, error) {
	if req.Table == "" {
		return false, fmt.Errorf("fleet task retry requires the claims table (FleetTaskVerdict.Table is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return false, err
	}
	row, err := s.getTask(ctx, client, req.Table, req.TaskID)
	if err != nil {
		return false, err
	}
	if row.Retries >= row.MaxRetries {
		// The budget is spent: terminal failed is the caller's answer,
		// not a re-dispatch.
		return false, nil
	}
	now := time.Now().Unix()
	_, err = client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: awsString(req.Table),
		Key: map[string]dynamodbtypes.AttributeValue{
			"hostname": &dynamodbtypes.AttributeValueMemberS{Value: taskKey(req.TaskID)},
		},
		UpdateExpression:    awsString("SET phase = :pending, epoch = epoch + :one, retries = retries + :one, instance_id = :clear, lease_until = :lease, updated_at = :now"),
		ConditionExpression: awsString("phase IN (:exec, :ack, :failed, :pending) AND epoch = :epoch"),
		ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
			":pending": &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskPending)},
			":exec":    &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskExecuting)},
			":ack":     &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskAck)},
			":failed":  &dynamodbtypes.AttributeValueMemberS{Value: string(chinitport.TaskFailed)},
			":epoch":   &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(row.Epoch, 10)},
			":one":     &dynamodbtypes.AttributeValueMemberN{Value: "1"},
			":clear":   &dynamodbtypes.AttributeValueMemberS{Value: ""},
			":lease":   &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
			":now":     &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now, 10)},
		},
	})
	if err == nil {
		return true, nil
	}
	if isConditionalCheckFailed(err) {
		// Another coordinator raced the epoch: the winner's dispatch is
		// the decision. Benign.
		return false, nil
	}
	return false, fmt.Errorf("retry fleet task %s: %w", req.TaskID, err)
}

// ListFleetTasks implements port.FleetTaskStore: one Scan over the
// "task/" key prefix (the fleet's rows are tens at most; the RCU note
// is the claim lister's). The Target filter is code-side (a Scan filter
// on target would cost the same scan).
func (s *DynamoDBFleetTaskStore) ListFleetTasks(ctx context.Context, req chinitport.FleetTaskList) ([]chinitport.FleetTask, error) {
	if req.Table == "" {
		return nil, fmt.Errorf("fleet task list requires the claims table (FleetTaskList.Table is empty)")
	}
	client, err := s.clientOrInit(ctx)
	if err != nil {
		return nil, err
	}
	var tasks []chinitport.FleetTask
	var start map[string]dynamodbtypes.AttributeValue
	for {
		out, err := client.Scan(ctx, &dynamodb.ScanInput{
			TableName:        awsString(req.Table),
			FilterExpression: awsString("begins_with(hostname, :prefix)"),
			ExpressionAttributeValues: map[string]dynamodbtypes.AttributeValue{
				":prefix": &dynamodbtypes.AttributeValueMemberS{Value: "task/"},
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("scan fleet tasks in %s: %w", req.Table, err)
		}
		for _, item := range out.Items {
			var row fleetTaskItem
			if err := attributevalue.UnmarshalMap(item, &row); err != nil {
				return nil, fmt.Errorf("unmarshal fleet task row in %s: %w", req.Table, err)
			}
			if req.Target != "" && row.Target != req.Target {
				continue
			}
			tasks = append(tasks, chinitport.FleetTask{
				TaskID:           row.TaskID,
				Type:             chinitport.FleetTaskType(row.Type),
				Target:           row.Target,
				Phase:            chinitport.FleetTaskPhase(row.Phase),
				Epoch:            row.Epoch,
				HeartbeatAt:      row.LeaseUntil - int64(row.HeartbeatSeconds),
				HeartbeatSeconds: row.HeartbeatSeconds,
				Retries:          row.Retries,
				MaxRetries:       row.MaxRetries,
				Payload:          row.Payload,
				UpdatedAt:        row.UpdatedAt,
			})
		}
		if out.LastEvaluatedKey == nil {
			break
		}
		start = out.LastEvaluatedKey
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].UpdatedAt < tasks[j].UpdatedAt })
	return tasks, nil
}

// Compile-time capability check.
var _ chinitport.FleetTaskStore = (*DynamoDBFleetTaskStore)(nil)
