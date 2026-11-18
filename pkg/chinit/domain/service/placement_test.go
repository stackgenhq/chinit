// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// The compute-placement allowlist contract: cross-node queries
// authenticate against the <networks> entries the render builds from
// the spec's subnet CIDRs. Single-AZ is the pre-multi-AZ shape (one
// CIDR); the multi-AZ opt-in adds per-AZ extras — a render that drops
// them Code 516s every cross-AZ peer.
func TestNodeSubnetCIDRs(t *testing.T) {
	tests := []struct {
		name string
		spec model.BootSpec
		want []string
	}{
		{
			name: "single-AZ spec answers the one historical CIDR",
			spec: model.BootSpec{Network: model.Network{SubnetCIDR: "10.0.1.0/24"}},
			want: []string{"10.0.1.0/24"},
		},
		{
			name: "multi-AZ spec answers own subnet first then the per-AZ extras",
			spec: model.BootSpec{Network: model.Network{
				SubnetCIDR:  "10.0.1.0/24",
				SubnetCIDRs: []string{"10.0.2.0/24", "10.0.4.0/24"},
			}},
			want: []string{"10.0.1.0/24", "10.0.2.0/24", "10.0.4.0/24"},
		},
		{
			name: "an extra duplicating the own subnet deduplicates",
			spec: model.BootSpec{Network: model.Network{
				SubnetCIDR:  "10.0.1.0/24",
				SubnetCIDRs: []string{"10.0.1.0/24", "10.0.4.0/24"},
			}},
			want: []string{"10.0.1.0/24", "10.0.4.0/24"},
		},
		{
			name: "empty entries drop without reordering",
			spec: model.BootSpec{Network: model.Network{
				SubnetCIDR:  "10.0.1.0/24",
				SubnetCIDRs: []string{"", "10.0.4.0/24"},
			}},
			want: []string{"10.0.1.0/24", "10.0.4.0/24"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.spec.NodeSubnetCIDRs())
		})
	}
}

// withSubnet never loses an original allowlist entry and never
// duplicates a subnet CIDR (the admin lists carry customer CIDRs; the
// subnet entries ride after them).
func TestWithSubnetMergesAllCIDRs(t *testing.T) {
	got := withSubnet(
		[]string{"203.0.113.0/24"},
		[]string{"10.0.1.0/24", "10.0.2.0/24"},
	)
	require.Equal(t, []string{"203.0.113.0/24", "10.0.1.0/24", "10.0.2.0/24"}, got)

	// Already-present subnet CIDRs stay put (idempotent renders).
	again := withSubnet(got, []string{"10.0.1.0/24", "10.0.2.0/24"})
	require.Equal(t, got, again)
}
