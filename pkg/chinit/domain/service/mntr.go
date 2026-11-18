// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// KeeperMntrService fetches and formats a keeper's mntr payload — the
// tested replacement for hand-rolled `echo mntr | nc host 9181 | awk`
// parsing in node bootstrap and health scripts. It deliberately carries
// no quorum decision logic: `keeper safe-to-stop` does that; this command
// only reports what the ensemble member says.
type KeeperMntrService struct {
	// probe reads live ensemble state.
	probe port.KeeperProbe
}

// NewKeeperMntrService builds the service over a probe.
func NewKeeperMntrService(probe port.KeeperProbe) *KeeperMntrService {
	return &KeeperMntrService{probe: probe}
}

// MntrRequest is the input to KeeperMntrService.Mntr.
type MntrRequest struct {
	// KeeperAddress is the mntr endpoint (host:port).
	KeeperAddress string
}

// MntrResult is the formatted output of KeeperMntrService.Mntr.
type MntrResult struct {
	// Fields is the verbatim mntr key=value payload.
	Fields map[string]string
	// ServerState is zk_server_state ("leader"/"follower"/...).
	ServerState string
	// SyncedFollowers is zk_synced_followers (the leader's sync set
	// minus itself — the quorum gate's healthy input).
	SyncedFollowers int
	// VotingMembers is the configured voting membership size (the
	// probe's derivation; 0 when the payload omits it).
	VotingMembers int
}

// Mntr queries the mntr endpoint and returns the full parsed payload.
//
// Why an error on empty: a keeper that answers with nothing is not
// answering mntr at all, and health scripts must read that as "keeper
// down" (exit 3), not as a healthy empty snapshot.
func (s *KeeperMntrService) Mntr(ctx context.Context, request MntrRequest) (MntrResult, error) {
	if request.KeeperAddress == "" {
		return MntrResult{}, fmt.Errorf("keeper address is empty")
	}
	mntr, err := s.probe.Mntr(ctx, port.MntrRequest{Address: request.KeeperAddress})
	if err != nil {
		return MntrResult{}, fmt.Errorf("mntr: %w", err)
	}
	if len(mntr.Fields) == 0 {
		return MntrResult{}, fmt.Errorf("mntr %s: empty response", request.KeeperAddress)
	}
	return MntrResult{
		Fields:          mntr.Fields,
		ServerState:     mntr.ServerState,
		SyncedFollowers: mntr.SyncedFollowers,
		VotingMembers:   mntr.VotingMembers,
	}, nil
}

// FormatLines renders the payload as `key=value` lines sorted by key
// deterministic output for scripts that grep a single field, exactly
// like the awk parsing it replaces.
func (r MntrResult) FormatLines() string {
	lines := make([]string, 0, len(r.Fields))
	for _, key := range r.sortedKeys() {
		lines = append(lines, key+"="+r.Fields[key])
	}
	return strings.Join(lines, "\n")
}

// FormatJSON renders the payload as one JSON object. encoding/json
// sorts map keys, so the output is deterministic without extra work.
func (r MntrResult) FormatJSON() (string, error) {
	data, err := json.Marshal(r.Fields)
	if err != nil {
		return "", fmt.Errorf("marshal mntr fields: %w", err)
	}
	return string(data), nil
}

// sortedKeys returns the field names in ascending order.
func (r MntrResult) sortedKeys() []string {
	keys := make([]string, 0, len(r.Fields))
	for key := range r.Fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
