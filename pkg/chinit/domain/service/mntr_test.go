// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"

	"github.com/stretchr/testify/require"
)

// mntrFields is a realistic leader payload, intentionally not
// alphabetical: the formatter must sort, not echo wire order.
func mntrFields() map[string]string {
	return map[string]string{
		"zk_server_state":         "leader",
		"zk_version":              "2.8.5",
		"zk_synced_followers":     "4",
		"zk_voting_followers":     "5",
		"zk_outstanding_requests": "0",
		"zk_znode_count":          "42",
	}
}

// probeResultWithFields builds the typed probe result the fake returns.
func probeResultWithFields(fields map[string]string) port.MntrResult {
	return port.MntrResult{
		ServerState:     fields["zk_server_state"],
		SyncedFollowers: 4,
		VotingMembers:   5,
		Fields:          fields,
	}
}

func TestKeeperMntrServiceReturnsFullPayload(t *testing.T) {
	svc := NewKeeperMntrService(&fakeProbe{result: probeResultWithFields(mntrFields())})

	result, err := svc.Mntr(context.Background(), MntrRequest{KeeperAddress: "keeper:9181"})

	require.NoError(t, err)
	require.Equal(t, mntrFields(), result.Fields)
}

func TestMntrResultFormatLinesSorted(t *testing.T) {
	result := MntrResult{Fields: map[string]string{
		"zk_znode_count":          "42",
		"zk_server_state":         "leader",
		"zk_outstanding_requests": "0",
	}}

	require.Equal(t,
		"zk_outstanding_requests=0\nzk_server_state=leader\nzk_znode_count=42",
		result.FormatLines())
}

func TestMntrResultFormatJSONShape(t *testing.T) {
	result := MntrResult{Fields: map[string]string{
		"zk_server_state":     "leader",
		"zk_synced_followers": "4",
	}}

	out, err := result.FormatJSON()

	require.NoError(t, err)
	// One object with sorted keys — parse it back so the shape check
	// does not depend on marshaling whitespace.
	var decoded map[string]string
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))
	require.Equal(t, result.Fields, decoded)
	require.Equal(t,
		`{"zk_server_state":"leader","zk_synced_followers":"4"}`,
		out)
}

func TestKeeperMntrServiceEmptyResponseIsError(t *testing.T) {
	svc := NewKeeperMntrService(&fakeProbe{})

	_, err := svc.Mntr(context.Background(), MntrRequest{KeeperAddress: "keeper:9181"})

	// The exit-3-equivalent: empty mntr must fail, not report a healthy
	// empty snapshot (the bash script's `-z "$MNTR_OUTPUT"` branch).
	require.ErrorContains(t, err, "empty response")
}

func TestKeeperMntrServicePropagatesProbeError(t *testing.T) {
	svc := NewKeeperMntrService(&fakeProbe{err: errors.New("dial keeper 192.0.2.1:9181: refused")})

	_, err := svc.Mntr(context.Background(), MntrRequest{KeeperAddress: "192.0.2.1:9181"})

	require.ErrorContains(t, err, "dial")
}

func TestKeeperMntrServiceRejectsEmptyAddress(t *testing.T) {
	svc := NewKeeperMntrService(&fakeProbe{})

	_, err := svc.Mntr(context.Background(), MntrRequest{})

	require.ErrorContains(t, err, "keeper address is empty")
}
