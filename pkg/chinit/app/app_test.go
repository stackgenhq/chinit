// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
)

// This file pins the CLI surface: subcommand dispatch and the boot
// flag contract. The subcommands' own behavior lives in their spec
// files (drain_test.go, maintenance_test.go, spec_inventory_test.go,
// lease_renew_test.go).

var _ = Describe("Run (subcommand dispatch)", func() {
	It("errors with usage when no command is given", func() {
		Expect(Run(nil)).To(HaveOccurred())
	})

	It("errors on an unknown command, naming it", func() {
		err := Run([]string{"teleport"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("teleport"))
	})

	It("prints the version", func() {
		// The stamp bootstrap scripts compare against the release tag.
		Expect(Run([]string{"version"})).To(Succeed())
	})
})

var _ = Describe("parseBootFlags", func() {
	It("defaults the spec path to the bootstrap bundle location and detects the cloud", func() {
		flags, err := parseBootFlags([]string{})
		Expect(err).NotTo(HaveOccurred())
		Expect(flags.specPath).To(Equal("/var/lib/clickhouse-bootstrap/chinit.yaml"))
		Expect(flags.cloud).To(BeEmpty())
		Expect(flags.dryRun).To(BeFalse())
	})

	It("rejects stray positionals as usage errors, not silently dropped args", func() {
		_, err := parseBootFlags([]string{"unexpected"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unexpected"))
	})
})

var _ = Describe("boot dry-run", func() {
	// writeSpec seeds a chinit.yaml the table below mutates.
	writeSpec := func(content string) string {
		path := filepath.Join(GinkgoT().TempDir(), "chinit.yaml")
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())
		return path
	}

	const validSpec = `role: writer
identity_mode: static
cluster_name: demo
config_bundle:
  bucket: data-bucket
  prefix: bootstrap
disks:
  metadata_label: ch-metadata
secrets:
  default_password: default-secret-name
  admin_password: admin-secret-name
`

	It("errors cleanly when the spec file is missing", func() {
		err := Run([]string{"boot", "-spec", "/nonexistent/chinit.yaml"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("/nonexistent/chinit.yaml"))
	})

	It("validates the spec and prints the step plan without touching the system", func() {
		spec := writeSpec(validSpec)
		// -cloud pins the provider (a dev machine has no DMI/IMDS
		// detection is exercised in the detect package's tests).
		dir := GinkgoT().TempDir()
		Expect(Run([]string{"boot", "-spec", spec, "-cloud", "aws", "-dry-run", "-state", filepath.Join(dir, "state.json")})).To(Succeed())
	})

	It("names the invalid field when the spec is malformed", func() {
		// role present, identity_mode missing → SpecError naming it.
		spec := writeSpec(`role: writer
cluster_name: demo
config_bundle:
  bucket: data-bucket
disks:
  metadata_label: ch-metadata
secrets:
  default_password: x
`)
		err := Run([]string{"boot", "-spec", spec, "-dry-run"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("identity_mode"))
	})
})

var _ = Describe("the command registry", func() {
	It("dispatches every registered name", func() {
		// The registry IS the dispatch table — a name present here is
		// runnable, and Run's unknown-command path answers anything
		// else. The boot command needs its spec; the maintenance
		// commands fail fast on their state file — both are
		// distinguishable from "unknown command".
		for _, c := range commands {
			_, known := commandByName(c.Name)
			Expect(known).To(BeTrue(), "every registry name resolves: %s", c.Name)
		}
	})

	It("names every ExecStart unit command (the units can drift from the registry only visibly)", func() {
		// The systemd units' ExecStart lines reference subcommands by
		// name (chinitUnit's third arg). This spec pins the reverse
		// direction: every name the boot's unit matrix installs must
		// exist in the dispatch registry — a renamed subcommand that
		// misses a unit shows up here as an unknown command.
		unitCommands := []string{
			"schema-sync", "schema-bootstrap", "replica-sync", "queue-heal",
			"lease-renew", "keeper-health", "dead-replica-gc", "orphaned-gc",
			"backup-daily", "backup-wake", "spot-watch", "drain", "drain-shutdown",
			"disk-autoscale", "span-ship",
		}
		byName := map[string]bool{}
		for _, c := range commands {
			byName[c.Name] = true
		}
		for _, name := range unitCommands {
			Expect(byName[name]).To(BeTrue(), "unit ExecStart command %q must be in the registry", name)
		}
	})
})

type stubSpanLogReader struct {
	spans []model.RecordedSpan
}

func (r *stubSpanLogReader) ReadSpans(_ context.Context, _ port.SpanLogQuery) ([]model.RecordedSpan, error) {
	return r.spans, nil
}

var _ = Describe("span-ship coordinator", func() {
	It("wires SpanLogReader, JSONSpanCursorStore, and OTLPSpanSink through service.SpanShipper", func() {
		var exportedBatches atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/insert/opentelemetry/v1/traces" {
				exportedBatches.Add(1)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		dir := GinkgoT().TempDir()
		specFile := filepath.Join(dir, "chinit.yaml")
		Expect(os.WriteFile(specFile, []byte("log_shipping:\n  traces_endpoint: "+srv.URL+"/insert/opentelemetry/v1/traces\n"), 0644)).To(Succeed())
		stateFile := filepath.Join(dir, "state.json")
		Expect(os.WriteFile(stateFile, []byte(`{"identity":{"role":"writer","hostname":"writer-0","cluster_name":"ch-int-dev","region":"us-west-2","cloud":"aws"}}`), 0644)).To(Succeed())
		cursorFile := filepath.Join(dir, "span-ship.json")

		origSpec := inventorySpecPath
		inventorySpecPath = func() string { return specFile }
		DeferCleanup(func() { inventorySpecPath = origSpec })

		coord, err := newSpanShipCoordinator([]string{"-state", stateFile, "-cursor", cursorFile})
		Expect(err).NotTo(HaveOccurred())
		coord.reader = &stubSpanLogReader{
			spans: []model.RecordedSpan{{
				Hostname:          "writer-0",
				ClickHouseVersion: "26.6.8.20004",
				TraceID:           "11111111-2222-3333-4444-555555555555",
				SpanID:            42,
				OperationName:     "query",
				Kind:              "SERVER",
				StartTimeMicros:   1800000000000000,
				FinishTimeMicros:  1800000000500000,
				StatusCode:        "OK",
			}},
		}

		Expect(coord.run(context.Background())).To(Succeed())
		Expect(exportedBatches.Load()).To(Equal(int32(1)))

		stored, ok, err := service.NewJSONSpanCursorStore(cursorFile).LoadCursor()
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		Expect(stored.FinishTimeMicros).To(Equal(uint64(1800000000500000)))
	})
})
