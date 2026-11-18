// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"gopkg.in/yaml.v3"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// baselineSpec is the valid shape every table entry mutates one field
// at a time. This table is the spec-validation gate's single home: the
// service package's specs consume the validated spec (settleIdentity,
// renders), so field coverage folds here rather than duplicating the
// same Validate call across suites.
func baselineSpec() model.BootSpec {
	return model.BootSpec{
		Role:         model.RoleWriter,
		IdentityMode: model.IdentityStatic,
		ClusterName:  "demo",
		ConfigBundle: model.ConfigBundle{Bucket: "data-bucket", Prefix: "bootstrap"},
		Disks:        model.Disks{MetadataLabel: "ch-metadata"},
		Secrets:      model.Secrets{DefaultPassword: "default-pw", AdminPassword: "admin-pw"},
	}
}

var _ = Describe("BootSpec.Validate", func() {
	// The writer-ASG contract lives here: lease identity requires the
	// role's ordinal claim ceiling (the ASG max), and a reader at
	// scale-out past its baseline is valid because ReaderMax — not
	// ReaderCount — is the ceiling.
	DescribeTable("accepts and rejects spec shapes",
		func(mutate func(*model.BootSpec), wantErrField string) {
			spec := baselineSpec()
			if mutate != nil {
				mutate(&spec)
			}
			err := spec.Validate()
			if wantErrField == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(HaveOccurred())
			var specErr *model.SpecError
			Expect(errors.As(err, &specErr)).To(BeTrue())
			Expect(specErr.Field).To(Equal(wantErrField))
			Expect(err.Error()).To(ContainSubstring("chinit.yaml: " + wantErrField))
		},
		Entry("baseline static spec is valid", nil, ""),
		Entry("valid writer lease spec (writer_max set)",
			func(s *model.BootSpec) {
				s.IdentityMode = model.IdentityLease
				s.WriterMax = 4
			}, ""),
		Entry("writer lease spec without writer_max is invalid",
			func(s *model.BootSpec) {
				s.IdentityMode = model.IdentityLease
				s.WriterMax = 0
			}, "writer_max"),
		Entry("reader lease spec without reader_max is invalid",
			func(s *model.BootSpec) {
				s.Role = model.RoleReader
				s.IdentityMode = model.IdentityLease
				s.ReaderMax = 0
			}, "reader_max"),
		// Cross-role isolation: the OTHER role's ceiling being present
		// must never satisfy this role's lease gate — a writer spec
		// that carries reader_max (the Terraform render writes both
		// keys into every role's spec) still needs writer_max, and
		// the mirror holds for readers.
		Entry("writer lease spec with reader_max set but writer_max absent is invalid",
			func(s *model.BootSpec) {
				s.IdentityMode = model.IdentityLease
				s.WriterMax = 0
				s.ReaderMax = 10
			}, "writer_max"),
		Entry("reader lease spec with writer_max set but reader_max absent is invalid",
			func(s *model.BootSpec) {
				s.Role = model.RoleReader
				s.IdentityMode = model.IdentityLease
				s.ReaderMax = 0
				s.WriterMax = 4
			}, "reader_max"),
		Entry("reader lease spec with reader_max but zero reader_count is valid (max is the ceiling)",
			func(s *model.BootSpec) {
				s.Role = model.RoleReader
				s.IdentityMode = model.IdentityLease
				s.ReaderMax = 10
				s.ReaderCount = 0
			}, ""),
		// An empty hostname prefix is ALLOWED at validation: prefixes
		// default at claim/inventory time (WriterPrefix/ReaderPrefix),
		// so the Terraform template can omit the key entirely.
		Entry("writer lease spec with an empty writer_hostname_prefix is valid",
			func(s *model.BootSpec) {
				s.IdentityMode = model.IdentityLease
				s.WriterHostPrefix = ""
				s.WriterMax = 4
			}, ""),
		Entry("static writer spec stays valid (interim path)",
			func(s *model.BootSpec) {
				s.IdentityMode = model.IdentityStatic
				s.WriterCount = 2
				s.WriterNodes = []string{"writer-0", "writer-1"}
			}, ""),
		Entry("reader lease with negative reader_max is invalid",
			func(s *model.BootSpec) {
				s.Role = model.RoleReader
				s.IdentityMode = model.IdentityLease
				s.ReaderMax = -1
			}, "reader_max"),
		Entry("writer lease with negative writer_max is invalid",
			func(s *model.BootSpec) {
				s.IdentityMode = model.IdentityLease
				s.WriterMax = -1
			}, "writer_max"),
		Entry("unknown role is invalid", func(s *model.BootSpec) { s.Role = "coordinator" }, "role"),
		Entry("missing role is invalid", func(s *model.BootSpec) { s.Role = "" }, "role"),
		Entry("missing identity mode is invalid", func(s *model.BootSpec) { s.IdentityMode = "" }, "identity_mode"),
		Entry("unknown identity mode is invalid", func(s *model.BootSpec) { s.IdentityMode = "dns" }, "identity_mode"),
		Entry("empty config bucket is invalid", func(s *model.BootSpec) { s.ConfigBundle.Bucket = "" }, "config_bundle.bucket"),
		Entry("empty metadata disk label is invalid", func(s *model.BootSpec) { s.Disks.MetadataLabel = "" }, "disks.metadata_label"),
		Entry("empty cluster name is invalid", func(s *model.BootSpec) { s.ClusterName = "" }, "cluster_name"),
		Entry("empty default-password secret is invalid", func(s *model.BootSpec) { s.Secrets.DefaultPassword = "" }, "secrets.default_password"),
		// The multi-AZ placement contract: opting in requires the
		// shared-blob cold tier (Cost-Audit §5.1 — stock peer-to-peer
		// replication across AZs bills $0.02/GB on every part fetch).
		// Disabled placement (the zero value) never validates the tier.
		Entry("multi-az enabled without cold_tier is invalid",
			func(s *model.BootSpec) {
				s.MultiAZ = model.MultiAZ{Enabled: true}
			}, "multi_az.cold_tier"),
		Entry("multi-az enabled with the cas cold tier is valid",
			func(s *model.BootSpec) {
				s.MultiAZ = model.MultiAZ{Enabled: true, ColdTier: "cas"}
			}, ""),
		Entry("multi-az disabled carries no cold_tier obligation",
			func(s *model.BootSpec) {
				s.MultiAZ = model.MultiAZ{Enabled: false}
			}, ""),
	)
})

var _ = Describe("BootSpec prefix and ceiling defaults", func() {
	// Where the empty-prefix allowance materializes: the accessors
	// default the claim/inventory prefixes, and the ceilings fall back
	// to the legacy counts for static specs. settleIdentity's claim
	// specs (the service suite) pin the prefix default at claim time;
	// these specs pin the accessor contract itself.
	It("defaults the writer and reader prefixes when the spec omits them", func() {
		spec := model.BootSpec{}
		Expect(spec.WriterPrefix()).To(Equal(model.DefaultWriterHostPrefix))
		Expect(spec.ReaderPrefix()).To(Equal(model.DefaultReaderHostPrefix))

		spec.WriterHostPrefix = "ingest-"
		spec.ReaderHostPrefix = "query-"
		Expect(spec.WriterPrefix()).To(Equal("ingest-"))
		Expect(spec.ReaderPrefix()).To(Equal("query-"))
	})

	It("keys the writer ceiling on writer_max, falling back to the legacy count", func() {
		spec := model.BootSpec{WriterMax: 4, WriterCount: 2}
		Expect(spec.WriterCeiling()).To(Equal(4), "the ASG max reserves the CR slots")

		spec.WriterMax = 0
		Expect(spec.WriterCeiling()).To(Equal(2), "static specs keep the baseline count")
	})

	It("keys the reader ceiling on reader_max, falling back to the baseline count", func() {
		spec := model.BootSpec{ReaderMax: 10, ReaderCount: 3}
		Expect(spec.ReaderCeiling()).To(Equal(10), "a scale-out past the baseline must still be listable")

		spec.ReaderMax = 0
		Expect(spec.ReaderCeiling()).To(Equal(3))
	})
})

var _ = Describe("BootSpec YAML round-trip", func() {
	It("preserves the writer hostname prefix and role ceilings", func() {
		spec := baselineSpec()
		spec.IdentityMode = model.IdentityLease
		spec.WriterHostPrefix = "writer-"
		spec.WriterMax = 4
		spec.ReaderMax = 10

		data, err := yaml.Marshal(spec)
		Expect(err).NotTo(HaveOccurred())
		var loaded model.BootSpec
		Expect(yaml.Unmarshal(data, &loaded)).To(Succeed())

		Expect(loaded.WriterHostPrefix).To(Equal("writer-"))
		Expect(loaded.WriterMax).To(Equal(4))
		Expect(loaded.ReaderMax).To(Equal(10))
		Expect(loaded.Validate()).To(Succeed())
	})

	It("parses the documented yaml key names from a rendered chinit.yaml", func() {
		// The Terraform template writes these keys verbatim; a tag
		// drift here breaks every writer boot after the ASG rollout.
		const doc = `
role: writer
identity_mode: lease
writer_hostname_prefix: writer-
writer_max: 4
reader_max: 10
cluster_name: demo
dns_zone_name: ch.internal
multi_az:
  enabled: true
  cold_tier: cas
config_bundle:
  bucket: data-bucket
  prefix: bootstrap
disks:
  metadata_label: ch-metadata
secrets:
  default_password: default-pw
  admin_password: admin-pw
`
		var spec model.BootSpec
		Expect(yaml.Unmarshal([]byte(doc), &spec)).To(Succeed())
		Expect(spec.WriterHostPrefix).To(Equal("writer-"))
		Expect(spec.WriterMax).To(Equal(4))
		Expect(spec.ReaderMax).To(Equal(10))
		Expect(spec.MultiAZ.Enabled).To(BeTrue())
		Expect(spec.MultiAZ.ColdTier).To(Equal("cas"))
		Expect(spec.Validate()).To(Succeed())
	})

	It("parses a single-AZ spec with no multi_az section (the default placement)", func() {
		// The zero value IS the single-AZ posture: an omitempty'd
		// absent section must neither flip placement nor fail
		// validation (every pre-multi-AZ bundle keeps booting).
		const doc = `
role: writer
identity_mode: lease
writer_max: 4
reader_max: 10
cluster_name: demo
dns_zone_name: ch.internal
config_bundle:
  bucket: data-bucket
  prefix: bootstrap
disks:
  metadata_label: ch-metadata
secrets:
  default_password: default-pw
  admin_password: admin-pw
`
		var spec model.BootSpec
		Expect(yaml.Unmarshal([]byte(doc), &spec)).To(Succeed())
		Expect(spec.MultiAZ.Enabled).To(BeFalse())
		Expect(spec.Validate()).To(Succeed())
	})

	It("resolves per-signal VictoriaMetrics / VictoriaTraces / VictoriaLogs and base OTLP URLs", func() {
		// VictoriaMetrics stack: distinct paths/hosts per signal.
		vm := model.LogShipping{
			Agent:           "otel",
			LogsEndpoint:    "https://vlinsert.example.com/insert/opentelemetry/v1/logs",
			MetricsEndpoint: "https://vminsert.example.com/insert/0/prometheus/api/v1/write",
			TracesEndpoint:  "https://vtinsert.example.com/insert/opentelemetry/v1/traces",
			Headers:         "Authorization=Bearer base-tok",
			TracesHeaders:   "Authorization=Bearer trace-tok",
		}
		Expect(vm.HasAnyEndpoint()).To(BeTrue())
		Expect(vm.IsPrometheusRemoteWrite()).To(BeTrue())
		Expect(vm.EffectiveLogsEndpoint()).To(Equal("https://vlinsert.example.com/insert/opentelemetry/v1/logs"))
		Expect(vm.EffectiveMetricsEndpoint()).To(Equal("https://vminsert.example.com/insert/0/prometheus/api/v1/write"))
		Expect(vm.EffectiveTracesEndpoint()).To(Equal("https://vtinsert.example.com/insert/opentelemetry/v1/traces"))
		Expect(vm.EffectiveMetricsHeaders()).To(Equal("Authorization=Bearer base-tok"))
		Expect(vm.EffectiveTracesHeaders()).To(Equal("Authorization=Bearer trace-tok"))

		// VictoriaMetrics OTLP metrics path must not double-append /v1/metrics.
		vmOTLP := model.LogShipping{
			MetricsEndpoint: "https://vm.example.com/opentelemetry/v1/metrics",
		}
		Expect(vmOTLP.IsPrometheusRemoteWrite()).To(BeFalse())
		Expect(vmOTLP.EffectiveMetricsEndpoint()).To(Equal("https://vm.example.com/opentelemetry/v1/metrics"))
		Expect(vmOTLP.EffectiveLogsEndpoint()).To(BeEmpty())
		Expect(vmOTLP.EffectiveTracesEndpoint()).To(BeEmpty())

		// Base OTLP collector URL expands cleanly across all three signals.
		base := model.LogShipping{Endpoint: "https://otel.example.com:4318/"}
		Expect(base.EffectiveLogsEndpoint()).To(Equal("https://otel.example.com:4318/v1/logs"))
		Expect(base.EffectiveMetricsEndpoint()).To(Equal("https://otel.example.com:4318/v1/metrics"))
		Expect(base.EffectiveTracesEndpoint()).To(Equal("https://otel.example.com:4318/v1/traces"))
	})
})
