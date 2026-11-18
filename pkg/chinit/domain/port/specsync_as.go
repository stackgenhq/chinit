// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

// The config-drift convergence capability probes (the provider's
// optional-capability seam — the AsMetricPusher/AsVolumeGrower shape).
// The spec-sync job degrades loudly (log + skip) when a provider lacks
// either half; the fleet keeps serving on its last-known-good config.

// AsRoleSpecSource returns the role-spec read capability, or nil.
func AsRoleSpecSource(p Provider) RoleSpecSource {
	if c, ok := p.(RoleSpecSource); ok {
		return c
	}
	return nil
}

// AsRefreshLock returns the refresh-serialization capability, or nil.
func AsRefreshLock(p Provider) RefreshLock {
	if c, ok := p.(RefreshLock); ok {
		return c
	}
	return nil
}
