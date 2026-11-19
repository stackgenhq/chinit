// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
)

var _ = Describe("configureTLS clickhouse-client CA drop-in", func() {
	It("generates self-signed certs with a non-empty CN and installs chinit-ca.xml", func() {
		root := GinkgoT().TempDir()
		sslDir := filepath.Join(root, "ssl")
		clientCfgDir := filepath.Join(root, "client-config.d")

		origSSL, origClient := tlsCertDir, clickhouseClientConfigDir
		origRun := runCommandFn
		tlsCertDir = func() string { return sslDir }
		clickhouseClientConfigDir = func() string { return clientCfgDir }
		var seenArgs []string
		runCommandFn = func(_ context.Context, name string, args ...string) (string, error) {
			if name == "openssl" {
				seenArgs = append([]string(nil), args...)
				for i := 0; i < len(args)-1; i++ {
					if args[i] == "-out" {
						_ = os.WriteFile(args[i+1], []byte("CERT"), 0o644)
					}
				}
			}
			return "", nil
		}
		DeferCleanup(func() {
			tlsCertDir = origSSL
			clickhouseClientConfigDir = origClient
			runCommandFn = origRun
		})

		b := &Boot{
			Spec:     &model.BootSpec{},
			Identity: model.NodeIdentity{Hostname: ""},
		}
		Expect(b.configureTLS(context.Background())).To(Succeed())

		Expect(seenArgs).To(ContainElement(MatchRegexp(`^/CN=.+`)))
		caCfg, err := os.ReadFile(filepath.Join(clientCfgDir, "chinit-ca.xml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(caCfg)).To(ContainSubstring("<caConfig>" + filepath.Join(sslDir, "ca.crt") + "</caConfig>"))
		Expect(string(caCfg)).To(ContainSubstring("<loadDefaultCAFile>true</loadDefaultCAFile>"))
	})

	It("still installs chinit-ca.xml on idempotent reboots when server.crt already exists", func() {
		root := GinkgoT().TempDir()
		sslDir := filepath.Join(root, "ssl")
		clientCfgDir := filepath.Join(root, "client-config.d")
		Expect(os.MkdirAll(sslDir, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(sslDir, "server.crt"), []byte("EXISTING"), 0o644)).To(Succeed())

		origSSL, origClient := tlsCertDir, clickhouseClientConfigDir
		tlsCertDir = func() string { return sslDir }
		clickhouseClientConfigDir = func() string { return clientCfgDir }
		DeferCleanup(func() {
			tlsCertDir = origSSL
			clickhouseClientConfigDir = origClient
		})

		b := &Boot{Spec: &model.BootSpec{}}
		Expect(b.configureTLS(context.Background())).To(Succeed())

		caCfg, err := os.ReadFile(filepath.Join(clientCfgDir, "chinit-ca.xml"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(caCfg)).To(ContainSubstring("<caConfig>" + filepath.Join(sslDir, "ca.crt") + "</caConfig>"))
	})
})
