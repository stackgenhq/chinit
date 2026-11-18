// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"fmt"
	"testing"

	chk "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse-keeper.altinity.com/v1"
	v1 "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/stretchr/testify/require"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// The SecretGetter seam: LoadCHI accepts a port.SecretSource and bridges
// it onto the operator normalizer's secret resolution. The operator's
// normalizer resolves security.clickhouse.tls.rootCASecretRef AT LOAD TIME
// through that callback — with a nil getter a spec carrying the ref
// crashes the loader (nil func call inside FetchSecretFieldValue), so a
// source must be wired wherever one exists (boot/peers carry the
// provider's store).

// fakeSecretSource is an in-memory port.SecretSource.
type fakeSecretSource struct {
	// values maps secret name → value (JSON strings get field-picked).
	values map[string]string
	// requested records every name the source resolved.
	requested []string
}

func (f *fakeSecretSource) GetSecretValue(_ context.Context, request port.SecretRequest) (string, error) {
	f.requested = append(f.requested, request.Name)
	value, ok := f.values[request.Name]
	if !ok {
		return "", fmt.Errorf("secret %q not found", request.Name)
	}
	return value, nil
}

// rootCASpec builds a minimal CHI spec carrying a rootCASecretRef.
func rootCASpec(secretName, key string) []byte {
	return []byte(fmt.Sprintf(`
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseInstallation
metadata:
  name: tls-test
spec:
  security:
    clickhouse:
      tls:
        rootCASecretRef:
          name: %s
          key: %s
  configuration:
    clusters:
      - name: default
        layout:
          shardsCount: 1
          replicasCount: 1
`, secretName, key))
}

func TestSecretGetterResolvesRootCASecretRef(t *testing.T) {
	// A JSON secret: the adapter maps every field to a Data key, and
	// the normalizer's key lookup resolves the RootCA PEM inline —
	// onto each cluster's inherited security (the operator inlines at
	// the cluster level via InheritClusterSecurityFrom →
	// normalizeClusterSecurity, not on spec.security).
	source := &fakeSecretSource{values: map[string]string{
		"clickhouse-certs": `{"ca.crt": "-----BEGIN CERTIFICATE-----\nTESTCA\n-----END CERTIFICATE-----\n"}`,
	}}
	loader := NewSpecLoader()
	crAny, err := loader.LoadCHI(context.Background(), port.LoadCHIRequest{
		Spec: domain.SpecReference{
			Name: "tls-test",
			Data: rootCASpec("clickhouse-certs", "ca.crt"),
		},
		Secrets: source,
	})
	require.NoError(t, err, "a resolvable rootCASecretRef must normalize cleanly")
	require.Equal(t, []string{"clickhouse-certs"}, source.requested, "the source must be consulted at load time")

	cr, ok := crAny.(*v1.ClickHouseInstallation)
	require.True(t, ok, "loader returned non-CHI")
	cluster := cr.FindCluster("default")
	require.NotNil(t, cluster)
	tls := cluster.GetSecurity().GetClickHouse().GetTLS()
	require.Equal(t,
		"-----BEGIN CERTIFICATE-----\nTESTCA\n-----END CERTIFICATE-----\n",
		tls.RootCA, "the Secret's value must be inlined into the cluster's RootCA")
	require.Nil(t, tls.RootCASecretRef, "the ref is dropped after inlining (the operator's contract)")
}

func TestSecretGetterUnresolvableRefAbortsSpec(t *testing.T) {
	// The source cannot resolve the secret: the operator's hard-fail
	// path must abort the load (status reason RootCASecretUnresolved
	// surfaces as SpecRejectedError), never a silently-weak config.
	source := &fakeSecretSource{values: map[string]string{}}
	loader := NewSpecLoader()
	_, err := loader.LoadCHI(context.Background(), port.LoadCHIRequest{
		Spec: domain.SpecReference{
			Name: "tls-test",
			Data: rootCASpec("missing-secret", "ca.crt"),
		},
		Secrets: source,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "spec rejected")
}

func TestSecretGetterNilSourceKeepsSpecWithoutRefsLoading(t *testing.T) {
	// No source wired (clusterops' historical shape): a spec WITHOUT
	// secret refs still loads — nil getter is the no-resolution
	// default, and only specs that actually need resolution are
	// affected.
	loader := NewSpecLoader()
	_, err := loader.LoadCHI(context.Background(), port.LoadCHIRequest{
		Spec: domain.SpecReference{
			Name: "plain",
			Data: []byte(`
apiVersion: clickhouse.altinity.com/v1
kind: ClickHouseInstallation
metadata:
  name: plain
spec:
  configuration:
    clusters:
      - name: default
        layout:
          shardsCount: 1
          replicasCount: 1
`),
		},
	})
	require.NoError(t, err)
}

func TestSecretGetterSingleValueSecretExposesOwnNameKey(t *testing.T) {
	// A single-value (non-JSON) secret: the adapter exposes the value
	// under the secret's own name — chinit's "no field = the whole
	// value" convention. A ref whose Key equals the secret name
	// resolves.
	source := &fakeSecretSource{values: map[string]string{
		"cluster-ca": "PEM-VALUE",
	}}
	loader := NewSpecLoader()
	_, err := loader.LoadCHI(context.Background(), port.LoadCHIRequest{
		Spec: domain.SpecReference{
			Name: "tls-test",
			Data: rootCASpec("cluster-ca", "cluster-ca"),
		},
		Secrets: source,
	})
	require.NoError(t, err, "single-value secret keyed by its own name must resolve")
}

func TestLoadCHKUnaffected(t *testing.T) {
	// The keeper loader has no secret seam (CHK has no TLS rootCA
	// resolution); a plain CHK spec keeps loading through LoadCHK.
	loader := NewSpecLoader()
	crAny, err := loader.LoadCHK(context.Background(), port.LoadCHKRequest{
		Spec: domain.SpecReference{
			Name: "keeper",
			Data: []byte(`
apiVersion: clickhouse-keeper.altinity.com/v1
kind: ClickHouseKeeperInstallation
metadata:
  name: keeper
spec:
  configuration:
    clusters:
      - name: keeper
        layout:
          shardsCount: 1
          replicasCount: 1
`),
		},
	})
	require.NoError(t, err)
	_, ok := crAny.(*chk.ClickHouseKeeperInstallation)
	require.True(t, ok, "loader returned non-CHK")
}
