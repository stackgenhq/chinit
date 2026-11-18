// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// levelSplitHandler routes records by severity: below Warn goes to
// the stdout handler, Warn and above to the stderr handler — the
// journal split the human-readable boot log always had (progress on
// stdout, the operator's WARNINGs on stderr). Both sides are JSON
// handlers; the process-wide levelVar is the only level filter, so
// SetLogLevel governs both streams with one call.
type levelSplitHandler struct {
	info slog.Handler
	warn slog.Handler
}

// NewLevelSplitHandler builds the routing handler over two underlying
// JSON handlers (info → out, warn+ → err), both at LevelDebug so the
// route decision and the levelVar are the only filters.
func NewLevelSplitHandler(out, err io.Writer) slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	return &levelSplitHandler{
		info: slog.NewJSONHandler(out, opts),
		warn: slog.NewJSONHandler(err, opts),
	}
}

func (h *levelSplitHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= levelVar.Level()
}

func (h *levelSplitHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		return h.warn.Handle(ctx, r)
	}
	return h.info.Handle(ctx, r)
}

func (h *levelSplitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelSplitHandler{info: h.info.WithAttrs(attrs), warn: h.warn.WithAttrs(attrs)}
}

func (h *levelSplitHandler) WithGroup(name string) slog.Handler {
	return &levelSplitHandler{info: h.info.WithGroup(name), warn: h.warn.WithGroup(name)}
}

// stdoutWriter/stderrWriter indirect through the os.Stdout/os.Stderr
// VARIABLES (not the values captured at init): the suites capture
// output by swapping those vars, and the JSON records must land in
// the captured file the same way the raw report lines do.
type stdoutWriter struct{}
type stderrWriter struct{}

func (stdoutWriter) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stderrWriter) Write(p []byte) (int, error) { return os.Stderr.Write(p) }
