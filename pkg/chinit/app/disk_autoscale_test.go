// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/stackgenhq/chinit/pkg/chinit/domain/model"
	chinitport "github.com/stackgenhq/chinit/pkg/chinit/domain/port"
)

// This file pins `chinit disk-autoscale` — the every-5-minute timer the
// units matrix installs on writers AND readers. The job keeps EFFECTIVE
// disk headroom above the floor by growing the node's own data volume +
// filesystem online — without it, keep_free_space on a small volume can
// leave ~1 MiB effective, merges stall on NOT_ENOUGH_SPACE, and parts
// climb toward the parts_to_throw_insert hard reject.

// diskAutoscaleDir is this suite's scratch dir (state files) — never
// /var/lib.
var diskAutoscaleDir = filepath.Join(os.TempDir(), "chinit-app-disk-autoscale-test")

// diskAutoscaleState seeds a writer's state file.
func diskAutoscaleState(identity model.NodeIdentity) string {
	data, err := json.Marshal(nodeState{Identity: identity})
	Expect(err).NotTo(HaveOccurred())
	Expect(os.MkdirAll(diskAutoscaleDir, 0755)).To(Succeed())
	path := filepath.Join(diskAutoscaleDir, "state.json")
	Expect(os.WriteFile(path, data, 0644)).To(Succeed())
	return path
}

// diskAutoscaleFakes records the exec + provider surface.
type diskAutoscaleFakes struct {
	runner   *fakeRunner
	grower   *fakeVolumeGrower
	pusher   *fakeMetricPusher
	provider *diskAutoscaleProvider
}

// fakeVolumeGrower records grow calls and answers with fixed volumes.
type fakeVolumeGrower struct {
	volume      *chinitport.VolumeInfo
	described   []chinitport.VolumeGrowRequest
	grew        []int32
	waited      []string
	describeErr error
	growErr     error
}

func (f *fakeVolumeGrower) DescribeOwnVolume(ctx context.Context, req chinitport.VolumeGrowRequest) (*chinitport.VolumeInfo, error) {
	f.described = append(f.described, req)
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	return f.volume, nil
}

func (f *fakeVolumeGrower) GrowVolume(ctx context.Context, volumeID string, targetSizeGB int32) (bool, error) {
	if f.growErr != nil {
		return false, f.growErr
	}
	f.grew = append(f.grew, targetSizeGB)
	return true, nil
}

func (f *fakeVolumeGrower) WaitForModification(ctx context.Context, volumeID string) error {
	f.waited = append(f.waited, volumeID)
	return nil
}

// fakeMetricPusher records pushes.
type fakeMetricPusher struct {
	pushed []chinitport.MetricPushRequest
}

func (f *fakeMetricPusher) PutMetric(ctx context.Context, req chinitport.MetricPushRequest) error {
	f.pushed = append(f.pushed, req)
	return nil
}

// diskAutoscaleProvider composes the capability fakes.
type diskAutoscaleProvider struct {
	name   string
	grower chinitport.VolumeGrower
	pusher chinitport.MetricPusher
}

func (p *diskAutoscaleProvider) Name() string { return p.name }
func (p *diskAutoscaleProvider) Metadata(context.Context) (chinitport.InstanceMetadata, error) {
	return chinitport.InstanceMetadata{}, nil
}
func (p *diskAutoscaleProvider) GetSecretValue(context.Context, chinitport.SecretRequest) (string, error) {
	return "", nil
}
func (p *diskAutoscaleProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}
func (p *diskAutoscaleProvider) DescribeOwnVolume(ctx context.Context, req chinitport.VolumeGrowRequest) (*chinitport.VolumeInfo, error) {
	return p.grower.DescribeOwnVolume(ctx, req)
}
func (p *diskAutoscaleProvider) GrowVolume(ctx context.Context, id string, size int32) (bool, error) {
	return p.grower.GrowVolume(ctx, id, size)
}
func (p *diskAutoscaleProvider) WaitForModification(ctx context.Context, id string) error {
	return p.grower.WaitForModification(ctx, id)
}
func (p *diskAutoscaleProvider) PutMetric(ctx context.Context, req chinitport.MetricPushRequest) error {
	return p.pusher.PutMetric(ctx, req)
}

// noGrowProvider is the capability-absent provider: metrics yes, volume
// growth no — AsVolumeGrower must return nil for it.
type noGrowProvider struct {
	name   string
	pusher chinitport.MetricPusher
}

func (p *noGrowProvider) Name() string { return p.name }
func (p *noGrowProvider) Metadata(context.Context) (chinitport.InstanceMetadata, error) {
	return chinitport.InstanceMetadata{}, nil
}
func (p *noGrowProvider) GetSecretValue(context.Context, chinitport.SecretRequest) (string, error) {
	return "", nil
}
func (p *noGrowProvider) FetchBundle(context.Context, model.ConfigBundle, string) ([]string, error) {
	return nil, nil
}
func (p *noGrowProvider) PutMetric(ctx context.Context, req chinitport.MetricPushRequest) error {
	return p.pusher.PutMetric(ctx, req)
}

// contains is the test's []string containment helper (Gomega's
// ContainElement works on matchers, not raw values — this keeps the
// positional-independent probe lookup readable).
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

var _ = Describe("runDiskAutoscale", func() {
	var fakes *diskAutoscaleFakes
	var statePath string

	var probedArgs [][]string
	var probedEnv [][]string

	seed := func(freeGB float64) {
		fakes = &diskAutoscaleFakes{runner: &fakeRunner{}}
		identity := model.NodeIdentity{
			Role:        model.RoleWriter,
			Hostname:    "writer-0",
			InstanceID:  "i-0cf1cda168d841a50",
			Region:      "us-west-2",
			ClusterName: "example",
		}
		statePath = diskAutoscaleState(identity)

		fakes.grower = &fakeVolumeGrower{volume: &chinitport.VolumeInfo{
			VolumeID: "vol-test", SizeGB: 32, AttachedTo: identity.InstanceID,
		}}
		fakes.pusher = &fakeMetricPusher{}
		fakes.provider = &diskAutoscaleProvider{name: "aws", grower: fakes.grower, pusher: fakes.pusher}

		// The admin password file seam (the probe's credential source).
		passwordPath := filepath.Join(diskAutoscaleDir, "admin-password")
		Expect(os.WriteFile(passwordPath, []byte("sekrit\n"), 0644)).To(Succeed())
		origPassword := adminPasswordPath
		adminPasswordPath = func() string { return passwordPath }
		DeferCleanup(func() { adminPasswordPath = origPassword })

		origRun := runCommand
		origRunWithEnv := runCommandWithEnv
		origBuild := buildProvider
		runCommandWithEnv = func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			probedArgs = append(probedArgs, args)
			probedEnv = append(probedEnv, env)
			// The disks probe: <unreserved> <keep_free> — the seeded
			// GiB as unreserved, storage.xml's 10 GiB reservation as
			// keep_free (the production storage.xml shape).
			return []byte(fmt.Sprintf("%d %d", int64(freeGB*(1<<30)), int64(10*(1<<30)))), nil
		}
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return fakes.provider, nil
		}
		DeferCleanup(func() {
			runCommand = origRun
			runCommandWithEnv = origRunWithEnv
			buildProvider = origBuild
		})
	}

	It("is a no-op above the floor", func() {
		seed(40) // 40 GiB unreserved > derived floor (keep_free 10 + headroom 2 = 12 GiB)
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.grew).To(BeEmpty())
		Expect(fakes.grower.described).To(BeEmpty())

		// The probe must authenticate as admin with the password file's
		// value — the bare client's `default` user is refused outright
		// (every timer start failed on
		// AUTHENTICATION_FAILED until the credentials were threaded).
		// Credentials ride ENV, never argv: argv is world-readable in
		// /proc/<pid>/cmdline for the process's whole lifetime.
		// The fs-converge step (findmnt/resize2fs) runs FIRST via the
		// same exec seam, so the SQL probe is located by its --query
		// element, not by position.
		Expect(probedArgs).NotTo(BeEmpty())
		var probe []string
		for _, p := range probedArgs {
			if contains(p, "--query") {
				probe = p
				break
			}
		}
		Expect(probe).NotTo(BeNil(), "the SQL headroom probe must run")
		Expect(probe).To(ContainElement("--query"))
		Expect(probe).NotTo(ContainElement("admin"), "the username must not ride argv")
		Expect(probe).NotTo(ContainElement("sekrit"), "the password must not ride argv")
		Expect(probe).NotTo(ContainElement("--password"), "the password flag must not ride argv")
		var probeEnv []string
		for _, e := range probedEnv {
			if contains(e, "CLICKHOUSE_USER=admin") {
				probeEnv = e
				break
			}
		}
		Expect(probeEnv).NotTo(BeEmpty(), "the probe's env must carry the admin credentials")
		Expect(probeEnv).To(ContainElement("CLICKHOUSE_USER=admin"))
		Expect(probeEnv).To(ContainElement("CLICKHOUSE_PASSWORD=sekrit"))
	})

	It("grows the volume and resizes the filesystem below the floor", func() {
		seed(2) // 2 GiB unreserved < derived floor (12 GiB): must grow 3x to 96
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.described).To(HaveLen(1))
		Expect(fakes.grower.described[0].InstanceID).To(Equal("i-0cf1cda168d841a50"))
		Expect(fakes.grower.described[0].Device).To(Equal("/dev/sdb"))
		// 3x the CURRENT size (32 -> 96): one grow event buys days of
		// scratch headroom instead of hours, staying far from AWS's
		// per-volume modification rate limit (the 2026-11-17 finding:
		// +32 linear steps burned the budget and locked a writer out
		// of modifications for 19 hours).
		Expect(fakes.grower.grew).To(Equal([]int32{96}))
		Expect(fakes.grower.waited).To(Equal([]string{"vol-test"}))
		// The fs step shells findmnt/resize2fs: the exec fake answered
		// with the seeded bytes for every call, so success means the
		// commands ran and were tolerated.
		autoScaled := false
		for _, p := range fakes.pusher.pushed {
			if p.Name == "DiskAutoscaled" {
				autoScaled = true
			}
		}
		Expect(autoScaled).To(BeTrue(), "the grow outcome must be published for the alarm side")
	})

	It("fails loudly when the provider cannot grow volumes", func() {
		seed(2)
		// Capability absent: a provider type with no grow methods
		// (AsVolumeGrower returns nil), not a nil grower field.
		origBuild := buildProvider
		buildProvider = func(string, string) (chinitport.Provider, error) {
			return &noGrowProvider{name: "aws", pusher: fakes.pusher}, nil
		}
		DeferCleanup(func() { buildProvider = origBuild })

		err := runDiskAutoscale([]string{"-state", statePath})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot grow volumes"))
		// The pressure is still published (the alarm-side signal).
		pressurePushed := false
		for _, p := range fakes.pusher.pushed {
			if p.Name == "DiskEffectiveFreeGB" {
				pressurePushed = true
			}
		}
		Expect(pressurePushed).To(BeTrue(), "the disk-pressure signal must reach CloudWatch even when the remedy is absent")
	})

	It("never grows past the ceiling", func() {
		seed(2)
		fakes.grower.volume.SizeGB = 250 // near the 512 GiB ceiling
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.grew).To(Equal([]int32{512}))
	})

	It("reports the pressure without growing when already at the ceiling", func() {
		seed(2)
		fakes.grower.volume.SizeGB = 512
		err := runDiskAutoscale([]string{"-state", statePath})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("ceiling"))
		Expect(fakes.grower.grew).To(BeEmpty())
	})

	It("triples an already-large volume in one event (the rate-limit budget)", func() {
		// The 2026-11-17 live shape: a 160 GiB hot volume at the floor
		// under 60TB/day ingest. The 3x step lands at 480 — days of
		// headroom in ONE modification slot; the old +32 step needed
		// ten rate-limited slots to converge the same distance.
		seed(2)
		fakes.grower.volume.SizeGB = 160
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.grew).To(Equal([]int32{480}))
	})
})
