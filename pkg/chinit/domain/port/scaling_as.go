// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package port

// The vertical-scaling capability probes (the provider's optional-
// capability seam — the same shape AsMetricPusher/AsVolumeGrower draw).
// Each answers the provider's implementation of one scaling port, or
// nil when the cloud lacks the capability; the service layer degrades
// loudly (log + no-op decision) rather than crashing, and the CLI
// reports the missing capability as the skip reason.

// AsMetricReader returns the metric-store read capability, or nil.
func AsMetricReader(p Provider) MetricReader {
	if c, ok := p.(MetricReader); ok {
		return c
	}
	return nil
}

// AsScaleSignalsReader returns the aggregated signals capability, or nil.
func AsScaleSignalsReader(p Provider) ScaleSignalsReader {
	if c, ok := p.(ScaleSignalsReader); ok {
		return c
	}
	return nil
}

// AsInstanceResizer returns the same-instance vertical-resize
// capability, or nil (the keeper coordinator's per-instance step).
func AsInstanceResizer(p Provider) InstanceResizer {
	if c, ok := p.(InstanceResizer); ok {
		return c
	}
	return nil
}

// AsFleetResizer returns the ASG launch-template roll capability, or
// nil (the writers' vertical step).
func AsFleetResizer(p Provider) FleetResizer {
	if c, ok := p.(FleetResizer); ok {
		return c
	}
	return nil
}

// AsInstanceFencer returns the self-fence capability, or nil (the
// identity-contested node's ASG replacement trigger).
func AsInstanceFencer(p Provider) InstanceFencer {
	if c, ok := p.(InstanceFencer); ok {
		return c
	}
	return nil
}

// AsInstanceLifecycle returns the on-demand instance start/stop
// capability, or nil (the backup replica's StatefulSet 0<->1 path).
func AsInstanceLifecycle(p Provider) InstanceLifecycle {
	if c, ok := p.(InstanceLifecycle); ok {
		return c
	}
	return nil
}

// AsOrphanSweeper returns the account-hygiene capability, or nil.
func AsOrphanSweeper(p Provider) OrphanSweeper {
	if c, ok := p.(OrphanSweeper); ok {
		return c
	}
	return nil
}

// AsCooldownStore returns the cooldown-record capability, or nil.
func AsCooldownStore(p Provider) CooldownStore {
	if c, ok := p.(CooldownStore); ok {
		return c
	}
	return nil
}
