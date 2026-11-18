// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aws_test

import (
	"context"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/aws"
)

// taskStoreFakeDynamoDB is the fleet-task store's in-memory fake: it
// implements the conditional Put/Update/Get/Scan surface with the
// SAME semantics the real table offers — a condition that does not
// match is a ConditionalCheckFailed, the benign refusal.
type taskStoreFakeDynamoDB struct {
	items map[string]map[string]dynamodbtypes.AttributeValue
}

func newTaskStoreFake() *taskStoreFakeDynamoDB {
	return &taskStoreFakeDynamoDB{items: map[string]map[string]dynamodbtypes.AttributeValue{}}
}

func (f *taskStoreFakeDynamoDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	hostname := in.Item["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	if row, live := f.items[hostname]; live {
		// The submit's condition: a live row refuses unless terminal.
		phase := row["phase"].(*dynamodbtypes.AttributeValueMemberS).Value
		if phase == string(chinitport.TaskDone) || phase == string(chinitport.TaskFailed) {
			f.items[hostname] = in.Item
			return &dynamodb.PutItemOutput{}, nil
		}
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	f.items[hostname] = in.Item
	return &dynamodb.PutItemOutput{}, nil
}

func (f *taskStoreFakeDynamoDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	hostname := in.Key["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	if row, ok := f.items[hostname]; ok {
		return &dynamodb.GetItemOutput{Item: row}, nil
	}
	return &dynamodb.GetItemOutput{}, nil
}

// evalTaskCondition applies the store's small condition vocabulary:
// "phase = :pending AND target = :target" (claim), "phase = :exec AND
// target = :me AND epoch = :epoch" (heartbeat), "phase IN (:exec,
// :ack) AND target = :me AND epoch = :epoch" (verdict), and the retry
// condition. The fakes answer exactly what the row says.
func (f *taskStoreFakeDynamoDB) evalTaskCondition(row map[string]dynamodbtypes.AttributeValue, cond string, vals map[string]dynamodbtypes.AttributeValue) bool {
	str := func(name string) string {
		v, ok := vals[name]
		if !ok {
			return ""
		}
		if s, ok := v.(*dynamodbtypes.AttributeValueMemberS); ok {
			return s.Value
		}
		return ""
	}
	rowStr := func(name string) string {
		v, ok := row[name]
		if !ok {
			return ""
		}
		if s, ok := v.(*dynamodbtypes.AttributeValueMemberS); ok {
			return s.Value
		}
		return ""
	}
	rowN := func(name string) int64 {
		v, ok := row[name]
		if !ok {
			return 0
		}
		if n, ok := v.(*dynamodbtypes.AttributeValueMemberN); ok {
			i, _ := strconv.ParseInt(n.Value, 10, 64)
			return i
		}
		return 0
	}
	switch cond {
	case "phase = :pending AND target = :target":
		return rowStr("phase") == string(chinitport.TaskPending) &&
			rowStr("target") == str(":target")
	case "phase = :exec AND target = :me AND epoch = :epoch":
		return rowStr("phase") == string(chinitport.TaskExecuting) &&
			rowStr("target") == str(":me") &&
			rowN("epoch") == attrN(vals, ":epoch")
	case "phase IN (:exec, :ack) AND target = :me AND epoch = :epoch":
		return (rowStr("phase") == string(chinitport.TaskExecuting) || rowStr("phase") == string(chinitport.TaskAck)) &&
			rowStr("target") == str(":me") &&
			rowN("epoch") == attrN(vals, ":epoch")
	case "phase IN (:exec, :ack, :failed, :pending) AND epoch = :epoch":
		p := rowStr("phase")
		return (p == string(chinitport.TaskExecuting) || p == string(chinitport.TaskAck) ||
			p == string(chinitport.TaskFailed) || p == string(chinitport.TaskPending)) &&
			rowN("epoch") == attrN(vals, ":epoch")
	}
	return false
}

func attrN(vals map[string]dynamodbtypes.AttributeValue, name string) int64 {
	v, ok := vals[name]
	if !ok {
		return 0
	}
	if n, ok := v.(*dynamodbtypes.AttributeValueMemberN); ok {
		i, _ := strconv.ParseInt(n.Value, 10, 64)
		return i
	}
	return 0
}

func (f *taskStoreFakeDynamoDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	hostname := in.Key["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
	row, ok := f.items[hostname]
	if !ok {
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	if !f.evalTaskCondition(row, *in.ConditionExpression, in.ExpressionAttributeValues) {
		return nil, &dynamodbtypes.ConditionalCheckFailedException{}
	}
	// Apply the SET clauses the store uses: each clause's value is
	// written with its OWN attribute type (the N types must survive —
	// the next getTask unmarshal reads them back as numbers).
	sets := map[string]dynamodbtypes.AttributeValue{}
	for k, v := range in.ExpressionAttributeValues {
		sets[k] = v
	}
	for _, clause := range splitSetClauses(*in.UpdateExpression) {
		name, val := splitSetClause(clause)
		if v, ok := sets[val]; ok {
			row[name] = v
		}
	}
	// The retry's "epoch = epoch + :one" arithmetic the generic setter
	// cannot express: apply it explicitly.
	if *in.UpdateExpression == "SET phase = :pending, epoch = epoch + :one, retries = retries + :one, instance_id = :clear, lease_until = :lease, updated_at = :now" {
		epoch := attrN(row, "epoch") + attrN(in.ExpressionAttributeValues, ":one")
		retries := attrN(row, "retries") + attrN(in.ExpressionAttributeValues, ":one")
		row["epoch"] = &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(epoch, 10)}
		row["retries"] = &dynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(retries, 10)}
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (f *taskStoreFakeDynamoDB) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	out := &dynamodb.ScanOutput{Items: []map[string]dynamodbtypes.AttributeValue{}}
	for _, row := range f.items {
		hostname := row["hostname"].(*dynamodbtypes.AttributeValueMemberS).Value
		if hostname[:5] == "task/" {
			out.Items = append(out.Items, row)
		}
	}
	return out, nil
}

// splitSetClauses pulls the "a = :x" fragments out of a SET expression.
func splitSetClauses(expr string) []string {
	const prefix = "SET "
	if len(expr) <= len(prefix) {
		return nil
	}
	var clauses []string
	depth := 0
	start := len(prefix)
	for i := len(prefix); i < len(expr); i++ {
		switch expr[i] {
		case ',':
			if depth == 0 {
				clauses = append(clauses, expr[start:i])
				start = i + 2 // skip ", "
			}
		}
	}
	return append(clauses, expr[start:])
}

// splitSetClause splits "name = :value" on the assignment.
func splitSetClause(clause string) (string, string) {
	for i := 0; i < len(clause); i++ {
		if clause[i] == '=' {
			name := clause[:i]
			val := clause[i+1:]
			for len(name) > 0 && name[len(name)-1] == ' ' {
				name = name[:len(name)-1]
			}
			for len(val) > 0 && val[0] == ' ' {
				val = val[1:]
			}
			return name, val
		}
	}
	return clause, ""
}

var _ = Describe("DynamoDBFleetTaskStore", func() {
	var (
		store *aws.DynamoDBFleetTaskStore
		fake  *taskStoreFakeDynamoDB
		ctx   context.Context
	)
	const table = "node-claims"

	BeforeEach(func() {
		fake = newTaskStoreFake()
		store = aws.NewDynamoDBFleetTaskStore("us-east-1").WithClient(fake)
		ctx = context.Background()
	})

	It("submits a pending row keyed out of the claim keyspace", func() {
		ok, err := store.SubmitTask(ctx, chinitport.FleetTaskSubmit{
			Table: table, TaskID: "refresh-writer-0", Type: chinitport.TaskRefresh, Target: "writer-0",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(fake.items).To(HaveKey("task/refresh-writer-0"), "the task/ prefix keeps task rows out of the claim keyspace")
	})

	It("refuses a live duplicate and replaces a terminal row", func() {
		submit := chinitport.FleetTaskSubmit{
			Table: table, TaskID: "refresh-writer-0", Type: chinitport.TaskRefresh, Target: "writer-0",
		}
		_, err := store.SubmitTask(ctx, submit)
		Expect(err).NotTo(HaveOccurred())
		ok, err := store.SubmitTask(ctx, submit)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "the live row is the already-queued refusal")

		// Claim + complete, then resubmit over the terminal row.
		claimed, err := store.ClaimTask(ctx, chinitport.FleetTaskClaim{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(claimed).To(BeTrue())
		done, err := store.CompleteTask(ctx, chinitport.FleetTaskVerdict{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1, Reason: "complete",
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		ok, err = store.SubmitTask(ctx, submit)
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue(), "a done row is replaced with a fresh epoch")
	})

	It("fences a stale epoch's heartbeat and verdict", func() {
		submit := chinitport.FleetTaskSubmit{
			Table: table, TaskID: "refresh-writer-0", Type: chinitport.TaskRefresh, Target: "writer-0",
		}
		_, err := store.SubmitTask(ctx, submit)
		Expect(err).NotTo(HaveOccurred())
		_, err = store.ClaimTask(ctx, chinitport.FleetTaskClaim{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		// The sweep re-dispatches: epoch 2.
		retried, err := store.RetryTask(ctx, chinitport.FleetTaskVerdict{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(retried).To(BeTrue())
		// The stale executor's writes are refused, never errors.
		ok, err := store.HeartbeatTask(ctx, chinitport.FleetTaskHeartbeat{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "the stale epoch's heartbeat is fenced")
		ok, err = store.CompleteTask(ctx, chinitport.FleetTaskVerdict{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-0", Epoch: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse(), "the stale epoch's verdict is fenced")
	})

	It("refuses a foreign executor's claim", func() {
		_, err := store.SubmitTask(ctx, chinitport.FleetTaskSubmit{
			Table: table, TaskID: "refresh-writer-0", Type: chinitport.TaskRefresh, Target: "writer-0",
		})
		Expect(err).NotTo(HaveOccurred())
		ok, err := store.ClaimTask(ctx, chinitport.FleetTaskClaim{
			Table: table, TaskID: "refresh-writer-0", Executor: "writer-1", Epoch: 1,
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeFalse())
	})

	It("lists the fleet's task rows with the target filter", func() {
		_, err := store.SubmitTask(ctx, chinitport.FleetTaskSubmit{
			Table: table, TaskID: "refresh-writer-0", Type: chinitport.TaskRefresh, Target: "writer-0",
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = store.SubmitTask(ctx, chinitport.FleetTaskSubmit{
			Table: table, TaskID: "refresh-reader-1", Type: chinitport.TaskRefresh, Target: "reader-1",
		})
		Expect(err).NotTo(HaveOccurred())
		all, err := store.ListFleetTasks(ctx, chinitport.FleetTaskList{Table: table})
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(2))
		mine, err := store.ListFleetTasks(ctx, chinitport.FleetTaskList{Table: table, Target: "reader-1"})
		Expect(err).NotTo(HaveOccurred())
		Expect(mine).To(HaveLen(1))
		Expect(mine[0].Target).To(Equal("reader-1"))
	})

	It("guards the empty table and missing fields", func() {
		_, err := store.SubmitTask(ctx, chinitport.FleetTaskSubmit{TaskID: "x", Target: "writer-0"})
		Expect(err).To(HaveOccurred(), "an empty table is a spec-render bug, never a silent no-op")
		_, err = store.SubmitTask(ctx, chinitport.FleetTaskSubmit{Table: table, Target: "writer-0"})
		Expect(err).To(HaveOccurred())
	})
})

// silence the attributevalue import when the marshal path changes.
var _ = attributevalue.MarshalMap
