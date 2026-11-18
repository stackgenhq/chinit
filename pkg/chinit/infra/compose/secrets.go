// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

const defaultSecretsDir = "/etc/chinit/secrets"

// FileSecretSource resolves secret values from mounted files on disk.
type FileSecretSource struct {
	dir string
}

// NewFileSecretSource constructs a secret source rooted at CHINIT_COMPOSE_SECRETS_DIR
// or /etc/chinit/secrets.
func NewFileSecretSource() *FileSecretSource {
	return &FileSecretSource{
		dir: firstNonEmpty(os.Getenv("CHINIT_COMPOSE_SECRETS_DIR"), defaultSecretsDir),
	}
}

// WithDir overrides the secrets directory (tests).
func (s *FileSecretSource) WithDir(dir string) *FileSecretSource {
	s.dir = dir
	return s
}

// GetSecretValue reads a secret file by name, optionally extracting a
// JSON field when req.Field is set.
func (s *FileSecretSource) GetSecretValue(_ context.Context, req port.SecretRequest) (string, error) {
	if strings.TrimSpace(req.Name) == "" {
		return "", fmt.Errorf("compose secrets: empty secret name")
	}
	cleanName := filepath.Clean(req.Name)
	if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("compose secrets: invalid secret path %q", req.Name)
	}
	candidates := []string{
		filepath.Join(s.dir, cleanName),
	}
	if base := filepath.Base(cleanName); base != cleanName {
		candidates = append(candidates, filepath.Join(s.dir, base))
	}

	var raw []byte
	var readErr error
	for _, p := range candidates {
		raw, readErr = os.ReadFile(p)
		if readErr == nil {
			break
		}
	}
	if readErr != nil {
		return "", fmt.Errorf("compose secrets: read %q in %s: %w", req.Name, s.dir, readErr)
	}

	val := strings.TrimSpace(string(raw))
	if req.Field != "" {
		var obj map[string]any
		if err := json.Unmarshal([]byte(val), &obj); err != nil {
			return "", fmt.Errorf("compose secrets: secret %q is not JSON for field %q: %w", req.Name, req.Field, err)
		}
		rawField, ok := obj[req.Field]
		if !ok {
			return "", fmt.Errorf("compose secrets: secret %q missing field %q", req.Name, req.Field)
		}
		val = strings.TrimSpace(fmt.Sprint(rawField))
	}
	if val == "" {
		return "", fmt.Errorf("compose secrets: secret %q is empty", req.Name)
	}
	return val, nil
}
