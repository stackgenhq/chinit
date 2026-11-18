// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
	"github.com/stackgenhq/chinit/pkg/chinit/infra/clusterops"
)

// clusterOps is the cluster-operations port used by subcommands.
var clusterOps clusterops.Ops = clusterops.New()

// clusterRequest builds the shared binding for a cluster operation
// from the settled state: cluster, spec, live-claims inventory and
// the admin credentials (read from the boot's password file so the
// value never hits another process's argv — one more reason the
// in-process port beats the CLI).
func clusterRequest(st *nodeState) clusterops.ClusterRequest {
	return clusterRequestWithHosts(st, specInventoryHosts(st))
}

// clusterRequestWithHosts is clusterRequest over a caller-built
// inventory: the drain's peer drop carries ONLY the two hosts the
// drop touches (self + the peer), not the full slot list.
func clusterRequestWithHosts(st *nodeState, hosts []clusterops.Host) clusterops.ClusterRequest {
	return clusterops.ClusterRequest{
		ClusterName: st.clusterName(),
		SpecPath:    clusterops.DefaultSpecPath(),
		Hosts:       hosts,
		Credentials: domain.Credentials{Username: "admin", Password: adminPassword()},
	}
}

// specInventoryHosts projects the spec inventory (live claims, with
// the static fallback) onto the port's Host list.
func specInventoryHosts(st *nodeState) []clusterops.Host {
	hosts := make([]clusterops.Host, 0)
	for _, pair := range st.specInventory() {
		name, addr, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		hosts = append(hosts, clusterops.Host{Name: name, Address: addr})
	}
	return hosts
}

// ceilingClusterRequest builds a ClusterRequest populated with the full
// ceiling slot inventory (0..writer_max-1, 0..reader_max-1, backup) from
// chinit.yaml so dead-replica-gc's DropReplica membership check accepts
// retired ordinals whose active lease has already expired or been released.
func ceilingClusterRequest(st *nodeState) clusterops.ClusterRequest {
	if spec, err := loadInventorySpec(); err == nil && spec != nil && spec.DNSZoneName != "" {
		hosts := make([]clusterops.Host, 0)
		for _, pair := range hostInventory(*spec) {
			name, addr, ok := strings.Cut(pair, "=")
			if !ok {
				continue
			}
			hosts = append(hosts, clusterops.Host{Name: name, Address: addr})
		}
		if len(hosts) > 0 {
			return clusterRequestWithHosts(st, hosts)
		}
	}
	return clusterRequest(st)
}

// runCommand is the exec seam: production shells out via
// exec.CommandContext (CombinedOutput), tests substitute a recorder.
// A package-level var rather than a runner struct on purpose — the
// drain is one function over the nodeState, and the var matches the
// package's existing seams (inventorySpecPath).
var runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runCommandWithEnv(ctx, nil, name, args...)
}

// runCommandWithEnv is runCommand with explicit child-process env:
// the credential-bearing clickhouse-client calls use it to pass the
// admin user/password via CLICKHOUSE_USER/CLICKHOUSE_PASSWORD env
// instead of argv — argv is world-readable in /proc/<pid>/cmdline for
// the process's whole lifetime. Credentials NEVER go on argv (the same
// rule the clusterRequest comment states for chctl argv).
var runCommandWithEnv = func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.CombinedOutput()
}

// clickhouseClientEnv builds the env vars that authenticate a local
// clickhouse-client invocation as the admin user. The password comes
// from the boot's password file (the same source every other admin
// credential path uses); empty when the file is absent, so a
// passwordless cluster behaves identically. Callers MUST pass this to
// runCommandWithEnv — a plain runCommand would silently fall back to
// the readonly `default` user (bare
// clickhouse-client calls authenticate as `default`, whose profile
// the chi spec renders readonly, and the graceful-drain queries get
// refused during scale-in).
func clickhouseClientEnv() []string {
	env := []string{"CLICKHOUSE_USER=admin"}
	if pw := adminPassword(); pw != "" {
		env = append(env, "CLICKHOUSE_PASSWORD="+pw)
	}
	return env
}

// statePathFlag parses the common -state flag.
func statePathFlag(args []string) (string, error) {
	fs := flag.NewFlagSet("chinit job", flag.ContinueOnError)
	path := fs.String("state", service.ChinitStatePath, "state file path")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return *path, nil
}
