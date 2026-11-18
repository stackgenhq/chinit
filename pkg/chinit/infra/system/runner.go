// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner executes external commands. Production is ExecRunner (real
// os/exec); tests inject a recording fake with scripted responses.
type Runner interface {
	// Run executes cmd with args and returns combined output. A
	// non-zero exit is an error carrying the output.
	Run(ctx context.Context, cmd string, args ...string) (string, error)
}

// ExecRunner is the real runner.
type ExecRunner struct{}

// Run runs the command, capturing combined stdout+stderr.
func (ExecRunner) Run(ctx context.Context, cmd string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, cmd, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", cmd, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// CommandError captures a failed command with its output for the boot
// log (the bash exit_if_error context, structured).
type CommandError struct {
	Cmd    string
	Output string
	Err    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("%s failed: %s: %s", e.Cmd, e.Err, strings.TrimSpace(e.Output))
}

func (e *CommandError) Unwrap() error { return e.Err }

// LookPath reports whether a binary exists (the golden-AMI require step).
func LookPath(bin string) (string, error) { return exec.LookPath(bin) }
