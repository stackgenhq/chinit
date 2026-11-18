// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// RolloutProgress is the coordinator's answer for one node's place in
// the rollout (the tick's decision input and the CLI's report).
type RolloutProgress struct {
	// Hostname is the node's cluster identity.
	Hostname string
	// OnTarget reports whether the node's status row shows the target
	// version (a replacement that booted the new AMI).
	OnTarget bool
	// Version is the node's reported ClickHouse version.
	Version string
	// Cordoned excludes the node from this rollout's dispatch (the
	// operator owns it mid-maintenance).
	Cordoned bool
	// HasTask reports an outstanding rollout task row for the node.
	HasTask bool
}

// RolloutState is the fleet's rollout position over the status rows.
type RolloutState struct {
	// Nodes is the per-node progress (sorted by hostname).
	Nodes []RolloutProgress
	// Complete is true when every non-cordoned node reports the target.
	Complete bool
}

// RolloutInput is the coordinator tick's fleet view.
type RolloutInput struct {
	// TargetVersion is the version a replacement must report (the
	// new AMI's ClickHouse; the version gate's authority).
	TargetVersion string
	// Statuses is the fleet's status rows.
	Statuses []port.NodeStatus
	// Tasks is the fleet's outstanding task rows (the rollout's own
	// dispatch residue; the coordinator never double-dispatches).
	Tasks []port.FleetTask
	// Role filters the nodes this rollout governs ("writer" or
	// "reader"); other roles' rows are ignored.
	Role string
}

// EvaluateRollout derives the rollout state and the next node to
// dispatch. Pure: the caller (the coordinator CLI) submits the task
// row; this function only decides.
//
// The sequencing rules (the operator's rollout parity):
//   - one outstanding rollout task at a time fleet-wide (the previous
//     node's verdict gates the next dispatch);
//   - cordoned nodes are skipped (the operator owns them) but do not
//     block the rest;
//   - a node already on the target version needs no dispatch;
//   - the lowest-ordinal off-target node is next (deterministic order).
func EvaluateRollout(in RolloutInput) RolloutState {
	state := RolloutState{}
	byHost := map[string]port.FleetTask{}
	for _, t := range in.Tasks {
		if t.Type == port.TaskRollout && !port.FleetTaskTerminal(t) {
			byHost[t.Target] = t
		}
	}
	for _, row := range in.Statuses {
		if in.Role != "" && row.Role != in.Role {
			continue
		}
		onTarget := row.ClickHouseVersion == in.TargetVersion && in.TargetVersion != ""
		progress := RolloutProgress{
			Hostname: row.Hostname,
			OnTarget: onTarget,
			Version:  row.ClickHouseVersion,
			Cordoned: row.Cordoned,
			HasTask:  false,
		}
		if _, has := byHost[row.Hostname]; has {
			progress.HasTask = true
		}
		state.Nodes = append(state.Nodes, progress)
	}
	sort.Slice(state.Nodes, func(i, j int) bool { return state.Nodes[i].Hostname < state.Nodes[j].Hostname })
	state.Complete = true
	for _, n := range state.Nodes {
		if n.Cordoned {
			continue
		}
		if !n.OnTarget {
			state.Complete = false
		}
	}
	return state
}

// NextRolloutDispatch answers the node the coordinator should submit a
// rollout task for, or "" when none: the rollout is complete, another
// task is outstanding, or every off-target node is cordoned.
func NextRolloutDispatch(state RolloutState) string {
	outstanding := false
	var next string
	for _, n := range state.Nodes {
		if n.HasTask {
			outstanding = true
			break
		}
	}
	if outstanding {
		return ""
	}
	for _, n := range state.Nodes {
		if n.Cordoned || n.OnTarget {
			continue
		}
		if next == "" || rolloutOrdinalLess(n.Hostname, next) {
			next = n.Hostname
		}
	}
	return next
}

// rolloutOrdinalLess orders writer-N/reader-N hostnames by their
// numeric ordinal (writer-2 < writer-10; the lexical order would roll
// writer-10 before writer-2).
func rolloutOrdinalLess(a, b string) bool {
	pa, oa := splitOrdinal(a)
	pb, ob := splitOrdinal(b)
	if pa != pb {
		return a < b
	}
	return oa < ob
}

// splitOrdinal splits "writer-03" into ("writer-", 3). Non-ordinal
// names return the whole string and -1 (sorted lexically).
func splitOrdinal(host string) (string, int) {
	i := strings.LastIndex(host, "-")
	if i < 0 || i == len(host)-1 {
		return host, -1
	}
	n := 0
	valid := true
	for _, c := range host[i+1:] {
		if c < '0' || c > '9' {
			valid = false
			break
		}
		n = n*10 + int(c-'0')
	}
	if !valid {
		return host, -1
	}
	return host[:i+1], n
}

// RolloutGate is the coordinator's verdict over the fleet view (the
// budget + version gates in one check the CLI logs).
func RolloutGate(budget BudgetVerdict, state RolloutState) error {
	if !budget.Allowed {
		return fmt.Errorf("rollout gated by disruption budget: %s", budget.Reason)
	}
	if state.Complete {
		return fmt.Errorf("rollout already complete: every non-cordoned node reports %s", "the target version")
	}
	return nil
}

// KeeperRolloutGate is the keeper-mode ensemble safety check — the
// operator's raft-safety parity: a keeper replacement during a
// degraded ensemble can lose quorum, so the gate refuses unless the
// ensemble is healthy AND the replacement's absence still leaves the
// quorum. Pure over the mntr facts.
//
// The rules (the highest-blast-radius item in the plan; no exceptions
// without the force flag, which the CLI owns):
//   - the reporting keeper must see a live ensemble (its own mntr
//     answered);
//   - the leader must have synced followers ≥ VotingMembers/2 (the
//     quorum: removing one voting member from a HEALTHY ensemble of
//     N leaves (N-1) voting members whose majority floor is
//     (N-1)/2+1 — the gate checks the ensemble CAN lose one: N ≥ 3
//     and the leader's sync is current);
//   - a 1- or 2-keeper ensemble NEVER rolls (the quorum cannot survive
//     the replacement's absence; a 2-keeper roll is a standing
//     outage).
func KeeperRolloutGate(votingMembers, syncedFollowers int, leaderPresent bool) error {
	if !leaderPresent {
		return fmt.Errorf("keeper rollout gated: no leader reported (the ensemble is degraded — replacing a keeper now can lose quorum)")
	}
	if votingMembers < 3 {
		return fmt.Errorf("keeper rollout gated: a %d-voting-member ensemble cannot survive a replacement's absence (3 is the minimum; a standing quorum must hold while the node is down)", votingMembers)
	}
	quorum := votingMembers/2 + 1
	// The leader's own sync set (itself + synced followers) must be at
	// the full quorum NOW, and the post-removal quorum (votingMembers-1
	// members, floor (votingMembers-1)/2+1) must be ≤ the remaining
	// healthy set.
	healthy := syncedFollowers + 1
	if healthy < quorum {
		return fmt.Errorf("keeper rollout gated: healthy voting members %d < quorum %d (the ensemble is degraded)", healthy, quorum)
	}
	postRemovalMembers := votingMembers - 1
	postQuorum := postRemovalMembers/2 + 1
	if healthy-1 < postQuorum {
		return fmt.Errorf("keeper rollout gated: removing one keeper leaves healthy %d < the post-rollout quorum %d", healthy-1, postQuorum)
	}
	return nil
}
