// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build ignore

// E2E acceptance test for chinit on the local Docker Compose cluster.
//
// Run:
//
//	go run ./acceptance_test/e2e.go
//
// The harness boots the compose cluster (deploy/compose/docker-compose.yaml,
// rebuilding the chinit-node image from the CURRENT checkout so the test
// exercises the code under change), waits for boot convergence, then
// verifies the cluster's core promise end to end:
//
//  1. keeper-0's ensemble is serving (ruok → imok)
//  2. writer-0 and reader-0 finished chinit-boot (boot-finished marker + live SQL ping)
//  3. the compose-demo cluster is registered on both nodes with both replicas
//  4. a row written on WRITER-0 arrives on READER-0 (ReplicatedMergeTree
//     replication across the secure interserver port — the whole point
//     of the stack)
//  5. system.replicas reports the events table writable with all replicas active
//
// Exit codes: 0 = pass; 1 = failure (the failing phase prints what it saw
// and the node journals that explain it). Designed for CI (docker compose
// up/down is orchestrated by the harness; pass -keep to leave the cluster
// up, pass -reuse to re-check a running one without rebuilding).
//
//go:nolint
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// composeFlags carry the docker compose invocation, resolved once.
type composeFlags struct {
	// base is ["docker", "compose"] or ["compose"] — whichever the
	// environment provides (the CLI plugin is standard; the standalone
	// binary is the fallback).
	base        []string
	projectName string
	fileArg     string
}

func (f composeFlags) args(extra ...string) []string {
	args := append([]string{}, f.base...)
	if f.projectName != "" {
		args = append(args, "-p", f.projectName)
	}
	if f.fileArg != "" {
		args = append(args, "-f", f.fileArg)
	}
	return append(args, extra...)
}

// resolveCompose finds a working docker compose invocation: the CLI
// plugin (docker compose) when the docker CLI is present, else the
// standalone compose binary.
func resolveCompose(ctx context.Context, root string) composeFlags {
	composeFile := filepath.Join(root, "deploy", "compose", "docker-compose.yaml")
	if _, err := exec.LookPath("docker"); err == nil {
		if out, err := exec.CommandContext(ctx, "docker", "compose", "version").CombinedOutput(); err == nil {
			_ = out
			return composeFlags{base: []string{"docker", "compose"}, projectName: "chinit-e2e", fileArg: composeFile}
		}
	}
	if _, err := exec.LookPath("compose"); err == nil {
		return composeFlags{base: []string{"compose"}, projectName: "chinit-e2e", fileArg: composeFile}
	}
	failf("no docker compose found (need `docker compose` plugin or standalone `compose`)")
	return composeFlags{}
}

func main() {
	var (
		keep       = flag.Bool("keep", false, "keep the cluster running on success (failure always keeps it for inspection)")
		reuse      = flag.Bool("reuse", false, "reuse a running cluster instead of rebuilding (fast local re-checks; the image may be stale)")
		timeout    = flag.Duration("timeout", 15*time.Minute, "overall deadline for boot convergence and the run")
		repoRoot   = flag.String("repo", "", "repo root (default: located by walking up for deploy/compose/docker-compose.yaml)")
		clusterRef = flag.String("cluster", "compose-demo", "ClickHouse cluster name the spec declares")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	root := *repoRoot
	if root == "" {
		root = defaultRepoRoot()
	}
	if _, err := os.Stat(filepath.Join(root, "deploy", "compose", "docker-compose.yaml")); err != nil {
		failf("compose file not found: %v", err)
	}
	flags := resolveCompose(ctx, root)

	// Phase 1: bring the cluster up (fresh volumes unless -reuse).
	if *reuse {
		fmt.Println("== reusing running cluster (image NOT rebuilt) ==")
	} else {
		fmt.Println("== bringing the compose cluster down (fresh volumes) ==")
		if out, err := run(ctx, flags.args("down", "-v", "--timeout", "10")); err != nil {
			fmt.Printf("pre-clean: %v\n%s\n", err, out) // not fatal: may not exist yet
		}
		fmt.Println("== building chinit-node image from the current checkout ==")
		if out, err := run(ctx, flags.args("build", "--pull", "writer-0")); err != nil {
			failf("build chinit-node: %v\n%s", err, out)
		}
		fmt.Println("== compose up -d (waiting for healthchecks) ==")
		if out, err := run(ctx, flags.args("up", "-d", "--wait")); err != nil {
			dumpDiagnostics(ctx, flags)
			failf("compose up failed (or a healthcheck never went green): %v\n%s", err, out)
		}
	}

	// Phase 2: keeper ensemble answering 4lw (ruok → imok). nc ships in
	// the clickhouse base image; the pipe half-closes the socket, which
	// keeper's 4lw needs to answer.
	fmt.Println("== keeper ensemble ==")
	out, err := run(ctx, flags.args("exec", "-T", "keeper-0", "bash", "-c", "echo ruok | timeout 5 nc 127.0.0.1 9181"))
	if err != nil || !strings.Contains(out, "imok") {
		dumpNodeLogs(ctx, flags, "keeper-0")
		failf("keeper ruok did not answer imok (out: %q err: %v)", strings.TrimSpace(out), err)
	}
	fmt.Println("   keeper-0: ruok → imok")

	// Phase 3: boot finished on writer and reader. The marker file proves
	// chinit-boot completed (the healthchecks gate on it too); the SQL
	// ping proves clickhouse is serving, belt and braces.
	for _, node := range []string{"writer-0", "reader-0"} {
		fmt.Printf("== boot convergence on %s ==\n", node)
		if mOut, mErr := run(ctx, flags.args("exec", "-T", node, "test", "-f", "/var/lib/cloud/instance/boot-finished")); mErr != nil {
			dumpNodeLogs(ctx, flags, node)
			failf("%s: boot-finished marker missing — chinit-boot never completed: %v\n%s", node, mErr, mOut)
		}
		if pOut, pErr := chQuery(ctx, flags, node, "SELECT 1"); pErr != nil || strings.TrimSpace(pOut) != "1" {
			dumpNodeLogs(ctx, flags, node)
			failf("%s: clickhouse SQL ping failed: %v (out %q)", node, pErr, pOut)
		}
		fmt.Printf("   %s: boot finished, SQL alive\n", node)
	}

	// Phase 4: the cluster is registered on both nodes with both replicas
	// (remote_servers rendering + lease-claimed identity + DNS aliases).
	for _, node := range []string{"writer-0", "reader-0"} {
		fmt.Printf("== cluster topology on %s ==\n", node)
		q := fmt.Sprintf("SELECT count(), countIf(is_local) FROM system.clusters WHERE cluster = '%s'", *clusterRef)
		out, err := chQuery(ctx, flags, node, q)
		if err != nil {
			dumpNodeLogs(ctx, flags, node)
			failf("%s: cluster query failed: %v\n%s", node, err, out)
		}
		fields := strings.Fields(strings.TrimSpace(out))
		if len(fields) != 2 || fields[0] != "2" || fields[1] != "1" {
			dumpDiagnostics(ctx, flags)
			failf("%s: cluster %q should list 2 replicas (1 local); got %q", node, *clusterRef, strings.TrimSpace(out))
		}
		fmt.Printf("   %s: cluster %q lists both replicas\n", node, *clusterRef)
	}

	// Phase 5: WRITE on writer-0, READ on reader-0 — the acceptance
	// heart. The marker row's source tags this run so a re-run on a
	// reused cluster cannot pass on yesterday's data.
	marker := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	fmt.Printf("== write on writer-0 (marker %s) ==\n", marker)
	insert := fmt.Sprintf("INSERT INTO default.events (source, message) VALUES ('%s', 'e2e write-read acceptance')", marker)
	if out, err := chQuery(ctx, flags, "writer-0", insert); err != nil {
		dumpNodeLogs(ctx, flags, "writer-0")
		failf("insert on writer-0 failed: %v\n%s", err, out)
	}

	fmt.Println("== read the marker back on reader-0 ==")
	selectQ := fmt.Sprintf("SELECT message FROM default.events WHERE source = '%s'", marker)
	readerSaw := ""
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := chQuery(ctx, flags, "reader-0", selectQ)
		if err == nil && strings.TrimSpace(out) != "" {
			readerSaw = strings.TrimSpace(out)
			break
		}
		select {
		case <-ctx.Done():
			failf("overall deadline exceeded while waiting for replication")
		case <-time.After(2 * time.Second):
		}
	}
	if readerSaw == "" {
		dumpDiagnostics(ctx, flags)
		failf("reader-0 never saw marker %q within 90s — replication from writer-0 broken", marker)
	}
	fmt.Printf("   reader-0 saw: %q\n", readerSaw)

	// Phase 6: replica state healthy on the reader (events table
	// writable, all replicas active — the fleet's posture, not just
	// this row).
	fmt.Println("== replica health on reader-0 ==")
	out, err = chQuery(ctx, flags, "reader-0",
		"SELECT count(), countIf(is_readonly), countIf(active_replicas != total_replicas) FROM system.replicas WHERE database = 'default' AND table = 'events'")
	if err != nil {
		failf("replica health query failed: %v\n%s", err, out)
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 3 || fields[0] == "0" {
		dumpDiagnostics(ctx, flags)
		failf("reader-0: no healthy events replica (out %q)", strings.TrimSpace(out))
	}
	if fields[1] != "0" {
		dumpDiagnostics(ctx, flags)
		failf("reader-0: events replica is READONLY (out %q) — keeper connectivity broken", strings.TrimSpace(out))
	}
	if fields[2] != "0" {
		dumpDiagnostics(ctx, flags)
		failf("reader-0: not all replicas active (out %q)", strings.TrimSpace(out))
	}
	fmt.Println("   events replica: writable, all replicas active")

	// Pass. Tear down unless asked to keep.
	if *keep {
		fmt.Println("PASS — cluster left running (rerun with -reuse to re-check)")
		return
	}
	fmt.Println("== teardown ==")
	if out, err := run(ctx, flags.args("down", "-v", "--timeout", "10")); err != nil {
		fmt.Printf("teardown warning: %v\n%s\n", err, out)
	}
	fmt.Println("PASS")
}

// chQuery runs one SQL statement on a node's clickhouse-client.
func chQuery(ctx context.Context, f composeFlags, node, query string) (string, error) {
	args := f.args("exec", "-T", node, "clickhouse-client",
		"--user", "admin", "--password", "compose-admin-pass",
		"--query", query, "--format", "TSV")
	return run(ctx, args)
}

// dumpNodeLogs prints the boot and server journal of one node.
func dumpNodeLogs(ctx context.Context, f composeFlags, node string) {
	fmt.Printf("--- %s: chinit-boot journal (last 60) ---\n", node)
	out, err := run(ctx, f.args("exec", "-T", node, "journalctl", "-u", "chinit-boot.service", "--no-pager", "-n", "60"))
	fmt.Println(orDefault(out, err))
	fmt.Printf("--- %s: clickhouse-server journal (last 30) ---\n", node)
	out, err = run(ctx, f.args("exec", "-T", node, "journalctl", "-u", "clickhouse-server", "--no-pager", "-n", "30"))
	fmt.Println(orDefault(out, err))
}

// dumpDiagnostics prints compose status plus every node's journals.
func dumpDiagnostics(ctx context.Context, f composeFlags) {
	fmt.Println("--- compose ps ---")
	out, err := run(ctx, f.args("ps"))
	fmt.Println(orDefault(out, err))
	for _, node := range []string{"writer-0", "reader-0", "keeper-0"} {
		dumpNodeLogs(ctx, f, node)
	}
}

// run executes a command and returns combined output. Errors are returned
// with the output so callers can print both in one shot.
func run(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf("empty command")
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// defaultRepoRoot locates the repo root by walking up from the working
// directory for the compose file (go run may be invoked from anywhere in
// the repo; -repo overrides).
func defaultRepoRoot() string {
	if root := os.Getenv("CHINIT_E2E_REPO"); root != "" {
		return root
	}
	dir, err := os.Getwd()
	if err != nil {
		failf("getwd: %v", err)
	}
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, "deploy", "compose", "docker-compose.yaml")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			failf("cannot locate repo root (no deploy/compose/docker-compose.yaml upward from %s); pass -repo", dir)
		}
	}
}

func orDefault(out string, err error) string {
	if err != nil {
		if out == "" {
			return fmt.Sprintf("(error: %v)", err)
		}
		return out + fmt.Sprintf("\n(error: %v)", err)
	}
	return out
}

func failf(format string, args ...any) {
	fmt.Printf("FAIL: "+format+"\n", args...)
	os.Exit(1)
}
