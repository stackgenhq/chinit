// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// nonAWSProvider fakes any non-AWS cloud for the gating spec. It
// composes the existing recording claimer fake (the same full-Provider
// shape fakeLeaseProvider has) and only overrides the name.
type nonAWSProvider struct {
	*recordingClaimer
}

func (nonAWSProvider) Name() string { return "azure" }

func (nonAWSProvider) Metadata(context.Context) (port.InstanceMetadata, error) {
	return port.InstanceMetadata{}, nil
}

func (nonAWSProvider) GetSecretValue(context.Context, port.SecretRequest) (string, error) {
	return "", nil
}

func (nonAWSProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}

// This suite pins configureCloudWatch's config-shape contracts without
// executing system commands: the step's observable surface is the
// rendered JSON/YAML carried in agentCfgBuilder, which is pure.
//
// The contracts (each guards a live outage class):
// - AWS-only: non-AWS providers no-op (Azure keeps its AMA path).
// - Server roles ship server+error logs; keepers ship keeper+health
// logs only — pointing the agent at files a role cannot exist
// with makes it retry forever (the role-gating contract the bash
// era had).
// - The EMF namespace and metric selectors split by role: the
// operator alert catalog thresholds on ClickHouse/Server vs
// ClickHouse/Keeper exposition names.
// - Custom identity labels live ONLY in the prometheus EMF config
// (append_dimensions with custom keys fails agent validation on
// EC2 — node-metric alarms key on the auto-appended InstanceID).
var _ = Describe("configureCloudWatch", func() {
	awsNamed := &fakeLeaseProvider{}
	ctx := context.Background()

	Context("cloud gating", func() {
		It("is a no-op on non-AWS providers", func() {
			b := &Boot{Spec: &model.BootSpec{Role: model.RoleWriter}}
			b.Provider = nonAWSProvider{&recordingClaimer{}}
			err := b.configureCloudWatch(ctx)
			Expect(err).To(Succeed())
			Expect(b.Provider.Name()).NotTo(Equal("aws"))
		})

		It("builds config on AWS providers", func() {
			b := &Boot{Spec: &model.BootSpec{Role: model.RoleWriter}}
			b.Provider = awsNamed
			cfg := b.cloudWatchConfig(model.RoleWriter, "demo")
			Expect(cfg.ChNamespace).To(Equal("ClickHouse/Server"))
			Expect(cfg.AgentJSON).To(ContainSubstring("/clickhouse/demo/server"))
			Expect(cfg.AgentJSON).To(ContainSubstring("/clickhouse/demo/error"))
			Expect(cfg.AgentJSON).NotTo(ContainSubstring("clickhouse-keeper"))
		})
	})

	Context("role gating", func() {
		It("keeper ships keeper and health logs, not server logs", func() {
			b := &Boot{Spec: &model.BootSpec{Role: model.RoleKeeper}}
			cfg := b.cloudWatchConfig(model.RoleKeeper, "demo")
			Expect(cfg.ChNamespace).To(Equal("ClickHouse/Keeper"))
			Expect(cfg.AgentJSON).To(ContainSubstring("/clickhouse/demo/keeper"))
			Expect(cfg.AgentJSON).To(ContainSubstring("clickhouse-keeper.log"))
			Expect(cfg.AgentJSON).To(ContainSubstring("keeper_health.log"))
			Expect(cfg.AgentJSON).NotTo(ContainSubstring("clickhouse-server.log"))
			Expect(cfg.MetricSelectors).To(ContainSubstring("KeeperOutstandingRequests"))
		})

		It("server roles use the server metric selectors", func() {
			b := &Boot{Spec: &model.BootSpec{Role: model.RoleReader}}
			cfg := b.cloudWatchConfig(model.RoleReader, "demo")
			Expect(cfg.MetricSelectors).To(ContainSubstring("ReadonlyReplica"))
			Expect(cfg.MetricSelectors).To(ContainSubstring("MaxPartCountForPartition"))
			Expect(cfg.AgentJSON).To(ContainSubstring("ClickHouse/Server"))
		})
	})

	Context("identity labels", func() {
		It("puts custom labels only in the EMF dimensions, not append_dimensions", func() {
			b := &Boot{Spec: &model.BootSpec{Role: model.RoleWriter}}
			cfg := b.cloudWatchConfig(model.RoleWriter, "demo")
			Expect(cfg.AgentJSON).NotTo(ContainSubstring("append_dimensions"))
			Expect(cfg.PromCfg).To(ContainSubstring("ClusterName: \"demo\""))
			Expect(cfg.PromCfg).To(ContainSubstring("NodeType: \"writer\""))
		})

		It("falls back to the spec cluster name when identity is unset", func() {
			b := &Boot{Spec: &model.BootSpec{Role: model.RoleWriter, ClusterName: "spec-cluster"}}
			cfg := b.cloudWatchConfig(model.RoleWriter, "spec-cluster")
			Expect(cfg.AgentJSON).To(ContainSubstring("spec-cluster"))
		})
	})
})
