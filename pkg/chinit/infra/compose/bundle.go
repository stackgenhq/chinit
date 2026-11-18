// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	chinitsvc "github.com/stackgenhq/chinit/pkg/chinit/domain/fail"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

const defaultBundleDir = "/etc/chinit/bundle"

type bundleMember struct {
	relPaths []string // candidate relative paths inside source bundle dir
	destName string   // filename written in destDir
	hashKey  string   // Terraform/S3 bundle digest key
	hashed   bool
}

var composeBundleMembers = []bundleMember{
	{relPaths: []string{"configs/network.xml", "network.xml"}, destName: "network.xml", hashKey: "configs/network.xml", hashed: true},
	{relPaths: []string{"configs/ssl.xml", "ssl.xml"}, destName: "ssl.xml", hashKey: "configs/ssl.xml", hashed: true},
	{relPaths: []string{"configs/storage.xml", "storage.xml"}, destName: "storage.xml", hashKey: "configs/storage.xml", hashed: true},
	{relPaths: []string{"configs/bootstrap-ddl.sql", "bootstrap-ddl.sql"}, destName: "bootstrap-ddl.sql", hashKey: "configs/bootstrap-ddl.sql", hashed: true},
	{relPaths: []string{"chi.yaml"}, destName: "chi.yaml", hashKey: "chi.yaml", hashed: true},
	{relPaths: []string{"keeper.yaml"}, destName: "keeper.yaml", hashed: false},
}

// LocalBundleSource copies the configuration bundle from a mounted directory
// and enforces the same SHA-256 verification contract as S3/Blob sources.
type LocalBundleSource struct {
	dir string
}

// NewLocalBundleSource constructs a bundle source rooted at
// CHINIT_COMPOSE_BUNDLE_DIR or /etc/chinit/bundle.
func NewLocalBundleSource() *LocalBundleSource {
	return &LocalBundleSource{
		dir: firstNonEmpty(os.Getenv("CHINIT_COMPOSE_BUNDLE_DIR"), defaultBundleDir),
	}
}

// WithDir overrides the bundle source directory (tests).
func (b *LocalBundleSource) WithDir(dir string) *LocalBundleSource {
	b.dir = dir
	return b
}

func (b *LocalBundleSource) resolveRoot(bundle model.ConfigBundle) string {
	var candidates []string
	prefix := strings.Trim(bundle.Prefix, "/")
	if bundle.Bucket != "" && prefix != "" {
		candidates = append(candidates, filepath.Join(b.dir, bundle.Bucket, prefix))
	}
	if prefix != "" {
		candidates = append(candidates, filepath.Join(b.dir, prefix))
	}
	if bundle.Bucket != "" {
		candidates = append(candidates, filepath.Join(b.dir, bundle.Bucket))
	}
	candidates = append(candidates, b.dir)

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && info.IsDir() {
			if hasAnyBundleMember(c) {
				return c
			}
		}
	}
	return b.dir
}

func hasAnyBundleMember(root string) bool {
	for _, m := range composeBundleMembers {
		for _, rel := range m.relPaths {
			if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
				return true
			}
		}
	}
	return false
}

// FetchBundle copies bundle files into destDir and verifies bundle.SHA256 when pinned.
func (b *LocalBundleSource) FetchBundle(_ context.Context, bundle model.ConfigBundle, destDir string) ([]string, error) {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, chinitsvc.Fatal(fmt.Errorf("create %s: %w", destDir, err))
	}

	root := b.resolveRoot(bundle)
	var written []string
	hashedContents := map[string]string{}

	for _, member := range composeBundleMembers {
		var body []byte
		var foundPath string
		var lastErr error
		for _, rel := range member.relPaths {
			p := filepath.Join(root, rel)
			data, err := os.ReadFile(p)
			if err == nil {
				body = data
				foundPath = p
				break
			}
			lastErr = err
		}
		if foundPath == "" {
			if errors.Is(lastErr, os.ErrNotExist) && (member.destName == "chi.yaml" || member.destName == "keeper.yaml" || member.destName == "bootstrap-ddl.sql") {
				continue
			}
			return nil, fmt.Errorf("compose bundle: missing %s under %s: %w", member.destName, root, lastErr)
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("compose bundle: %s is empty", foundPath)
		}
		dest := filepath.Join(destDir, member.destName)
		if err := os.WriteFile(dest, body, 0644); err != nil {
			return nil, chinitsvc.Fatal(fmt.Errorf("write %s: %w", dest, err))
		}
		written = append(written, dest)
		if member.hashed {
			hashedContents[member.hashKey] = string(body)
		}
	}

	if len(written) == 0 {
		return nil, fmt.Errorf("compose bundle: no bundle files found under %s", root)
	}

	if bundle.SHA256 != "" {
		raw, err := json.Marshal(hashedContents)
		if err != nil {
			return nil, chinitsvc.Fatal(fmt.Errorf("marshal bundle contents: %w", err))
		}
		sum := sha256.Sum256(raw)
		got := hex.EncodeToString(sum[:])
		if got != bundle.SHA256 {
			return nil, chinitsvc.Fatal(fmt.Errorf("bundle SHA256 mismatch: got %s, want %s", got, bundle.SHA256))
		}
	}

	return written, nil
}

// FetchRoleSpec reads a role-specific chinit.yaml from the bundle root when present.
func (b *LocalBundleSource) FetchRoleSpec(_ context.Context, req port.RoleSpecRequest) ([]byte, bool, error) {
	root := b.resolveRoot(model.ConfigBundle{Bucket: req.Bucket, Prefix: req.Prefix})
	candidates := []string{
		filepath.Join(root, fmt.Sprintf("chinit-%s.yaml", req.Role)),
		filepath.Join(root, "specs", req.Role+".yaml"),
		filepath.Join(root, req.Role, "chinit.yaml"),
		filepath.Join(root, fmt.Sprintf("chinit.%s.yaml", req.Role)),
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, true, nil
		}
	}
	return nil, false, nil
}
