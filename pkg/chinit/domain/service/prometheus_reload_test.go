// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

var _ = Describe("prometheusReload", func() {
	var (
		dir          string
		promFile     string
		origPromPath func() string
		origPwPath   func() string
		origExec     func(context.Context, string, string) (string, error)
		origRunCmd   func(context.Context, string, ...string) (string, error)
		sqlCalls     []string
		sqlPasswords []string
		cmdCalls     []string
	)

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
		promFile = filepath.Join(dir, "server-prometheus.xml")
		Expect(os.WriteFile(promFile, []byte("<clickhouse/>"), 0644)).To(Succeed())
		origPromPath = serverPrometheusPath
		serverPrometheusPath = func() string { return promFile }

		pwFile := filepath.Join(dir, "admin-password")
		Expect(os.WriteFile(pwFile, []byte("test-admin-secret\n"), 0600)).To(Succeed())
		origPwPath = bootstrapAdminPasswordPath
		bootstrapAdminPasswordPath = func() string { return pwFile }

		sqlCalls = nil
		sqlPasswords = nil
		cmdCalls = nil

		origExec = execClientFn
		execClientFn = func(_ context.Context, pw, query string) (string, error) {
			sqlCalls = append(sqlCalls, query)
			sqlPasswords = append(sqlPasswords, pw)
			return "", nil
		}

		origRunCmd = runCommandFn
		runCommandFn = func(_ context.Context, name string, args ...string) (string, error) {
			cmdCalls = append(cmdCalls, name+" "+strings.Join(args, " "))
			return "", nil
		}

		DeferCleanup(func() {
			serverPrometheusPath = origPromPath
			bootstrapAdminPasswordPath = origPwPath
			execClientFn = origExec
			runCommandFn = origRunCmd
		})
	})

	It("authenticates SYSTEM RELOAD CONFIG as admin and does not restart clickhouse-server when reload succeeds", func() {
		b := &Boot{Spec: &model.BootSpec{Role: model.RoleReader}}
		Expect(b.prometheusReload(context.Background())).To(Succeed())

		Expect(sqlCalls).To(Equal([]string{"SYSTEM RELOAD CONFIG"}))
		Expect(sqlPasswords).To(Equal([]string{"test-admin-secret"}))
		Expect(cmdCalls).To(ContainElement("curl -s -o /dev/null http://localhost:9363/metrics"))
		Expect(cmdCalls).NotTo(ContainElement("systemctl restart clickhouse-server"))
	})

	It("falls back to systemctl restart clickhouse-server only when SYSTEM RELOAD CONFIG fails", func() {
		execClientFn = func(_ context.Context, pw, query string) (string, error) {
			sqlCalls = append(sqlCalls, query)
			sqlPasswords = append(sqlPasswords, pw)
			return "Code: 516", fmt.Errorf("reload failed")
		}

		b := &Boot{Spec: &model.BootSpec{Role: model.RoleWriter}}
		Expect(b.prometheusReload(context.Background())).To(Succeed())

		Expect(sqlCalls).To(Equal([]string{"SYSTEM RELOAD CONFIG"}))
		Expect(cmdCalls).To(ContainElement("systemctl restart clickhouse-server"))
		Expect(cmdCalls).To(ContainElement("curl -s -o /dev/null http://localhost:9363/metrics"))
	})

	It("skips keeper nodes without probing config or running commands", func() {
		Expect(os.Remove(promFile)).To(Succeed())
		b := &Boot{Spec: &model.BootSpec{Role: model.RoleKeeper}}
		Expect(b.prometheusReload(context.Background())).To(Succeed())
		Expect(sqlCalls).To(BeEmpty())
		Expect(cmdCalls).To(BeEmpty())
	})
})
