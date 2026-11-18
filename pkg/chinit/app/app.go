// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// Version is the chinit build version (release workflow overrides at
// build time via -ldflags "-X .../app.Version=<tag>"; locally "dev").
var Version = "dev"

// Process exit codes used by scripts and lifecycle hooks.
const (
	exitOK           = 0
	exitUsageError   = 1
	exitBootFailed   = 2
	exitDrainFailure = 3
)

// coder is an error that dictates its own process exit code.
type coder interface {
	ExitCode() int
}

// exitCodeError carries a process exit code (a fatal boot failure is
// distinct from a usage error: the ASG's boot-loop diagnostics).
type exitCodeError struct {
	Code int
	Err  error
}

func (e *exitCodeError) Error() string { return e.Err.Error() }
func (e *exitCodeError) Unwrap() error { return e.Err }
func (e *exitCodeError) ExitCode() int { return e.Code }

// command is one CLI entry: a subcommand's name and its run function.
// Every recurring maintenance job and operator action is a command —
// the registry below is the single dispatch table (adding a
// subcommand is one entry, not a new switch arm).
type command struct {
	// Name is the CLI word (chinit <name>).
	Name string
	// Usage is the flag hint printed after the name ("" = bare).
	Usage string
	// Run executes the subcommand with its remaining args.
	Run func(args []string) error
	// Boot is true for the one-shot boot pipeline (not a maintenance
	// job — usage() and the docs group them apart).
	Boot bool
}

// commands is the dispatch registry, ordered as the usage prints:
// boot, then the schema/replica cluster ops, then the periodic
// maintenance jobs, then the termination-path commands.
var commands = []command{
	{Name: "boot", Run: runBoot, Usage: "[-spec chinit.yaml] [-cloud aws|azure] [-dry-run] [-refresh]", Boot: true},
	{Name: "schema-sync", Run: runSchemaSync, Usage: "[-retries N] [-prune-stale-self]"},
	{Name: "schema-bootstrap", Run: runSchemaBootstrap},
	{Name: "replica-sync", Run: runReplicaSync},
	{Name: "queue-heal", Run: runQueueHeal, Usage: "[-stuck-tries N] [-no-busy-skip]"},
	{Name: "autoscale", Run: runAutoscale, Usage: "[-state FILE] (keeper-0 self-resize; the ordinal-0 writer rolls the fleet)"},
	{Name: "config-check", Run: runConfigCheck, Usage: "[-state FILE] (re-render when the fetched bundle/spec drifts from the recorded fingerprint)"},
	{Name: "lease-renew", Run: runLeaseRenew},
	{Name: "keeper-health", Run: runKeeperHealth},
	{Name: "disk-autoscale", Run: runDiskAutoscale},
	{Name: "dead-replica-gc", Run: runDeadReplicaGc},
	{Name: "orphaned-gc", Run: runOrphanedGc},
	{Name: "fleet-converge", Run: runFleetConverge, Usage: "[-state FILE] (the ordinal-0 writer sweeps the task table)"},
	{Name: "fleet-rollout", Run: runFleetRollout, Usage: "-target-version V [-role writer|reader] [-state FILE] (AMI rollout coordinator)"},
	{Name: "exporter", Run: runExporter, Usage: "[-port 9150] [-state FILE] (the fleet metrics endpoint: /metrics + /clickhouse-metrics)"},
	{Name: "task-poll", Run: runTaskPoll, Usage: "[-state FILE] (claim and run this node's fleet task)"},
	{Name: "fleet-status", Run: runFleetStatus, Usage: "[-json] [-state FILE] (the fleet's status table; exit 2 = degraded)"},
	{Name: "cordon", Run: runCordon, Usage: "[-state FILE] <hostname> (flag a node for maintenance)"},
	{Name: "uncordon", Run: runUncordon, Usage: "[-state FILE] <hostname>"},
	{Name: "spec-sync", Run: runSpecSync, Usage: "[-state FILE] (pull the role spec; converge on drift)"},
	{Name: "backup-daily", Run: runBackupDaily},
	{Name: "backup-wake", Run: runBackupWake, Usage: "[-state FILE] (ordinal-0 writer wakes the on-demand backup replica)"},
	{Name: "spot-watch", Run: runSpotWatch},
	{Name: "drain", Run: runDrain, Usage: "[-reason spot|shutdown|termination-hook] [-timeout seconds]"},
	{Name: "drain-shutdown", Run: runDrainShutdown},
}

// commandByName answers the registry entry for a CLI word.
func commandByName(name string) (command, bool) {
	for _, c := range commands {
		if c.Name == name {
			return c, true
		}
	}
	return command{}, false
}

// Run parses arguments and dispatches to a subcommand.
func Run(args []string) error {
	if len(args) == 0 {
		usage(os.Stdout)
		return fmt.Errorf("no command given")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version":
		fmt.Printf("chinit %s\n", Version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	}
	if c, ok := commandByName(cmd); ok {
		// Every registry subcommand runs instrumented: init/emit/flush
		// wrap the dispatch so every timer tick's outcome reaches the
		// OTLP endpoint (the version/help arms above stay bare — a
		// usage print is not an observable job).
		return runWithTelemetry(c, rest)
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", cmd)
}

// Main is the process entry: prints the error, honors exit codes.
func Main(args []string) {
	// Central logging on every entry path (boot + all subcommands):
	// JSON slog records through the singleton — INFO to stdout,
	// WARN/ERROR to stderr, the journal split the contract always had.
	logger.Init()
	if err := Run(args); err != nil {
		// The terminal error is one structured ERROR record (the
		// level-split handler lands it on stderr, where the raw
		// print always went) — then the exit code distinguishes the
		// failure modes for scripts and lifecycle hooks.
		logger.GetLogger(context.TODO()).Error("chinit: command failed", "err", err)
		var coded coder
		if errors.As(err, &coded) {
			os.Exit(coded.ExitCode())
		}
		var fatal *fail.FatalError
		if errors.As(err, &fatal) {
			os.Exit(exitBootFailed)
		}
		os.Exit(exitUsageError)
	}
}

// usage prints the help.
func usage(w *os.File) {
	_, _ = fmt.Fprintf(w, `chinit %s — cloud-adaptive ClickHouse node bootstrapper

chinit replaces the cloud-init bash script: it detects the cloud it runs
on (or takes -cloud), fetches its config bundle, converges the node to
the BootSpec (chinit.yaml) and installs its own recurring jobs as
subcommands.

Usage:
  chinit boot [-spec chinit.yaml] [-cloud aws|azure] [-dry-run] [-refresh]
`, Version)
	// The maintenance/termination command list prints from the
	// dispatch registry — the usage text and the dispatch table are
	// one structure and cannot drift apart.
	for _, c := range commands {
		if c.Boot {
			continue
		}
		if c.Usage != "" {
			_, _ = fmt.Fprintf(w, "  chinit %s %s\n", c.Name, c.Usage)
		} else {
			_, _ = fmt.Fprintf(w, "  chinit %s\n", c.Name)
		}
	}
	_, _ = fmt.Fprintf(w, `  chinit version

The spec is chinit.yaml (fetched with the config bundle when booted via
cloud-init; -spec points at it for manual runs). Identity and state live
in /var/lib/chinit/state.json — subcommands reload it, never re-derive.
`)
}

// bootFlags is the `boot` flag surface.
type bootFlags struct {
	specPath  string
	cloud     string
	dryRun    bool
	refresh   bool
	statePath string
}

func parseBootFlags(args []string) (*bootFlags, error) {
	fs := flag.NewFlagSet("chinit boot", flag.ContinueOnError)
	f := &bootFlags{}
	fs.StringVar(&f.specPath, "spec", "/var/lib/clickhouse-bootstrap/chinit.yaml", "path to chinit.yaml")
	fs.StringVar(&f.cloud, "cloud", "", "cloud provider override (aws|azure); empty = detect")
	fs.BoolVar(&f.dryRun, "dry-run", false, "validate the spec and print the plan without acting")
	fs.BoolVar(&f.refresh, "refresh", false, "in-place refresh of a converged node: re-execute every step against the live node (never re-claims identity, never wipes keeper raft state)")
	fs.StringVar(&f.statePath, "state", "/var/lib/chinit/state.json", "state file path")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	return f, nil
}
