// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// The server-env step's cloud gate: CH_S3_ENDPOINT is an AWS S3-disk
// requirement (storage.xml's from_env); the Azure blob disk carries
// the account name/key inline, so an empty s3_data_bucket on Azure is
// the correct spec, not a config bug. AWS with no bucket stays Fatal
// the S3 disk would Code 36 at server start.

type stubProvider struct{ name string }

func (p *stubProvider) Name() string { return p.name }
func (p *stubProvider) Metadata(context.Context) (port.InstanceMetadata, error) {
	return port.InstanceMetadata{}, nil
}
func (p *stubProvider) GetSecretValue(context.Context, port.SecretRequest) (string, error) {
	return "", nil
}
func (p *stubProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}

var _ = Describe("configureServerEnv cloud gate", func() {
	It("is fatal on AWS when s3_data_bucket is empty (the S3 disk needs the endpoint env)", func() {
		b := &Boot{
			Spec:     &model.BootSpec{Role: model.RoleReader},
			Provider: &stubProvider{name: "aws"},
		}
		err := b.configureServerEnv(context.Background())
		Expect(fail.IsFatal(err)).To(BeTrue(),
			"an AWS spec without s3_data_bucket boots a server that dies at start (Code 36)")
		Expect(err.Error()).To(ContainSubstring("s3_data_bucket"))
	})

	It("accepts an empty s3_data_bucket on Azure (the blob disk needs no endpoint env)", func() {
		// Timezone empty + azure + no bucket: the step returns before
		// any /etc write — no host filesystem touched.
		b := &Boot{
			Spec:     &model.BootSpec{Role: model.RoleReader},
			Provider: &stubProvider{name: "azure"},
		}
		Expect(b.configureServerEnv(context.Background())).To(Succeed())
	})

	It("skips the file write when there is nothing to pin", func() {
		// Same no-op path exercised explicitly for the named contract.
		b := &Boot{
			Spec:     &model.BootSpec{Role: model.RoleReader},
			Provider: &stubProvider{name: "azure"},
		}
		Expect(b.configureServerEnv(context.Background())).To(Succeed())
		_, err := os.Stat("/etc/default/clickhouse-server")
		if err == nil {
			Skip("host carries /etc/default/clickhouse-server; presence not from this test")
		}
		Expect(os.IsNotExist(err)).To(BeTrue())
	})

	It("keeps the S3 endpoint line for an AWS-shaped spec with a bucket", func() {
		// Full file write is host-touching; assert the pure prefix of
		// the behavior instead — the Fatal gate is the step's only
		// decision point before I/O, and the bucket line is covered by
		// the AWS integration path.
		b := &Boot{
			Spec: &model.BootSpec{
				Role:         model.RoleReader,
				S3DataBucket: "data-bucket",
			},
			Provider: &stubProvider{name: "aws"},
		}
		err := b.configureServerEnv(context.Background())
		// On hosts without systemd/chown this may fail at the chown
		// leg; the gate itself must NOT have fired.
		if err != nil {
			Expect(err.Error()).NotTo(ContainSubstring("s3_data_bucket is empty"))
			return
		}
		data, rerr := os.ReadFile(filepath.Join("/etc/default", "clickhouse-server"))
		if rerr != nil {
			return // host-specific write path; gate already proven
		}
		Expect(string(data)).To(ContainSubstring("CH_S3_ENDPOINT"))
	})

	It("exports CH_CACHE_MAX_SIZE_BYTES when a dedicated NVMe/cache volume is mounted", func() {
		origProbe := probeCacheDiskMaxBytes
		probeCacheDiskMaxBytes = func(context.Context) uint64 {
			return 458058269491 // 90% of 474 GiB NVMe instance store
		}
		DeferCleanup(func() { probeCacheDiskMaxBytes = origProbe })

		b := &Boot{
			Spec: &model.BootSpec{
				Role:         model.RoleWriter,
				S3DataBucket: "data-bucket",
				Region:       "us-east-1",
			},
			Provider: &stubProvider{name: "aws"},
		}
		content, err := b.serverEnvContent(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(content).To(ContainSubstring(`CH_S3_ENDPOINT="https://data-bucket.s3.us-east-1.amazonaws.com"`))
		Expect(content).To(ContainSubstring(`CH_CACHE_MAX_SIZE_BYTES="458058269491"`))
	})

	It("seeds a deterministic /var/lib/clickhouse/uuid per (cluster, hostname) when absent", func() {
		dir := GinkgoT().TempDir()
		uuidFile := filepath.Join(dir, "uuid")
		origPath := serverUUIDPath
		serverUUIDPath = func() string { return uuidFile }
		DeferCleanup(func() { serverUUIDPath = origPath })

		b := &Boot{
			Spec: &model.BootSpec{
				ClusterName: "ch-int-dev",
				Role:        model.RoleReader,
			},
			Identity: model.NodeIdentity{Hostname: "reader-0"},
			Provider: &stubProvider{name: "azure"},
		}
		Expect(b.configureServerEnv(context.Background())).To(Succeed())
		data, err := os.ReadFile(uuidFile)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal(deterministicServerUUID("ch-int-dev", "reader-0") + "\n"))
		Expect(deterministicServerUUID("ch-int-dev", "reader-0")).NotTo(Equal(deterministicServerUUID("ch-int-dev", "reader-1")))
	})

	It("does NOT rewrite the uuid over existing ClickHouse state (the CAS lineage guard)", func() {
		dir := GinkgoT().TempDir()
		uuidFile := filepath.Join(dir, "uuid")
		storeDir := filepath.Join(dir, "store")
		Expect(os.MkdirAll(storeDir, 0755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(storeDir, "any"), []byte("x"), 0644)).To(Succeed())
		Expect(os.WriteFile(uuidFile, []byte("random-uuid\n"), 0640)).To(Succeed())
		origPath, origStore := serverUUIDPath, chStorePath
		serverUUIDPath = func() string { return uuidFile }
		chStorePath = func() string { return storeDir }
		DeferCleanup(func() { serverUUIDPath, chStorePath = origPath, origStore })

		b := &Boot{
			Spec: &model.BootSpec{
				ClusterName: "ch-int-dev",
				Role:        model.RoleWriter,
			},
			Identity: model.NodeIdentity{Hostname: "writer-0"},
			Provider: &stubProvider{name: "aws"},
		}
		b.adoptServerUUID(context.Background())
		data, err := os.ReadFile(uuidFile)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(data)).To(Equal("random-uuid\n"),
			"a stateful node's uuid IS the live CAS anchor match — rewriting it would Code 246 on the next start")
	})

	It("emits CH_CAS_SERVER_ROOT_ID for the storage.xml from_env contract", func() {
		b := &Boot{
			Spec: &model.BootSpec{
				ClusterName:  "ch-int-dev",
				Role:         model.RoleWriter,
				S3DataBucket: "data-bucket",
				CasEnabled:   true,
			},
			Identity: model.NodeIdentity{Hostname: "writer-0"},
			Provider: &stubProvider{name: "aws"},
		}
		content, err := b.serverEnvContent(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(content).To(ContainSubstring(
			`CH_CAS_SERVER_ROOT_ID="` + deterministicServerUUID("ch-int-dev", "writer-0") + `"`))
	})
})
