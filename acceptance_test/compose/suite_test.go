// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build acceptance

package compose_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Suite flags, wired through `go test -args`:
//
//	go test -tags acceptance ./acceptance_test/compose/ -args -keep-cluster
var (
	keepCluster  = flag.Bool("keep-cluster", false, "keep the compose cluster running after the suite (teardown is skipped on success)")
	reuseCluster = flag.Bool("reuse-cluster", false, "reuse a running cluster instead of rebuilding (fast local re-checks; the image may be stale)")
)

// env is the suite's shared cluster environment, built by BeforeSuite.
var env *clusterEnv

// clusterEnv carries the docker compose invocation and the resolved
// paths every spec queries through.
type clusterEnv struct {
	// ctx bounds every docker/compose and SQL call of the suite.
	ctx context.Context
	// base is ["docker", "compose"] or ["compose"] — whichever the
	// host provides.
	base []string
	// composeFile is the absolute path of the cluster definition.
	composeFile string
	// projectName isolates the suite's compose project.
	projectName string
	// clusterName is the ClickHouse cluster the spec declares.
	clusterName string
}

// args builds one docker compose invocation.
func (e *clusterEnv) args(extra ...string) []string {
	args := append([]string{}, e.base...)
	args = append(args, "-p", e.projectName, "-f", e.composeFile)
	return append(args, extra...)
}

// compose runs one docker compose command, asserting success.
func (e *clusterEnv) compose(description string, extra ...string) string {
	GinkgoWriter.Printf("[$ compose %s]\n", strings.Join(extra, " "))
	out, err := e.run(e.args(extra...))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "%s failed:\n%s", description, out)
	return out
}

// composeTry runs one docker compose command, returning output and
// error without asserting (pre-clean paths).
func (e *clusterEnv) composeTry(extra ...string) (string, error) {
	return e.run(e.args(extra...))
}

// run executes a command on the suite context, returning combined output.
func (e *clusterEnv) run(args []string) (string, error) {
	return e.runWith(e.ctx, args)
}

// runWith executes a command on an explicit context, returning combined
// output. The teardown path needs this: its commands outlive the suite
// context (see the ReportAfterSuite note).
func (e *clusterEnv) runWith(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// chQuery runs one SQL statement on a node's clickhouse-client and
// returns the TSV output.
func (e *clusterEnv) chQuery(node, query string) (string, error) {
	args := e.args("exec", "-T", node, "clickhouse-client",
		"--user", "admin", "--password", "compose-admin-pass",
		"--query", query, "--format", "TSV")
	return e.run(args)
}

// chQueryOK runs one SQL statement, asserting success, and returns the
// trimmed TSV output.
func (e *clusterEnv) chQueryOK(node, query string) string {
	out, err := e.chQuery(node, query)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "%s: query %q failed:\n%s", node, query, out)
	return strings.TrimSpace(out)
}

// dumpNodeLogs prints one node's chinit-boot and clickhouse-server
// journals to GinkgoWriter so a failing spec's output names the cause.
func (e *clusterEnv) dumpNodeLogs(node string) {
	fmt.Fprintf(GinkgoWriter, "--- %s: chinit-boot journal (last 60) ---\n", node)
	out, err := e.run(e.args("exec", "-T", node, "journalctl", "-u", "chinit-boot.service", "--no-pager", "-n", "60"))
	fmt.Fprintln(GinkgoWriter, orErrOut(out, err))
	fmt.Fprintf(GinkgoWriter, "--- %s: clickhouse-server journal (last 30) ---\n", node)
	out, err = e.run(e.args("exec", "-T", node, "journalctl", "-u", "clickhouse-server", "--no-pager", "-n", "30"))
	fmt.Fprintln(GinkgoWriter, orErrOut(out, err))
}

// dumpDiagnostics prints compose ps plus every node's journals — the
// failure-path context the suite always wants in CI output.
func (e *clusterEnv) dumpDiagnostics() {
	fmt.Fprintln(GinkgoWriter, "--- compose ps ---")
	out, _ := e.run(e.args("ps"))
	fmt.Fprintln(GinkgoWriter, out)
	for _, node := range []string{"writer-0", "reader-0", "keeper-0"} {
		e.dumpNodeLogs(node)
	}
}

func orErrOut(out string, err error) string {
	if err != nil {
		if out == "" {
			return fmt.Sprintf("(error: %v)", err)
		}
		return out + fmt.Sprintf("\n(error: %v)", err)
	}
	return out
}

// resolveRepoRoot locates the repo root by walking up for the compose
// file (go test may run from anywhere; -repo equivalent via
// CHINIT_E2E_REPO env).
func resolveRepoRoot() string {
	if root := os.Getenv("CHINIT_E2E_REPO"); root != "" {
		return root
	}
	dir, err := os.Getwd()
	Expect(err).NotTo(HaveOccurred(), "getwd failed")
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, "deploy", "compose", "docker-compose.yaml")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			Fail(fmt.Sprintf("cannot locate repo root (no deploy/compose/docker-compose.yaml upward from %s); set CHINIT_E2E_REPO", dir))
		}
	}
}

// resolveCompose finds a working docker compose invocation: the CLI
// plugin (docker compose) when present, else the standalone binary.
func resolveCompose(ctx context.Context) []string {
	if _, err := exec.LookPath("docker"); err == nil {
		out, err := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput()
		if err == nil {
			_ = out
			return []string{"docker", "compose"}
		}
	}
	if _, err := exec.LookPath("compose"); err == nil {
		return []string{"compose"}
	}
	Fail("no docker compose found (need `docker compose` plugin or standalone `compose`)")
	return nil
}

// suiteDeadline bounds the whole run; boot convergence and the
// replication wait each carve their own slice.
const suiteDeadline = 25 * time.Minute

var _ = BeforeSuite(func() {
	ctx, cancel := context.WithTimeout(context.Background(), suiteDeadline)
	DeferCleanup(cancel)

	root := resolveRepoRoot()
	composeFile := filepath.Join(root, "deploy", "compose", "docker-compose.yaml")
	Expect(filepath.IsAbs(composeFile)).To(BeTrue())

	env = &clusterEnv{
		ctx:         ctx,
		base:        resolveCompose(ctx),
		composeFile: composeFile,
		projectName: "chinit-e2e",
		clusterName: "compose-demo",
	}

	if *reuseCluster {
		GinkgoWriter.Println("== reusing running cluster (image NOT rebuilt) ==")
	} else {
		By("bringing the cluster down (fresh volumes)")
		if out, err := env.composeTry("down", "-v", "--timeout", "10"); err != nil {
			GinkgoWriter.Printf("pre-clean: %v\n%s\n", err, out) // not fatal: may not exist yet
		}

		By("building the chinit-node image from the current checkout")
		env.compose("build chinit-node", "build", "--pull", "writer-0")

		By("booting the cluster (compose up --wait gates on healthchecks)")
		out, err := env.composeTry("up", "-d", "--wait")
		if err != nil {
			env.dumpDiagnostics()
			Fail(fmt.Sprintf("compose up failed (or a healthcheck never went green): %v\n%s", err, out))
		}
	}
})

// The teardown decision lives in ReportAfterSuite, the only node that
// sees the finished suite verdict: a FAILED run keeps the cluster up
// for inspection, a green run tears it down (or keeps it, per
// -keep-cluster).
//
// The teardown runs on a FRESH background context: the suite context
// is cancelled by BeforeSuite's DeferCleanup, which runs as an
// AfterSuite node BEFORE ReportAfterSuite — a teardown issued through
// the suite context would die with "context canceled" and leak the
// cluster.
var _ = ReportAfterSuite("teardown decision", func(report Report) {
	if env == nil {
		return
	}
	if !report.SuiteSucceeded {
		GinkgoWriter.Println("== suite failed: cluster left running for inspection ==")
		return
	}
	if *keepCluster {
		GinkgoWriter.Println("== PASS: cluster left running (-keep-cluster) ==")
		return
	}
	By("tearing the cluster down")
	downCtx, downCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer downCancel()
	out, err := env.runWith(downCtx, env.args("down", "-v", "--timeout", "10"))
	if err != nil {
		GinkgoWriter.Printf("teardown warning: %v\n%s\n", err, out)
	} else {
		fmt.Fprintln(GinkgoWriter, out)
	}
})
