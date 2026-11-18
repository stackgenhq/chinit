// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

// This file pins the Blob bundle fetch: the Terraform bundle layout
// (the scripts-container keys the module uploads) and the SHA256 gate,
// whose digest MUST equal Terraform's config_files_hash — the same
// contract the AWS fetcher's parity test pins. It runs under the
// package's single Ginkgo suite.

// blobKeys are the canned blobs, keyed by the container/name path the
// fetcher requests (container-relative, same as the S3 prefix keys).
type blobHTTP struct {
	blobs map[string]string
}

func (f *blobHTTP) Do(req *http.Request) (*http.Response, error) {
	// The fetch URL is https://<account>.blob.core.windows.net/<container>/<key>:
	// the path's first segment is the container.
	parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/"), "/", 2)
	if len(parts) != 2 {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	body, ok := f.blobs[parts[0]+"/"+parts[1]]
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
}

// blobBundleKeys mirrors the AWS test's bundleKeys: the hashed members
// (the three static XMLs + the DDL + chi.yaml).
func blobBundleKeys() map[string]string {
	return map[string]string{
		"scripts/configs/network.xml":       "<network/>",
		"scripts/configs/ssl.xml":           "<ssl/>",
		"scripts/configs/storage.xml":       "<storage/>",
		"scripts/configs/bootstrap-ddl.sql": "-- ddl",
		"scripts/chi.yaml":                  "chi: spec",
	}
}

// fetchBlob runs one Blob FetchBundle against the canned blobs.
func fetchBlob(blobs map[string]string, bundle model.ConfigBundle) ([]string, error) {
	src := &BlobBundleSource{
		client: &blobHTTP{blobs: blobs},
		TokenSource: func(context.Context, string) (string, error) {
			return "token", nil
		},
	}
	return src.FetchBundle(context.Background(), bundle, GinkgoT().TempDir())
}

var _ = Describe("BlobBundleSource.FetchBundle", func() {
	Context("the SHA256 gate", func() {
		It("accepts Terraform's own config_files_hash and refuses a one-bit content change", func() {
			// The digest MUST equal Terraform's
			// sha256(jsonencode(merge(config_files_all, {"chi.yaml": spec}))) —
			// computed here independently of chinit's code path. The
			// fetcher must hash the encoded pairs INTO the hasher
			// before Sum: writing the digest any other way (e.g.
			// sha256.New().Sum(enc), which never digests the content)
			// refuses every correct bundle.
			net := "<network>0.0.0.0</network>"
			ssl := "<ssl>\n<cert>/etc/clickhouse-server/ssl/server.crt</cert>\n</ssl>"
			stor := "<storage><disk>azure_blob</disk></storage>"
			ddl := "-- ddl"
			chi := "apiVersion: clickhouse.altinity.com/v1\nkind: ClickHouseInstallation\n"

			merge := map[string]string{
				"configs/network.xml":       net,
				"configs/ssl.xml":           ssl,
				"configs/storage.xml":       stor,
				"configs/bootstrap-ddl.sql": ddl,
				"chi.yaml":                  chi,
			}
			enc, err := json.Marshal(merge) // jsonencode: sorted pairs, escaped values
			Expect(err).NotTo(HaveOccurred())
			want := sha256.Sum256(enc)
			wantHex := hex.EncodeToString(want[:])

			blobs := map[string]string{
				"scripts/configs/network.xml":       net,
				"scripts/configs/ssl.xml":           ssl,
				"scripts/configs/storage.xml":       stor,
				"scripts/configs/bootstrap-ddl.sql": ddl,
				"scripts/chi.yaml":                  chi,
			}
			written, err := fetchBlob(blobs, model.ConfigBundle{
				Bucket: "storageacct", Prefix: "scripts", SHA256: wantHex,
			})
			Expect(err).NotTo(HaveOccurred(), "the gate MUST accept Terraform's own digest; got: %v", err)
			Expect(written).To(HaveLen(5))

			// A one-bit content change must be refused.
			blobs["scripts/configs/storage.xml"] = stor + "\n<!-- edited -->"
			_, err = fetchBlob(blobs, model.ConfigBundle{
				Bucket: "storageacct", Prefix: "scripts", SHA256: wantHex,
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("SHA256 mismatch"))
		})

		It("excludes keeper.yaml from the digest (Terraform's hash does not cover it)", func() {
			merge := map[string]string{
				"configs/network.xml":       "<network/>",
				"configs/ssl.xml":           "<ssl/>",
				"configs/storage.xml":       "<storage/>",
				"configs/bootstrap-ddl.sql": "-- ddl",
				"chi.yaml":                  "chi: spec",
			}
			enc, err := json.Marshal(merge)
			Expect(err).NotTo(HaveOccurred())
			want := sha256.Sum256(enc)
			wantHex := hex.EncodeToString(want[:])

			blobs := blobBundleKeys()
			blobs["scripts/keeper.yaml"] = "keeper: spec"
			_, err = fetchBlob(blobs, model.ConfigBundle{
				Bucket: "storageacct", Prefix: "scripts", SHA256: wantHex,
			})
			Expect(err).NotTo(HaveOccurred(), "keeper.yaml presence must not affect the gate")
		})
	})
})
