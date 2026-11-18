// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fakes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// SecretSource is a canned in-memory SecretSource.
type SecretSource struct {
	// Values maps secret name to value.
	Values map[string]string
}

// GetSecretValue serves the canned value; unknown names error loudly
// (mirrors the real stores' behavior — a typo'd secret name must fail
// the boot, not write an empty password).
func (s *SecretSource) GetSecretValue(_ context.Context, request port.SecretRequest) (string, error) {
	value, ok := s.Values[request.Name]
	if !ok {
		return "", fmt.Errorf("secret %q not found in fake", request.Name)
	}
	return value, nil
}

// BundleSource is a canned config bundle: writes fixed files.
type BundleSource struct {
	// Files maps bundle member names to content.
	Files map[string]string
}

// FetchBundle writes the canned members into destDir and lists them.
func (b *BundleSource) FetchBundle(_ context.Context, _ model.ConfigBundle, destDir string) ([]string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, err
	}
	var written []string
	for name, content := range b.Files {
		dest := filepath.Join(destDir, name)
		if err := os.WriteFile(dest, []byte(content), 0644); err != nil {
			return written, err
		}
		written = append(written, dest)
	}
	return written, nil
}
