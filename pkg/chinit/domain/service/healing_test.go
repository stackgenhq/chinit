// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// The healing services' BDD-style specs: ReplacementGate (the drain's
// DNS ownership gate), SelfFenceService (the split-brain retire), and
// DiskRelief (the at-ceiling partition motion). Each pins the policy
// contract; the adapters are transport-only and get their own exec
// seams exercised through the CLI specs.

// --- ReplacementGate ---------------------------------------------------

type fakeClaimLister struct {
	claims []port.Claim
	err    error
}

func (f *fakeClaimLister) ListActiveClaims(ctx context.Context, req port.ListClaimsRequest) ([]port.Claim, error) {
	return f.claims, f.err
}

func TestReplacementGateAnswersWhetherALiveReplacementHoldsTheName(t *testing.T) {
	t.Run("another instance holding the slot reports replaced", func(t *testing.T) {
		lister := &fakeClaimLister{claims: []port.Claim{
			{Hostname: "reader-2", InstanceID: "i-new"},
		}}
		gate := NewReplacementGate("claims", lister)

		who, replaced := gate.ReplacedBy(context.Background(), "reader-2", "i-old")
		require.True(t, replaced)
		require.Equal(t, "i-new", who)
	})

	t.Run("own claim is not a replacement", func(t *testing.T) {
		lister := &fakeClaimLister{claims: []port.Claim{
			{Hostname: "reader-2", InstanceID: "i-old"},
		}}
		gate := NewReplacementGate("claims", lister)

		_, replaced := gate.ReplacedBy(context.Background(), "reader-2", "i-old")
		require.False(t, replaced, "the draining node's own claim must not gate its own DNS release")
	})

	t.Run("an empty claim holder is not a replacement (test-fixture parity, not a real row)", func(t *testing.T) {
		// Live rows always carry instance_id; the empty form must not
		// false-gate the drain (the drain_test fixture shape).
		lister := &fakeClaimLister{claims: []port.Claim{
			{Hostname: "reader-2"},
		}}
		gate := NewReplacementGate("claims", lister)

		_, replaced := gate.ReplacedBy(context.Background(), "reader-2", "i-old")
		require.False(t, replaced)
	})

	t.Run("a claims scan failure proceeds ungated", func(t *testing.T) {
		lister := &fakeClaimLister{err: errors.New("throttled")}
		gate := NewReplacementGate("claims", lister)

		_, replaced := gate.ReplacedBy(context.Background(), "reader-2", "i-old")
		require.False(t, replaced, "an unreadable table must not block the drain: the exact-only delete remains the guard")
	})

	t.Run("a nil capability proceeds ungated", func(t *testing.T) {
		gate := NewReplacementGate("claims", nil)

		_, replaced := gate.ReplacedBy(context.Background(), "reader-2", "i-old")
		require.False(t, replaced)
	})
}

// --- SelfFenceService ---------------------------------------------------

type fakeQuiescer struct {
	err error
}

func (f *fakeQuiescer) Quiesce() error { return f.err }

type fakeFencer struct {
	marked  []string
	err     error
	capable bool
}

func (f *fakeFencer) SetSelfUnhealthy(ctx context.Context, instanceID string) error {
	if !f.capable {
		return errors.New("unexpected call: fencer not wired")
	}
	if f.err != nil {
		return f.err
	}
	f.marked = append(f.marked, instanceID)
	return nil
}

func TestSelfFenceRetiresTheDuplicate(t *testing.T) {
	t.Run("quiesces then marks unhealthy", func(t *testing.T) {
		quiescer := &fakeQuiescer{}
		fencer := &fakeFencer{capable: true}
		svc := NewSelfFenceService(quiescer, fencer)

		res, err := svc.Fence(context.Background(), "reader-2", "i-dup")
		require.NoError(t, err)
		require.True(t, res.MarkedUnhealthy)
		require.False(t, res.QuiesceFailed)
		require.Equal(t, []string{"i-dup"}, fencer.marked)
	})

	t.Run("a failed quiesce still marks unhealthy", func(t *testing.T) {
		// Best-effort by contract: the ASG teardown stops the server
		// more definitively than the statements can.
		quiescer := &fakeQuiescer{err: errors.New("server refused")}
		fencer := &fakeFencer{capable: true}
		svc := NewSelfFenceService(quiescer, fencer)

		res, err := svc.Fence(context.Background(), "reader-2", "i-dup")
		require.NoError(t, err, "the fence's terminal state is the unhealthy mark, not the quiesce")
		require.True(t, res.QuiesceFailed)
		require.True(t, res.MarkedUnhealthy)
	})

	t.Run("a missing fence capability answers the loud degradation", func(t *testing.T) {
		quiescer := &fakeQuiescer{}
		svc := NewSelfFenceService(quiescer, nil)

		res, err := svc.Fence(context.Background(), "reader-2", "i-dup")
		require.Error(t, err)
		var capErr *FenceCapabilityError
		require.True(t, errors.As(err, &capErr))
		require.False(t, res.MarkedUnhealthy)
	})

	t.Run("a failed unhealthy mark surfaces the API error", func(t *testing.T) {
		quiescer := &fakeQuiescer{}
		fencer := &fakeFencer{capable: true, err: errors.New("asg refused")}
		svc := NewSelfFenceService(quiescer, fencer)

		_, err := svc.Fence(context.Background(), "reader-2", "i-dup")
		require.ErrorContains(t, err, "asg refused")
	})
}

// --- DiskRelief ---------------------------------------------------------

type fakePartitionStore struct {
	candidates []port.HotPartition
	moved      []port.HotPartition
	moveErr    map[string]bool
	listErr    error
}

func (f *fakePartitionStore) HotPartitions(limit int) ([]port.HotPartition, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if len(f.candidates) > limit {
		return f.candidates[:limit], nil
	}
	return f.candidates, nil
}

func (f *fakePartitionStore) MoveToCold(p port.HotPartition) error {
	if f.moveErr[p.Partition] {
		return errors.New("move refused")
	}
	f.moved = append(f.moved, p)
	return nil
}

func TestDiskReliefMovesTheLargestHotPartitions(t *testing.T) {
	big := port.HotPartition{Database: "default", Table: "events", Partition: "2026-11-15", BytesOnDisk: 40 << 30}
	mid := port.HotPartition{Database: "default", Table: "events", Partition: "2026-11-14", BytesOnDisk: 12 << 30}
	small := port.HotPartition{Database: "default", Table: "events", Partition: "2026-11-13", BytesOnDisk: 6 << 30}

	t.Run("moves up to the pass bound, biggest first", func(t *testing.T) {
		store := &fakePartitionStore{candidates: []port.HotPartition{big, mid, small}}
		relief := NewDiskRelief(store, 2)

		res, err := relief.Relieve(context.Background())
		require.NoError(t, err)
		require.Equal(t, 2, res.Moved)
		require.Equal(t, []port.HotPartition{big, mid}, store.moved)
	})

	t.Run("one partition's failure never aborts the pass", func(t *testing.T) {
		store := &fakePartitionStore{
			candidates: []port.HotPartition{big, mid},
			moveErr:    map[string]bool{big.Partition: true},
		}
		relief := NewDiskRelief(store, 0) // default bound (3)

		res, err := relief.Relieve(context.Background())
		require.NoError(t, err, "the steady-state TTL mover owns retries; the pass continues")
		require.Equal(t, 1, res.Moved)
		require.Equal(t, 1, res.Failures)
	})

	t.Run("zero candidates answers zero (the caller decides terminal)", func(t *testing.T) {
		store := &fakePartitionStore{}
		relief := NewDiskRelief(store, 3)

		res, err := relief.Relieve(context.Background())
		require.NoError(t, err)
		require.Zero(t, res.Moved)
	})

	t.Run("a list failure surfaces the store error", func(t *testing.T) {
		store := &fakePartitionStore{listErr: errors.New("server refused")}
		relief := NewDiskRelief(store, 3)

		_, err := relief.Relieve(context.Background())
		require.ErrorContains(t, err, "server refused")
	})
}

// --- LeaseRecovery -----------------------------------------------------

type fakeRecoveryClaimer struct {
	claimed port.ClaimRequest
	ordinal int
	err     error
}

func (f *fakeRecoveryClaimer) ClaimOrdinal(ctx context.Context, req port.ClaimRequest) (int, error) {
	f.claimed = req
	return f.ordinal, f.err
}

func (f *fakeRecoveryClaimer) RenewLease(ctx context.Context, req port.RenewRequest) error {
	return nil
}
func (f *fakeRecoveryClaimer) ReleaseLease(ctx context.Context, req port.ReleaseRequest) error {
	return nil
}

func TestLeaseRecoveryVerdicts(t *testing.T) {
	baseReq := func() port.ClaimRequest {
		return port.ClaimRequest{
			Table: "claims", Prefix: "reader-", Count: 3,
			InstanceID: "i-self", ExactOrdinal: true,
		}
	}

	t.Run("a successful own-name claim is the reclaimed verdict", func(t *testing.T) {
		claimer := &fakeRecoveryClaimer{ordinal: 2}
		svc := NewLeaseRecovery(claimer, "claims")

		outcome, err := svc.Recover(context.Background(), baseReq(), 2)
		require.NoError(t, err)
		require.Equal(t, OutcomeReclaimed, outcome)
		require.True(t, claimer.claimed.ExactOrdinal, "the recovery must claim its own name only")
	})

	t.Run("a held name is the contested verdict (the caller fences)", func(t *testing.T) {
		claimer := &fakeRecoveryClaimer{err: errors.New("exact claim of reader-2 failed: held by another live instance")}
		svc := NewLeaseRecovery(claimer, "claims")

		outcome, err := svc.Recover(context.Background(), baseReq(), 2)
		require.Error(t, err)
		require.Equal(t, OutcomeContested, outcome)
	})

	t.Run("an answered different ordinal is the renumbered verdict (never converged)", func(t *testing.T) {
		// Defensive: the ExactOrdinal shape cannot answer another
		// ordinal, but the identity contract is too important to
		// trust shape alone.
		claimer := &fakeRecoveryClaimer{ordinal: 1}
		svc := NewLeaseRecovery(claimer, "claims")

		outcome, err := svc.Recover(context.Background(), baseReq(), 2)
		require.Error(t, err)
		require.Equal(t, OutcomeRenumbered, outcome)
	})
}

// --- the init-FAILED readonly heal (force-restore class) ---------------

type fakeFlagger struct {
	touched []string
}

func (f *fakeFlagger) SetForceRestoreFlag(table string) error {
	f.touched = append(f.touched, table)
	return nil
}

func TestQueueHealHealsInitFailedReadonlyWithForceRestore(t *testing.T) {
	// The 2026-11-17 backup-0 class: the attach thread refused the
	// table (TOO_MANY_UNEXPECTED_DATA_PARTS), last_queue_update sat
	// at the epoch, and plain RESTARTs re-ran the same failed attach.
	// The flag + restart pair heals (the source contract,
	// StorageReplicatedMergeTree.cpp:558).
	t.Run("touches the flag then restarts", func(t *testing.T) {
		runner := &fakeSchemaRunner{initFailed: []string{"default.hourly_log_summary"}}
		flagger := &fakeFlagger{}
		svc := NewSchemaService(&fakeSchemaFactory{runner: runner}).WithForceRestoreFlagger(flagger)

		res, err := svc.QueueHeal(context.Background(), gcTestSchemaRequest(), domain.QueueHealRequest{RunOn: "backup-0"})
		require.NoError(t, err)
		require.Equal(t, []string{"default.hourly_log_summary"}, res.InitFailedTables)
		require.Equal(t, []string{"default.hourly_log_summary"}, res.Restored)
		require.Equal(t, []string{"default.hourly_log_summary"}, flagger.touched)
		require.Len(t, runner.restarts, 1, "the flag is followed by the restart that consumes it")
	})

	t.Run("without the capability the pass reports but defers", func(t *testing.T) {
		runner := &fakeSchemaRunner{initFailed: []string{"default.hourly_log_summary"}}
		svc := NewSchemaService(&fakeSchemaFactory{runner: runner}) // no flagger

		res, err := svc.QueueHeal(context.Background(), gcTestSchemaRequest(), domain.QueueHealRequest{RunOn: "backup-0"})
		require.NoError(t, err)
		require.Equal(t, []string{"default.hourly_log_summary"}, res.SkippedHeals,
			"a guessed heal is worse than none: the SQL surface rejects znode writes")
		require.Empty(t, res.Restored)
		require.Empty(t, runner.restarts)
	})

	t.Run("evaluates init-failed before plain readonly and skips duplicate heal", func(t *testing.T) {
		runner := &fakeSchemaRunner{
			initFailed: []string{"default.events"},
			readonly:   []string{"default.events"},
		}
		flagger := &fakeFlagger{}
		svc := NewSchemaService(&fakeSchemaFactory{runner: runner}).WithForceRestoreFlagger(flagger)

		res, err := svc.QueueHeal(context.Background(), gcTestSchemaRequest(), domain.QueueHealRequest{RunOn: "writer-2"})
		require.NoError(t, err)
		require.Equal(t, []string{"default.events"}, res.Restored)
		require.Equal(t, []string{"default.events"}, flagger.touched)
		require.Len(t, runner.restarts, 1, "init-failed table must only be restarted once after setting force_restore_data")
		require.Empty(t, runner.restores, "plain readonly pass must not re-heal an init-failed table")
	})
}

// gcTestSchemaRequest is gcTestService's request without the service
// (the force-restore specs build their own service over the runner).
func gcTestSchemaRequest() SchemaRequest {
	_, req := gcTestService(&fakeSchemaRunner{})
	return req
}
