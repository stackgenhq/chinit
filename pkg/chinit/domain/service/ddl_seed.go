// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/logger"
)

// DDLSeedSource locates and reads the optional cold-start DDL file and admin password.
type DDLSeedSource interface {
	DDLPath() string
	ReadDDL() (string, error)
	ReadAdminPassword() (string, error)
}

// SQLFileExecutor executes a SQL file directly via ClickHouse.
type SQLFileExecutor interface {
	ExecFile(ctx context.Context, adminPassword, filePath string) (string, error)
}

// DDLSeedService executes an optional bundle DDL file directly through
// clickhouse-client for initial cold-start table creation without parsing SQL.
type DDLSeedService struct {
	source   DDLSeedSource
	executor SQLFileExecutor
}

// NewDDLSeedService constructs a DDLSeedService wired to the local bundle and clickhouse-client.
func NewDDLSeedService() *DDLSeedService {
	return NewDDLSeedServiceWith(localDDLSeedSource{}, localSQLFileExecutor{})
}

// NewDDLSeedServiceWith constructs a DDLSeedService with explicit dependencies.
func NewDDLSeedServiceWith(source DDLSeedSource, executor SQLFileExecutor) *DDLSeedService {
	return &DDLSeedService{source: source, executor: executor}
}

// Apply runs the bundle's bootstrap-ddl.sql file when present and non-empty.
// Missing or comment-only files are treated as a no-op.
func (s *DDLSeedService) Apply(ctx context.Context) error {
	ddlPath := s.source.DDLPath()
	raw, err := s.source.ReadDDL()
	if err != nil {
		logger.GetLogger(ctx).Info("bootstrap-ddl.sql not present; skipping cold-start DDL seed", "path", ddlPath)
		return nil
	}
	if !s.hasExecutableSQL(raw) {
		logger.GetLogger(ctx).Info("bootstrap-ddl.sql contains no statements; skipping cold-start DDL seed", "path", ddlPath)
		return nil
	}
	adminPw, err := s.source.ReadAdminPassword()
	if err != nil {
		return fmt.Errorf("read admin password file: %w", err)
	}
	if out, err := s.executor.ExecFile(ctx, adminPw, ddlPath); err != nil {
		return fmt.Errorf("execute %s: %w: %s", ddlPath, err, strings.TrimSpace(out))
	}
	logger.GetLogger(ctx).Info("applied cold-start bootstrap DDL", "path", ddlPath)
	return nil
}

func (s *DDLSeedService) hasExecutableSQL(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		return true
	}
	return false
}

type localDDLSeedSource struct{}

func (localDDLSeedSource) DDLPath() string {
	return filepath.Join(BootstrapDir, "bootstrap-ddl.sql")
}

func (localDDLSeedSource) ReadDDL() (string, error) {
	data, err := os.ReadFile(filepath.Join(BootstrapDir, "bootstrap-ddl.sql"))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (localDDLSeedSource) ReadAdminPassword() (string, error) {
	pw, err := os.ReadFile(bootstrapAdminPasswordPath())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(pw)), nil
}

type localSQLFileExecutor struct{}

func (localSQLFileExecutor) ExecFile(ctx context.Context, adminPassword, filePath string) (string, error) {
	cmd := exec.CommandContext(ctx, "clickhouse-client", "--queries-file", filePath)
	if adminPassword != "" {
		cmd.Env = append(os.Environ(), "CLICKHOUSE_USER=admin", "CLICKHOUSE_PASSWORD="+adminPassword)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}
