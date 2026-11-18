// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package system

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs pin ValidateConfigSemantics — the startup-fatal settings
// placement classes the convergence audit found in SHIPPED
// config templates (backup-0 crash-looped through three of them:
// Code 137, Code 115 inside merge_tree, and the floor>pool Code 36
// class). The server's own startup check is the last line of defense;
// this validator is the FIRST — a broken template fails the boot's
// install step ONCE with a pointed error instead of a crash loop.
var _ = Describe("ValidateConfigSemantics", func() {
	var dir string

	write := func(content string) string {
		path := filepath.Join(dir, "fragment.xml")
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())
		return path
	}

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "chinit-config-validate-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(dir) })
	})

	It("accepts a legal server fragment", func() {
		path := write(`<clickhouse>
    <merge_tree>
        <always_fetch_merged_part>1</always_fetch_merged_part>
    </merge_tree>
    <background_pool_size>2</background_pool_size>
</clickhouse>
`)
		Expect(ValidateXML(path)).To(Succeed())
	})

	It("rejects a user-level setting at fragment top level (Code 137 class)", func() {
		// The backup-0 crash-loop round one: <max_threads> at the top
		// of backup-replica-settings.xml — restart counter 67.
		path := write(`<clickhouse>
    <merge_tree>
        <always_fetch_merged_part>1</always_fetch_merged_part>
    </merge_tree>
    <max_threads>4</max_threads>
</clickhouse>
`)
		err := ValidateXML(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("user-level setting"))
		Expect(err.Error()).To(ContainSubstring("max_threads"))
		Expect(err.Error()).To(ContainSubstring("Code 137"))
	})

	It("rejects a server-level pool setting inside merge_tree (Code 115 class)", func() {
		// The backup-0 crash-loop round two: background_pool_size
		// inside <merge_tree>.
		path := write(`<clickhouse>
    <merge_tree>
        <background_pool_size>2</background_pool_size>
    </merge_tree>
</clickhouse>
`)
		err := ValidateXML(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("SERVER-level setting"))
		Expect(err.Error()).To(ContainSubstring("background_pool_size"))
		Expect(err.Error()).To(ContainSubstring("Code 115"))
	})

	It("rejects a merge-tree-scoped setting at fragment top level (Code 115 mirror)", func() {
		// always_fetch_merged_part OUTSIDE <merge_tree>: 26.x rejects
		// it at startup with the same UNKNOWN_SETTING class.
		path := write(`<clickhouse>
    <always_fetch_merged_part>1</always_fetch_merged_part>
</clickhouse>
`)
		err := ValidateXML(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("inside <merge_tree>"))
		Expect(err.Error()).To(ContainSubstring("always_fetch_merged_part"))
	})

	It("accepts user-level settings inside a profiles tree (users.d shape)", func() {
		path := write(`<clickhouse>
    <profiles>
        <default>
            <max_threads>4</max_threads>
        </default>
    </profiles>
</clickhouse>
`)
		Expect(ValidateXML(path)).To(Succeed())
	})

	It("rejects the free-entry-threshold family at fragment top level", func() {
		// merge-tree-scoped settings belong in <merge_tree>; at the
		// top level they are Code 115 (the backup-0 round-three class
		// preceded the floor>pool fix).
		path := write(`<clickhouse>
    <number_of_free_entries_in_pool_to_execute_mutation>1</number_of_free_entries_in_pool_to_execute_mutation>
</clickhouse>
`)
		err := ValidateXML(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("inside <merge_tree>"))
	})
})

var _ = Describe("ValidateConfigSemantics pool thresholds (Code 36 class)", func() {
	var dir string

	write := func(content string) string {
		path := filepath.Join(dir, "fragment.xml")
		Expect(os.WriteFile(path, []byte(content), 0644)).To(Succeed())
		return path
	}

	BeforeEach(func() {
		var err error
		dir, err = os.MkdirTemp("", "chinit-config-pool-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(dir) })
	})

	It("accepts thresholds scaled under a reduced pool", func() {
		// The FIXED backup overlay shape: pool 2, thresholds 1.
		path := write(`<clickhouse>
    <merge_tree>
        <always_fetch_merged_part>1</always_fetch_merged_part>
        <number_of_free_entries_in_pool_to_execute_mutation>1</number_of_free_entries_in_pool_to_execute_mutation>
    </merge_tree>
    <background_pool_size>2</background_pool_size>
</clickhouse>
`)
		Expect(ValidateXML(path)).To(Succeed())
	})

	It("rejects a default threshold above a reduced pool (the crash-loop shape)", func() {
		// backup-0 round three EXACTLY: the pool dropped to 2 while
		// the thresholds kept their defaults (20) — ClickHouse 26.8
		// refuses to start with Code 36.
		path := write(`<clickhouse>
    <merge_tree>
        <number_of_free_entries_in_pool_to_execute_mutation>20</number_of_free_entries_in_pool_to_execute_mutation>
    </merge_tree>
    <background_pool_size>2</background_pool_size>
</clickhouse>
`)
		err := ValidateXML(path)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("number_of_free_entries_in_pool_to_execute_mutation"))
		Expect(err.Error()).To(ContainSubstring("Code 36"))
		Expect(err.Error()).To(ContainSubstring("background pool size"))
	})

	It("skips the cross-check when only one side is visible", func() {
		// Thresholds alone (the pool lives in another fragment or
		// config.xml): nothing to cross in THIS fragment.
		path := write(`<clickhouse>
    <merge_tree>
        <number_of_free_entries_in_pool_to_execute_mutation>20</number_of_free_entries_in_pool_to_execute_mutation>
    </merge_tree>
</clickhouse>
`)
		Expect(ValidateXML(path)).To(Succeed())
	})

	It("skips the cross-check when only the pool is visible", func() {
		path := write(`<clickhouse>
    <background_pool_size>2</background_pool_size>
</clickhouse>
`)
		Expect(ValidateXML(path)).To(Succeed())
	})
})

var _ = Describe("defaultBackupOverlayXML (the built-in fallback)", func() {
	It("passes the semantic validator it enforces on bundle copies", func() {
		// The built-in overlay must satisfy every placement rule:
		// pools at top level (Code 115), no user-level settings
		// (Code 137), thresholds under the pool (Code 36). If a future
		// edit breaks any of them, this spec fails BEFORE the overlay
		// ships in an AMI.
		dir, err := os.MkdirTemp("", "chinit-overlay-*")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(dir) })
		path := filepath.Join(dir, "backup-replica-settings.xml")
		Expect(os.WriteFile(path, []byte(defaultBackupOverlayXML()), 0644)).To(Succeed())
		Expect(ValidateXML(path)).To(Succeed())
	})

	It("carries the backup role's defining settings", func() {
		overlay := defaultBackupOverlayXML()
		Expect(overlay).To(ContainSubstring("<always_fetch_merged_part>1</always_fetch_merged_part>"))
		Expect(overlay).To(ContainSubstring("<replicated_can_become_leader>0</replicated_can_become_leader>"))
		Expect(overlay).To(ContainSubstring("<background_pool_size>2</background_pool_size>"))
		// The crash-loop trilogy must NOT be in the shape: no
		// user-level settings, no pools inside <merge_tree>.
		Expect(overlay).NotTo(ContainSubstring("max_threads"))
	})

	It("keeps the fetch pool at the ClickHouse default", func() {
		// background_fetches_pool_size sets both the thread count and
		// the max concurrent task count of the fetch executor, so
		// lowering it throttles the backup's replication convergence
		// behind the fleet. The overlay tunes everything EXCEPT the
		// fetch pool — this spec pins that it stays at the upstream
		// default and never regresses to a starving value.
		overlay := defaultBackupOverlayXML()
		Expect(overlay).To(ContainSubstring("<background_fetches_pool_size>16</background_fetches_pool_size>"))
		Expect(overlay).NotTo(ContainSubstring("<background_fetches_pool_size>4</background_fetches_pool_size>"))
	})
})

var _ = Describe("HardwareSizingOverlayXML & HardwareProfilesOverlayXML", func() {
	DescribeTable("renders valid, pool-consistent XML across instance shapes",
		func(vcpus int, memGiB uint64, wantPool string, wantFree string) {
			dir, err := os.MkdirTemp("", "chinit-hw-overlay-*")
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(func() { _ = os.RemoveAll(dir) })

			hwXML := HardwareSizingOverlayXML(vcpus, memGiB<<30)
			hwPath := filepath.Join(dir, "z_hardware-sizing.xml")
			Expect(os.WriteFile(hwPath, []byte(hwXML), 0644)).To(Succeed())
			Expect(ValidateXML(hwPath)).To(Succeed())
			Expect(hwXML).To(ContainSubstring("<background_pool_size>" + wantPool + "</background_pool_size>"))
			Expect(hwXML).To(ContainSubstring("<number_of_free_entries_in_pool_to_execute_mutation>" + wantFree + "</number_of_free_entries_in_pool_to_execute_mutation>"))

			profXML := HardwareProfilesOverlayXML(memGiB << 30)
			profPath := filepath.Join(dir, "z_hardware-profiles.xml")
			Expect(os.WriteFile(profPath, []byte(profXML), 0644)).To(Succeed())
			Expect(ValidateXML(profPath)).To(Succeed())
			Expect(profXML).To(ContainSubstring("<max_bytes_before_external_group_by>"))
			Expect(profXML).To(ContainSubstring("<max_bytes_before_external_sort>"))
		},
		Entry("2 vCPU / 4 GiB (tiny burstable safety net)", 2, uint64(4), "4", "2"),
		Entry("2 vCPU / 8 GiB (t4g.large / m8g.large floor)", 2, uint64(8), "4", "2"),
		Entry("4 vCPU / 8 GiB (c8g.xlarge reader)", 4, uint64(8), "8", "4"),
		Entry("8 vCPU / 32 GiB (m8g.2xlarge writer)", 8, uint64(32), "16", "8"),
		Entry("8 vCPU / 64 GiB (r8g.2xlarge writer)", 8, uint64(64), "16", "8"),
		Entry("16 vCPU / 128 GiB (r8g.4xlarge scaled writer)", 16, uint64(128), "32", "16"),
	)
})
