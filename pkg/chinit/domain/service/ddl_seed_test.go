// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type stubDDLSeedSource struct {
	path     string
	ddl      string
	ddlErr   error
	password string
	pwErr    error
}

func (s stubDDLSeedSource) DDLPath() string                    { return s.path }
func (s stubDDLSeedSource) ReadDDL() (string, error)           { return s.ddl, s.ddlErr }
func (s stubDDLSeedSource) ReadAdminPassword() (string, error) { return s.password, s.pwErr }

type recordingSQLFileExecutor struct {
	calls []string
	err   error
}

func (r *recordingSQLFileExecutor) ExecFile(_ context.Context, _, filePath string) (string, error) {
	r.calls = append(r.calls, filePath)
	if r.err != nil {
		return "syntax error", r.err
	}
	return "", nil
}

var _ = Describe("DDLSeedService", func() {
	It("no-ops when bootstrap-ddl.sql is absent", func() {
		exec := &recordingSQLFileExecutor{}
		svc := NewDDLSeedServiceWith(stubDDLSeedSource{
			path:   "/tmp/bootstrap-ddl.sql",
			ddlErr: os.ErrNotExist,
		}, exec)

		Expect(svc.Apply(context.Background())).To(Succeed())
		Expect(exec.calls).To(BeEmpty())
	})

	It("no-ops when bootstrap-ddl.sql has only comments and whitespace", func() {
		exec := &recordingSQLFileExecutor{}
		svc := NewDDLSeedServiceWith(stubDDLSeedSource{
			path: "/tmp/bootstrap-ddl.sql",
			ddl:  "-- header comment\n\n  -- trailing comment\n",
		}, exec)

		Expect(svc.Apply(context.Background())).To(Succeed())
		Expect(exec.calls).To(BeEmpty())
	})

	It("passes non-empty bootstrap-ddl.sql directly to SQLFileExecutor", func() {
		exec := &recordingSQLFileExecutor{}
		svc := NewDDLSeedServiceWith(stubDDLSeedSource{
			path:     "/tmp/bootstrap-ddl.sql",
			ddl:      "-- cold-start table\nCREATE TABLE IF NOT EXISTS default.events (id String) ENGINE = MergeTree ORDER BY id;\n",
			password: "secret",
		}, exec)

		Expect(svc.Apply(context.Background())).To(Succeed())
		Expect(exec.calls).To(Equal([]string{"/tmp/bootstrap-ddl.sql"}))
	})

	It("surfaces executor failures", func() {
		exec := &recordingSQLFileExecutor{err: errors.New("exit status 1")}
		svc := NewDDLSeedServiceWith(stubDDLSeedSource{
			path:     "/tmp/bootstrap-ddl.sql",
			ddl:      "CREATE TABLE IF NOT EXISTS default.events (id String);",
			password: "secret",
		}, exec)

		Expect(svc.Apply(context.Background())).To(HaveOccurred())
	})
})
