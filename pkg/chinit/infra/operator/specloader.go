// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"encoding/json"
	"sync"

	chk "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	v1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/chop"
	"github.com/altinity/clickhouse-operator/pkg/model/chi/normalizer"
	chkNormalizer "github.com/altinity/clickhouse-operator/pkg/model/chk/normalizer"
	commonNormalizer "github.com/altinity/clickhouse-operator/pkg/model/common/normalizer"
	"github.com/altinity/clickhouse-operator/pkg/model/common/normalizer/subst"
	core "k8s.io/api/core/v1"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

var chopOnce sync.Once

// bootstrapChop initializes the global chop singleton with compiled-in
// defaults, exactly as the operator's own off-kube unit tests do
// (chop.New(nil, nil, "")).
//
// Why: the normalizer and config generators read global operator
// configuration via chop.Config; without a non-nil singleton every
// normalization/render panics. In-repo callers may have bootstrapped it
// already, so this is idempotent.
//
// The operator's extra-config-file scan paths are then pinned to a
// non-existent directory. The default is "config.d" RELATIVE TO THE
// PROCESS CWD (normalizeSectionClickHouseConfigurationFile builds
// RelativeToBasePath(ConfigFolderPath, "config.d") — and with no config
// file the folder path is empty, so the path stays the bare relative
// name). On a node the cwd is / and the scan finds nothing — but a
// render whose -out IS ./config.d (the validate harness, a developer
// shell) would feed its own previous output back into the next render
// as operator extra files: nondeterministic output that grows per run
// and drift-check false positives. Pinning to a path that cannot exist
// makes the scan a no-op everywhere, preserving on-cluster behavior
// (there chCommonConfigsPath is set explicitly in the operator's
// config file).
func bootstrapChop() {
	chopOnce.Do(func() {
		chop.New(nil, nil, "")
		cfg := chop.Config()
		// The scan paths are pinned and the ALREADY-scanned Runtime
		// maps cleared: chop.New → Init → Postprocess reads them above
		// (see the function comment), so pinning paths alone would
		// leave this process's maps polluted.
		const inert = "/nonexistent-chctl-operator-extra-configs"
		cfg.ClickHouse.Config.File.Path.Common = inert
		cfg.ClickHouse.Config.File.Path.Host = inert
		cfg.ClickHouse.Config.File.Path.User = inert
		cfg.Keeper.Config.File.Path.Common = inert
		cfg.Keeper.Config.File.Path.Host = inert
		cfg.Keeper.Config.File.Path.User = inert
		cfg.ClickHouse.Config.File.Runtime = v1.OperatorConfigFileRuntime{}
		cfg.Keeper.Config.File.Runtime = v1.OperatorConfigFileRuntime{}
	})
}

// SpecLoader implements port.SpecLoader over the operator's normalizer.
type SpecLoader struct{}

// NewSpecLoader builds the operator-backed spec loader.
func NewSpecLoader() *SpecLoader {
	return &SpecLoader{}
}

// secretGetter adapts a port.SecretSource onto the operator's
// normalizer seam (subst.SecretGetter): the operator resolves
// TLS rootCASecretRef (and 0.28.0's backup-Secret credentials) AT
// NORMALIZE TIME through this callback, fetching a whole Secret by
// namespace/name and picking the field out of its Data afterward.
//
// Off-cluster there is no Kubernetes API server; chinit's cloud secret
// store (AWS Secrets Manager / Azure Key Vault via the provider) is the
// equivalent. The namespace is a Kubernetes concept the cloud stores do
// not share (a single account/region per fleet), so it is accepted and
// dropped. The cloud store's whole value is fetched (Field empty) and
// mapped onto the Kubernetes Secret shape the operator's key lookup
// expects:
//   - a JSON object of strings becomes one Data key per field — the
//     same field-selection semantics chinit's own GetSecretValue
//     applies to structured secrets;
//   - any other value is exposed under the secret's own name as the one
//     data key (chinit's "no field = the whole value" convention).
//
// Without this adapter (nil secretGet): a spec that declares
// rootCASecretRef makes FetchSecretFieldValue call the nil func and
// PANICS the loader (not a rejection — a crash); the operator's
// hard-fail status paths assume the callback exists.
func secretGetter(ctx context.Context, secrets port.SecretSource) subst.SecretGetter {
	if secrets == nil {
		return nil
	}
	return func(namespace, name string) (*core.Secret, error) {
		_ = namespace // cloud secret stores are not namespaced
		value, err := secrets.GetSecretValue(ctx, port.SecretRequest{Name: name})
		if err != nil {
			return nil, err
		}
		data := map[string][]byte{}
		var fields map[string]string
		if json.Unmarshal([]byte(value), &fields) == nil && len(fields) > 0 {
			for field, fieldValue := range fields {
				data[field] = []byte(fieldValue)
			}
		} else {
			data[name] = []byte(value)
		}
		return &core.Secret{Data: data}, nil
	}
}

// LoadCHI parses, defaults and runtime-wires a CHI spec document.
//
// Why: rendering configs and running schemer operations require a
// *normalized* CR — the raw parsed spec has nil runtime addresses and
// panics inside the config generator's host-naming lookups.
//
// Without it: every consumer would re-derive the normalizer bootstrap
// order and drift from the operator's on-cluster behavior.
func (l *SpecLoader) LoadCHI(ctx context.Context, request port.LoadCHIRequest) (any, error) {
	bootstrapChop()

	raw := &v1.ClickHouseInstallation{}
	if err := parseSpec(request.Spec.Data, raw); err != nil {
		return nil, err
	}
	if request.Spec.Name != "" {
		raw.Name = request.Spec.Name
	}
	if request.Spec.Namespace != "" {
		raw.Namespace = request.Spec.Namespace
	}

	// Off-cluster the secret getter is the cloud secret store when the
	// caller wired one (the provider's Secrets Manager / Key Vault
	// source). Options must be non-nil: the normalizer dereferences
	// Options in ensureSubject, matching the controller's
	// NewOptions[...] wiring. Without a source, a spec relying on
	// secret refs is rejected by the normalizer's status path instead
	// of resolved.
	options := commonNormalizer.NewOptions[v1.ClickHouseInstallation]()

	n := normalizer.New(secretGetter(ctx, request.Secrets))
	cr, err := n.CreateTemplated(raw, options)
	if err != nil {
		return nil, err
	}
	// Hard rejects (removed user-setting syntax, FIPS violations) are
	// reported via status, not error — the controller checks status the
	// same way. Surface them as load errors.
	if status := cr.EnsureStatus(); status.GetStatus() == v1.StatusAborted {
		return nil, &SpecRejectedError{Errors: status.GetErrors()}
	}
	return cr, nil
}

// LoadCHK parses, defaults and runtime-wires a CHK spec document.
func (l *SpecLoader) LoadCHK(ctx context.Context, request port.LoadCHKRequest) (any, error) {
	bootstrapChop()

	raw := &chk.ClickHouseKeeperInstallation{}
	if err := parseSpec(request.Spec.Data, raw); err != nil {
		return nil, err
	}
	if request.Spec.Name != "" {
		raw.Name = request.Spec.Name
	}
	if request.Spec.Namespace != "" {
		raw.Namespace = request.Spec.Namespace
	}

	n := chkNormalizer.New()
	options := commonNormalizer.NewOptions[chk.ClickHouseKeeperInstallation]()
	cr, err := n.CreateTemplated(raw, options)
	if err != nil {
		return nil, err
	}
	if status := cr.EnsureStatus(); status.GetStatus() == v1.StatusAborted {
		return nil, &SpecRejectedError{Errors: status.GetErrors()}
	}
	return cr, nil
}

// SpecRejectedError marks a spec the normalizer refused (e.g. the
// RemovedSecretRefSyntax hardening).
type SpecRejectedError struct {
	// Errors are the normalizer's status errors.
	Errors []string
}

// Error implements error.
func (e *SpecRejectedError) Error() string {
	return "spec rejected: " + joinErrors(e.Errors)
}
