// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

type fakeLifecycleProvider struct {
	foundInstance *port.InstanceState
	findErr       error
	startedIDs    []string
	stoppedIDs    []string
	claims        []port.Claim
}

func (f *fakeLifecycleProvider) Name() string { return "aws" }
func (f *fakeLifecycleProvider) Metadata(context.Context) (port.InstanceMetadata, error) {
	return port.InstanceMetadata{Provider: "aws"}, nil
}
func (f *fakeLifecycleProvider) GetSecretValue(context.Context, port.SecretRequest) (string, error) {
	return "", nil
}
func (f *fakeLifecycleProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}
func (f *fakeLifecycleProvider) ListActiveClaims(context.Context, port.ListClaimsRequest) ([]port.Claim, error) {
	return f.claims, nil
}
func (f *fakeLifecycleProvider) FindInstanceByName(_ context.Context, _ string) (*port.InstanceState, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.foundInstance, nil
}
func (f *fakeLifecycleProvider) StartInstance(_ context.Context, instanceID string) error {
	f.startedIDs = append(f.startedIDs, instanceID)
	return nil
}
func (f *fakeLifecycleProvider) StopInstance(_ context.Context, instanceID string) error {
	f.stoppedIDs = append(f.stoppedIDs, instanceID)
	return nil
}

var _ = Describe("backup-wake and on-demand backup-daily", func() {
	var (
		dir       string
		statePath string
		specPath  string
		fakeProv  *fakeLifecycleProvider
	)

	writeState := func(role model.Role, hostname, instanceID string) {
		data, err := json.Marshal(nodeState{
			Identity: model.NodeIdentity{
				Role:        role,
				Hostname:    hostname,
				InstanceID:  instanceID,
				Region:      "us-west-2",
				ClusterName: "demo",
				DNSZone:     "demo.clickhouse.internal",
				LeaseTable:  "demo-node-claims",
			},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(os.WriteFile(statePath, data, 0644)).To(Succeed())
	}

	writeSpec := func(onDemand bool) {
		onDemandStr := "false"
		if onDemand {
			onDemandStr = "true"
		}
		spec := `role: writer
identity_mode: lease
cluster_name: demo
dns_zone_name: demo.clickhouse.internal
claimer_table_name: demo-node-claims
backup:
  bucket: demo-clickhouse-backups
  prefix: backups
  schedule: "0 2 * * *"
  on_demand: ` + onDemandStr + `
  backup_replica_hostname: "backup-0"
`
		Expect(os.WriteFile(specPath, []byte(spec), 0644)).To(Succeed())
	}

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		statePath = filepath.Join(dir, "state.json")
		specPath = filepath.Join(dir, "chinit.yaml")

		origSpec := inventorySpecPath
		inventorySpecPath = func() string { return specPath }
		DeferCleanup(func() { inventorySpecPath = origSpec })

		fakeProv = &fakeLifecycleProvider{
			foundInstance: &port.InstanceState{
				InstanceID:   "i-backup0",
				InstanceType: "t4g.xlarge",
				State:        "stopped",
			},
			claims: []port.Claim{{Hostname: "writer-0", InstanceID: "i-writer0"}},
		}
		origBuild := buildProvider
		buildProvider = func(string, string) (port.Provider, error) {
			return fakeProv, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })
	})

	It("starts a stopped backup-0 instance from the ordinal-0 writer", func() {
		writeSpec(true)
		writeState(model.RoleWriter, "writer-0", "i-writer0")

		Expect(runBackupWake([]string{"-state", statePath})).To(Succeed())
		Expect(fakeProv.startedIDs).To(Equal([]string{"i-backup0"}))
	})

	It("skips starting backup-0 on non-ordinal-0 writers", func() {
		writeSpec(true)
		writeState(model.RoleWriter, "writer-1", "i-writer1")

		Expect(runBackupWake([]string{"-state", statePath})).To(Succeed())
		Expect(fakeProv.startedIDs).To(BeEmpty())
	})

	It("skips starting backup-0 when it is already running", func() {
		writeSpec(true)
		writeState(model.RoleWriter, "writer-0", "i-writer0")
		fakeProv.foundInstance.State = "running"

		Expect(runBackupWake([]string{"-state", statePath})).To(Succeed())
		Expect(fakeProv.startedIDs).To(BeEmpty())
	})

	It("runs pre-backup sync, executes clickhouse-backup, and stops itself when on_demand is true", func() {
		writeSpec(true)
		writeState(model.RoleBackup, "backup-0", "i-backup0")

		synced := false
		origSync := preBackupSync
		preBackupSync = func(_ context.Context, _ *nodeState, _ *inventorySpec) {
			synced = true
		}
		DeferCleanup(func() { preBackupSync = origSync })

		var toolCalls [][]string
		origTool := runBackupTool
		runBackupTool = func(_ context.Context, args ...string) (string, error) {
			toolCalls = append(toolCalls, args)
			return "ok", nil
		}
		DeferCleanup(func() { runBackupTool = origTool })

		Expect(runBackupDaily([]string{"-state", statePath})).To(Succeed())
		Expect(synced).To(BeTrue())
		Expect(toolCalls).To(HaveLen(3))
		Expect(toolCalls[0][0]).To(Equal("create"))
		Expect(toolCalls[1][0]).To(Equal("upload"))
		Expect(toolCalls[2][0]).To(Equal("delete"))
		Expect(fakeProv.stoppedIDs).To(Equal([]string{"i-backup0"}))
	})
})
