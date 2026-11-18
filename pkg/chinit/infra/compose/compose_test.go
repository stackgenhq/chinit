// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

func TestMetadataFromFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	metaFile := filepath.Join(dir, "metadata.json")
	require.NoError(t, os.WriteFile(metaFile, []byte(`{
  "region": "compose-eu",
  "zone": "az-1",
  "instance_id": "writer-0-id",
  "private_ip": "172.28.0.10",
  "hostname_hint": "writer-0"
}`), 0644))

	t.Setenv("CHINIT_COMPOSE_METADATA_FILE", metaFile)
	t.Setenv("CHINIT_PRIVATE_IP", "172.28.0.11") // env overrides file

	src := NewMetadataSource("fallback")
	meta, err := src.Get(context.Background())
	require.NoError(t, err)
	require.Equal(t, "compose", meta.Provider)
	require.Equal(t, "compose-eu", meta.Region)
	require.Equal(t, "az-1", meta.Zone)
	require.Equal(t, "writer-0-id", meta.InstanceID)
	require.Equal(t, "172.28.0.11", meta.PrivateIP)
	require.Equal(t, "writer-0", meta.HostnameHint)
}

func TestSecretsRoundTripAndJSONField(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "admin-password"), []byte("super-secret\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tls-bundle"), []byte(`{"cert":"CERT_PEM","key":"KEY_PEM"}`), 0600))

	src := NewFileSecretSource().WithDir(dir)

	val, err := src.GetSecretValue(context.Background(), port.SecretRequest{Name: "admin-password"})
	require.NoError(t, err)
	require.Equal(t, "super-secret", val)

	cert, err := src.GetSecretValue(context.Background(), port.SecretRequest{Name: "tls-bundle", Field: "cert"})
	require.NoError(t, err)
	require.Equal(t, "CERT_PEM", cert)

	_, err = src.GetSecretValue(context.Background(), port.SecretRequest{Name: "missing-secret"})
	require.Error(t, err)
}

func TestBundleFetchAndSHAGate(t *testing.T) {
	srcDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(srcDir, "configs"), 0755))
	files := map[string]string{
		"configs/network.xml":       "<clickhouse><listen_host>0.0.0.0</listen_host></clickhouse>",
		"configs/ssl.xml":           "<clickhouse/>",
		"configs/storage.xml":       "<clickhouse><storage_configuration/></clickhouse>",
		"configs/bootstrap-ddl.sql": "SELECT 1;",
		"chi.yaml":                  "apiVersion: clickhouse.altinity.com/v1\nkind: ClickHouseInstallation\n",
	}
	for rel, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(srcDir, rel), []byte(content), 0644))
	}
	encoded, err := json.Marshal(files)
	require.NoError(t, err)
	sum := sha256.Sum256(encoded)
	wantSHA := hex.EncodeToString(sum[:])

	destDir := t.TempDir()
	src := NewLocalBundleSource().WithDir(srcDir)
	written, err := src.FetchBundle(context.Background(), model.ConfigBundle{
		Bucket: "local",
		Prefix: "bootstrap",
		SHA256: wantSHA,
	}, destDir)
	require.NoError(t, err)
	require.Len(t, written, 5)

	_, err = src.FetchBundle(context.Background(), model.ConfigBundle{
		Bucket: "local",
		Prefix: "bootstrap",
		SHA256: "deadbeef",
	}, t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "SHA256 mismatch")
}

func TestLeaseClaimRenewReleaseAndList(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "leases.json")
	store := NewFileLeaseStore().WithPath(leasePath)
	ctx := context.Background()

	ord0, err := store.ClaimOrdinal(ctx, port.ClaimRequest{
		Table:        "claims",
		Prefix:       "writer-",
		Count:        2,
		InstanceID:   "inst-a",
		PrivateIP:    "172.28.0.10",
		LeaseSeconds: 300,
	})
	require.NoError(t, err)
	require.Equal(t, 0, ord0)

	ord1, err := store.ClaimOrdinal(ctx, port.ClaimRequest{
		Table:        "claims",
		Prefix:       "writer-",
		Count:        2,
		InstanceID:   "inst-b",
		PrivateIP:    "172.28.0.11",
		LeaseSeconds: 300,
	})
	require.NoError(t, err)
	require.Equal(t, 1, ord1)

	claims, err := store.ListActiveClaims(ctx, port.ListClaimsRequest{Table: "claims"})
	require.NoError(t, err)
	require.Len(t, claims, 2)
	require.Equal(t, "writer-0", claims[0].Hostname)
	require.Equal(t, "writer-1", claims[1].Hostname)

	require.NoError(t, store.RenewLease(ctx, port.RenewRequest{
		Table:        "claims",
		Hostname:     "writer-0",
		InstanceID:   "inst-a",
		LeaseSeconds: 300,
	}))

	err = store.RenewLease(ctx, port.RenewRequest{
		Table:        "claims",
		Hostname:     "writer-0",
		InstanceID:   "wrong-inst",
		LeaseSeconds: 300,
	})
	require.ErrorIs(t, err, port.ErrLeaseStolen)

	require.NoError(t, store.ReleaseLease(ctx, port.ReleaseRequest{
		Table:      "claims",
		Hostname:   "writer-0",
		InstanceID: "inst-a",
	}))
	claims, err = store.ListActiveClaims(ctx, port.ListClaimsRequest{Table: "claims"})
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Equal(t, "writer-1", claims[0].Hostname)
}

func TestLeaseExpiryAllowsReclaim(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "leases.json")
	now := time.Unix(1000, 0)
	store := NewFileLeaseStore().WithPath(leasePath)
	store.now = func() time.Time { return now }

	ctx := context.Background()
	ord, err := store.ClaimOrdinal(ctx, port.ClaimRequest{
		Table:        "claims",
		Prefix:       "reader-",
		Count:        1,
		InstanceID:   "old-inst",
		PrivateIP:    "172.28.0.30",
		LeaseSeconds: 10,
	})
	require.NoError(t, err)
	require.Equal(t, 0, ord)

	now = now.Add(20 * time.Second)
	ord, err = store.ClaimOrdinal(ctx, port.ClaimRequest{
		Table:        "claims",
		Prefix:       "reader-",
		Count:        1,
		InstanceID:   "new-inst",
		PrivateIP:    "172.28.0.31",
		LeaseSeconds: 60,
	})
	require.NoError(t, err)
	require.Equal(t, 0, ord)
}

func TestFleetTaskLifecycle(t *testing.T) {
	leasePath := filepath.Join(t.TempDir(), "leases.json")
	store := NewFileLeaseStore().WithPath(leasePath)
	ctx := context.Background()

	ok, err := store.SubmitTask(ctx, port.FleetTaskSubmit{
		Table:   "claims",
		TaskID:  "refresh-writer-0-1",
		Type:    port.TaskRefresh,
		Target:  "writer-0",
		Payload: "sha256:abc",
	})
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = store.ClaimTask(ctx, port.FleetTaskClaim{
		Table:    "claims",
		TaskID:   "refresh-writer-0-1",
		Executor: "writer-0",
		Epoch:    1,
	})
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = store.CompleteTask(ctx, port.FleetTaskVerdict{
		Table:    "claims",
		TaskID:   "refresh-writer-0-1",
		Executor: "writer-0",
		Epoch:    1,
	})
	require.NoError(t, err)
	require.True(t, ok)

	tasks, err := store.ListFleetTasks(ctx, port.FleetTaskList{Table: "claims", Target: "writer-0"})
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	require.Equal(t, port.TaskDone, tasks[0].Phase)
}
