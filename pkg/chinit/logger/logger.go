// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package logger

import (
	"context"
	"log/slog"
)

var (
	singleton *slog.Logger
	levelVar  slog.LevelVar
)

func init() {
	// The zero LevelVar already reads Info; the explicit Set documents
	// the operator-facing default (Debug records stay out of the
	// journal until SetLogLevel unlocks them).
	levelVar.Set(slog.LevelInfo)
	Init()
}

// Init installs the process-wide singleton over the REAL stdout and
// stderr (JSON records, level-split). Idempotent and re-callable:
// Main installs it at entry, and specs re-call it to restore the
// default after SetLogHandler swaps in a capture buffer.
func Init() {
	SetLogHandler(NewLevelSplitHandler(stdoutWriter{}, stderrWriter{}))
}

// SetLogHandler replaces the singleton's handler — the one
// configuration seam. The new logger also becomes slog.Default so a
// stray package-level slog call can never bypass the singleton.
func SetLogHandler(h slog.Handler) {
	singleton = slog.New(h)
	slog.SetDefault(singleton)
}

// GetLogger returns the central singleton logger. ctx is the reserved
// correlation seam (per-context attributes ride it the day chinit
// grows request-scoped logging); nil is accepted and reads as
// context.Background(). The singleton is never nil: package init
// installs the default handler.
func GetLogger(ctx context.Context) *slog.Logger {
	return singleton
}

// SetLogLevel sets the process-wide minimum record level (Debug
// unlocks the chatty internals the Info default keeps out of the
// journal).
func SetLogLevel(level slog.Level) {
	levelVar.Set(level)
}

// LogLevel reports the process-wide minimum record level.
func LogLevel() slog.Level {
	return levelVar.Level()
}

// IsDebugEnabled reports whether Debug records pass the filter.
func IsDebugEnabled() bool {
	return levelVar.Level() == slog.LevelDebug
}
