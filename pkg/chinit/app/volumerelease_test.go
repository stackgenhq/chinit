// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	"github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// This file pins `chinit volume-release` — the shutdown-path volume
// reclaim the chinit-volume-release.service unit's ExecStop invokes.
// Three contracts make it safe:
// - the REBOOT GUARD: a reboot reloads the same node with the same
//   volumes; deleting them on reboot is data loss by definition.
// - the ROLE GATE: keepers/backup carry persistent standalone volumes
//   by design; the release is a writer/reader concern only.
// - LIVE-DISCOVERY-FIRST: the tracked state list is the fallback,
//   never the primary (a stale list could name a REPLACEMENT's
//   volume; live discovery names only THIS instance's).

type releaseFakes struct {
	releaser *fakeVolumeReleaser
	restored func()
}

type fakeVolumeReleaser struct {
	listed    []port.AttachedVolume
	listErr   error
	listCalls int
	released  []string
}

func (f *fakeVolumeReleaser) ListOwnVolumes(ctx context.Context, req port.VolumeReleaseRequest) ([]port.AttachedVolume, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listed, nil
}

func (f *fakeVolumeReleaser) ReleaseVolume(ctx context.Context, volumeID string) error {
	f.released = append(f.released, volumeID)
	return nil
}

type releaseProvider struct {
	releaser *fakeVolumeReleaser
}

func (p *releaseProvider) Name() string { return "aws" }
func (p *releaseProvider) Metadata(context.Context) (port.InstanceMetadata, error) {
	return port.InstanceMetadata{}, nil
}
func (p *releaseProvider) GetSecretValue(context.Context, port.SecretRequest) (string, error) {
	return "", nil
}
func (p *releaseProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}
func (p *releaseProvider) ListOwnVolumes(ctx context.Context, req port.VolumeReleaseRequest) ([]port.AttachedVolume, error) {
	return p.releaser.ListOwnVolumes(ctx, req)
}
func (p *releaseProvider) ReleaseVolume(ctx context.Context, volumeID string) error {
	return p.releaser.ReleaseVolume(ctx, volumeID)
}

var _ = Describe("runVolumeRelease", func() {
	var f *releaseFakes
	var statePath string

	writeState := func(body string) string {
		Expect(os.WriteFile(statePath, []byte(body), 0644)).To(Succeed())
		return statePath
	}

	BeforeEach(func() {
		f = &releaseFakes{releaser: &fakeVolumeReleaser{}}
		origBuild := buildProvider
		origReboot := shutdownIsReboot
		buildProvider = func(string, string) (port.Provider, error) {
			return &releaseProvider{releaser: f.releaser}, nil
		}
		shutdownIsReboot = func() (bool, error) { return false, nil }
		f.restored = func() {
			buildProvider = origBuild
			shutdownIsReboot = origReboot
		}
		DeferCleanup(f.restored)

		dir, err := os.MkdirTemp("", "volrel")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() { _ = os.RemoveAll(dir) })
		statePath = filepath.Join(dir, "state.json")
	})

	It("declines on reboot (the volumes survive it)", func() {
		writeState(`{"identity":{"role":"writer","hostname":"writer-1","instance_id":"i-1","private_ip":"10.0.1.21","cluster_name":"demo","region":"us-west-2"}}`)
		shutdownIsReboot = func() (bool, error) { return true, nil }
		Expect(runVolumeRelease([]string{"-state", statePath})).To(Succeed())
		Expect(f.releaser.released).To(BeEmpty(), "a reboot must never release volumes")
	})

	It("skips roles whose volumes are persistent by design", func() {
		writeState(`{"identity":{"role":"keeper","hostname":"keeper-0","instance_id":"i-k","private_ip":"10.0.1.20","cluster_name":"demo","region":"us-west-2"}}`)
		Expect(runVolumeRelease([]string{"-state", statePath})).To(Succeed())
		Expect(f.releaser.released).To(BeEmpty(), "keepers keep their standalone volumes")
	})

	It("releases the live-discovered non-root volumes of a writer", func() {
		writeState(`{"identity":{"role":"writer","hostname":"writer-1","instance_id":"i-1","private_ip":"10.0.1.21","cluster_name":"demo","region":"us-west-2"},"tracked_volumes":["vol-meta","vol-cache"]}`)
		f.releaser.listed = []port.AttachedVolume{
			{VolumeID: "vol-root", Device: "/dev/sda1", SizeGB: 32, DeleteOnTermination: true},
			{VolumeID: "vol-meta", Device: "/dev/sdb", SizeGB: 64, DeleteOnTermination: false},
			{VolumeID: "vol-cache", Device: "/dev/sdc", SizeGB: 500, DeleteOnTermination: true},
		}
		Expect(runVolumeRelease([]string{"-state", statePath})).To(Succeed())
		Expect(f.releaser.released).To(ConsistOf("vol-meta", "vol-cache"),
			"the root volume (sda1) is the OS's; data volumes release")
	})

	It("falls back to the tracked state list when live discovery fails", func() {
		writeState(`{"identity":{"role":"reader","hostname":"reader-2","instance_id":"i-2","private_ip":"10.0.1.32","cluster_name":"demo","region":"us-west-2"},"tracked_volumes":["vol-meta","vol-cache"]}`)
		f.releaser.listErr = context.DeadlineExceeded
		Expect(runVolumeRelease([]string{"-state", statePath})).To(Succeed())
		Expect(f.releaser.released).To(ConsistOf("vol-meta", "vol-cache"))
	})

	It("releases nothing when no volumes qualify", func() {
		writeState(`{"identity":{"role":"writer","hostname":"writer-1","instance_id":"i-1","private_ip":"10.0.1.21","cluster_name":"demo","region":"us-west-2"}}`)
		Expect(runVolumeRelease([]string{"-state", statePath})).To(Succeed())
		Expect(f.releaser.released).To(BeEmpty())
	})

	It("fails closed on an unreadable state (ownership unprovable)", func() {
		Expect(runVolumeRelease([]string{"-state", filepath.Join(filepath.Dir(statePath), "absent.json")})).NotTo(Succeed())
		Expect(f.releaser.released).To(BeEmpty(), "no state, no proof of ownership — nothing releases")
	})
})
