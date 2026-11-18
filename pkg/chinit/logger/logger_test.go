// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// captureStream redirects the os.Stdout or os.Stderr VARIABLE into a
// file for the duration of run and returns what was written — the
// journal-capture contract the existing suites rely on (the handler's
// writers resolve the *os.File vars at record time). name keys the
// scratch file ("stdout"/"stderr").
func captureStream(v **os.File, name string, run func()) string {
	dir := filepath.Join(os.TempDir(), "chinit-logger-test")
	Expect(os.MkdirAll(dir, 0755)).To(Succeed())
	logPath := filepath.Join(dir, name+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	Expect(err).NotTo(HaveOccurred())
	orig := *v
	*v = f
	defer func() { *v = orig }()
	run()
	Expect(f.Close()).To(Succeed())
	data, err := os.ReadFile(logPath)
	Expect(err).NotTo(HaveOccurred())
	return string(data)
}

var _ = Describe("levelSplitHandler", func() {
	It("routes Info to stdout and Warn to stderr as JSON records", func() {
		var out, errOut bytes.Buffer
		log := slog.New(logger.NewLevelSplitHandler(&out, &errOut))

		log.Info("boot progress", "step", "settle_identity")
		log.Warn("degraded", "reason", "lease outage")

		Expect(out.String()).To(ContainSubstring(`"level":"INFO"`))
		Expect(out.String()).To(ContainSubstring(`"msg":"boot progress"`))
		Expect(out.String()).To(ContainSubstring(`"step":"settle_identity"`))
		Expect(out.String()).NotTo(ContainSubstring("degraded"))

		Expect(errOut.String()).To(ContainSubstring(`"level":"WARN"`))
		Expect(errOut.String()).To(ContainSubstring(`"msg":"degraded"`))
		Expect(errOut.String()).To(ContainSubstring(`"reason":"lease outage"`))
		Expect(errOut.String()).NotTo(ContainSubstring("boot progress"))
	})

	It("emits one parseable JSON object per record", func() {
		var out, errOut bytes.Buffer
		slog.New(logger.NewLevelSplitHandler(&out, &errOut)).
			Warn("multi word message", "lease_table", "node-claims", "attempt", 2)
		var rec map[string]any
		Expect(json.Unmarshal(errOut.Bytes(), &rec)).To(Succeed())
		Expect(rec["msg"]).To(Equal("multi word message"))
		Expect(rec["lease_table"]).To(Equal("node-claims"))
		Expect(rec["attempt"]).To(Equal(float64(2)))
	})

	It("filters records below the process level", func() {
		var out, errOut bytes.Buffer
		logger.SetLogLevel(slog.LevelInfo)
		DeferCleanup(func() { logger.SetLogLevel(slog.LevelInfo) })

		log := slog.New(logger.NewLevelSplitHandler(&out, &errOut))
		log.Debug("chatty internals")
		Expect(out.String()).To(BeEmpty())
		Expect(errOut.String()).To(BeEmpty())

		logger.SetLogLevel(slog.LevelDebug)
		log.Debug("now visible")
		Expect(out.String()).To(ContainSubstring("now visible"))
	})

	It("routes Error to stderr like Warn", func() {
		var out, errOut bytes.Buffer
		slog.New(logger.NewLevelSplitHandler(&out, &errOut)).Error("fatal shape")
		Expect(out.String()).NotTo(ContainSubstring("fatal shape"))
		Expect(errOut.String()).To(ContainSubstring(`"level":"ERROR"`))
	})
})

var _ = Describe("the singleton", func() {
	It("GetLogger answers a non-nil logger without any setup", func() {
		Expect(logger.GetLogger(context.TODO())).NotTo(BeNil())
		Expect(logger.GetLogger(context.TODO())).To(Equal(logger.GetLogger(context.TODO())),
			"the same instance answers every call — the central singleton")
	})

	It("SetLogHandler installs the capture seam and Init restores the real streams", func() {
		var buf bytes.Buffer
		logger.SetLogHandler(logger.NewLevelSplitHandler(&buf, &buf))
		logger.GetLogger(context.TODO()).Warn("captured through the seam")
		Expect(buf.String()).To(ContainSubstring("captured through the seam"))
		Expect(slog.Default()).To(Equal(logger.GetLogger(context.TODO())),
			"a stray package-level slog call can never bypass the singleton")

		logger.Init()
		DeferCleanup(logger.Init)
		stderr := captureStream(&os.Stderr, "stderr", func() {
			logger.GetLogger(context.TODO()).Warn("restored to the swapped handle")
		})
		Expect(stderr).To(ContainSubstring(`"restored to the swapped handle"`))
	})

	It("InitLogging's journal contract holds: stdout carries Info, stderr carries Warn", func() {
		logger.Init()
		DeferCleanup(logger.Init)

		stdout := captureStream(&os.Stdout, "stdout", func() {
			logger.GetLogger(context.TODO()).Info("progress through the swapped stdout")
		})
		stderr := captureStream(&os.Stderr, "stderr", func() {
			logger.GetLogger(context.TODO()).Warn("warning through the swapped stderr")
		})
		Expect(stdout).To(ContainSubstring("progress through the swapped stdout"))
		Expect(stderr).To(ContainSubstring("warning through the swapped stderr"))
	})

	It("the default level is Info and Debug is unlockable", func() {
		Expect(logger.LogLevel()).To(Equal(slog.LevelInfo))
		Expect(logger.IsDebugEnabled()).To(BeFalse())
		logger.SetLogLevel(slog.LevelDebug)
		DeferCleanup(func() { logger.SetLogLevel(slog.LevelInfo) })
		Expect(logger.IsDebugEnabled()).To(BeTrue())
	})
})
