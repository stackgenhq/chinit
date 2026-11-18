// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package service_test

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/service"
)

// The storage-loss detector's contract: the re-image/replacement case
// (empty data dir + stale Keeper registrations) is recognized and the
// fresh-node and healthy cases are not.
var _ = Describe("DetectStorageLoss", func() {
	var dir string

	BeforeEach(func() {
		dir = GinkgoT().TempDir()
	})

	writeParts := func() {
		partDir := filepath.Join(dir, "store", "db", "events", "data_all_1_1_1")
		Expect(os.MkdirAll(partDir, 0755)).To(Succeed())
	}

	It("flags an empty data dir with existing registrations (the re-image case)", func() {
		v, err := service.DetectStorageLoss(dir, []string{"writer-0"})
		Expect(err).NotTo(HaveOccurred())
		Expect(v.StorageLoss).To(BeTrue())
		Expect(v.Reason).To(ContainSubstring("storage loss"))
	})

	It("does not flag a fresh node (empty dir, no registrations)", func() {
		v, err := service.DetectStorageLoss(dir, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.StorageLoss).To(BeFalse(), "a first boot is the normal case, never a loss verdict")
	})

	It("does not flag a healthy volume (parts present)", func() {
		writeParts()
		v, err := service.DetectStorageLoss(dir, []string{"writer-0"})
		Expect(err).NotTo(HaveOccurred())
		Expect(v.StorageLoss).To(BeFalse())
		Expect(v.Reason).To(ContainSubstring("healthy"))
	})

	It("does not flag the re-attach case (parts present, no registrations)", func() {
		writeParts()
		v, err := service.DetectStorageLoss(dir, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(v.StorageLoss).To(BeFalse(), "the replica-sync/restore path heals this, not the detector")
	})

	It("errors on an unstatable directory (a mount bug, not a verdict)", func() {
		_, err := service.DetectStorageLoss(filepath.Join(dir, "nonexistent"), nil)
		Expect(err).To(HaveOccurred())
	})

	It("errors on an empty path (a wiring bug)", func() {
		_, err := service.DetectStorageLoss("", nil)
		Expect(err).To(HaveOccurred())
	})

	It("is not fooled by detached or check directories (only data_* parts count)", func() {
		Expect(os.MkdirAll(filepath.Join(dir, "store", "db", "events", "detached"), 0755)).To(Succeed())
		v, err := service.DetectStorageLoss(dir, []string{"writer-0"})
		Expect(err).NotTo(HaveOccurred())
		Expect(v.StorageLoss).To(BeTrue(), "detached dirs carry no serving parts — the loss verdict stands")
	})
})
