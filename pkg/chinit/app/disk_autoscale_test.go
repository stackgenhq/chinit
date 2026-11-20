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

// This file pins `chinit disk-autoscale`'s app-layer wiring — the
// exec-backed adapters the domain service drives. The POLICY specs
// (floor derivation, grow-or-relieve) live in the service package's
// own suite; here the contract is transport: the probe runs
// clickhouse-client with credentials on ENV (never argv), the df
// probe feeds the fs size, the resizer shells findmnt/resize2fs, and
// the outcome reaches the metric pusher.

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

var _ = Describe("runDiskAutoscale wiring", func() {
	var fakes *diskAutoscaleFakes
	var statePath string

	var probedArgs [][]string
	var probedEnv [][]string

	// seedWithFs fakes the exec seam: the reservation-book probe answers
	// <unreserved> <keep_free>, the df probe answers the fs size
	// (fsSizeGB 0 = emulated df failure → the fixed-headroom floor).
	seedWithFs := func(freeGB float64, fsSizeGB uint64) {
		fakes = &diskAutoscaleFakes{grower: &fakeVolumeGrower{volume: &chinitport.VolumeInfo{
			VolumeID: "vol-test", SizeGB: 32, AttachedTo: "i-0cf1cda168d841a50",
		}}}
		fakes.pusher = &fakeMetricPusher{}
		fakes.provider = &diskAutoscaleProvider{name: "aws", grower: fakes.grower, pusher: fakes.pusher}

		// The admin password file seam (the probe's credential source).
		// MkdirAll first: on a fresh runner the scratch dir does not
		// exist until diskAutoscaleState creates it for the state file,
		// and the FIRST spec in a random order writes the password
		// BEFORE any state file lands — the bare WriteFile failed with
		// ENOENT and the whole Describe failed on cold CI.
		Expect(os.MkdirAll(diskAutoscaleDir, 0755)).To(Succeed())
		passwordPath := filepath.Join(diskAutoscaleDir, "admin-password")
		Expect(os.WriteFile(passwordPath, []byte("sekrit\n"), 0644)).To(Succeed())
		origPassword := adminPasswordPath
		adminPasswordPath = func() string { return passwordPath }
		DeferCleanup(func() { adminPasswordPath = origPassword })

		origRun := runCommand
		origRunWithEnv := runCommandWithEnv
		origBuild := buildProvider
		runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch name {
			case "df":
				if fsSizeGB == 0 {
					return nil, fmt.Errorf("df: emulated failure")
				}
				return []byte(fmt.Sprintf("1B-blocks\n%d", fsSizeGB*(1<<30))), nil
			case "findmnt":
				return []byte("/dev/nvme1n1"), nil
			default:
				return []byte(""), nil
			}
		}
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
	seed := func(freeGB float64) { seedWithFs(freeGB, 256) }

	identity := model.NodeIdentity{
		Role:        model.RoleWriter,
		Hostname:    "writer-0",
		InstanceID:  "i-0cf1cda168d841a50",
		Region:      "us-west-2",
		ClusterName: "example",
	}

	It("runs the SQL probe with credentials on env, never argv", func() {
		seed(60) // above the proportional floor (48.4 GiB): idle tick
		statePath = diskAutoscaleState(identity)
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.grew).To(BeEmpty())
		Expect(fakes.grower.described).To(BeEmpty())

		// The probe must authenticate as admin with the password file's
		// value — the bare client's `default` user is refused outright
		// (every timer start failed on AUTHENTICATION_FAILED until the
		// credentials were threaded). Credentials ride ENV, never argv:
		// argv is world-readable in /proc/<pid>/cmdline for the
		// process's whole lifetime. The fs-converge step
		// (findmnt/resize2fs) and the df probe run through the same exec
		// seams, so the SQL probe is located by its --query element.
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

	It("grows the volume and filesystem below the floor", func() {
		// 30 GiB unreserved < proportional floor (48.4 GiB) on a 256 GiB
		// fs: the 3x grow lands at 96, the fs is resized, the outcome
		// published.
		seed(30)
		statePath = diskAutoscaleState(identity)
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.described).To(HaveLen(1))
		Expect(fakes.grower.described[0].InstanceID).To(Equal(identity.InstanceID))
		Expect(fakes.grower.described[0].Device).To(Equal("/dev/sdb"))
		// 3x the CURRENT size (32 -> 96): one grow event buys days of
		// scratch headroom instead of hours, staying far from AWS's
		// per-volume modification rate limit (linear steps burn the
		// modification budget and lock a node out of resizes for
		// hours).
		Expect(fakes.grower.grew).To(Equal([]int32{96}))
		Expect(fakes.grower.waited).To(Equal([]string{"vol-test"}))
		autoScaled := false
		for _, p := range fakes.pusher.pushed {
			if p.Name == "DiskAutoscaled" {
				autoScaled = true
			}
		}
		Expect(autoScaled).To(BeTrue(), "the grow outcome must be published for the alarm side")
	})

	It("fails loudly when the provider cannot grow volumes", func() {
		seed(2) // below any floor: grow path entered
		statePath = diskAutoscaleState(identity)
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

	It("degrades to the fixed-headroom floor when the df probe fails", func() {
		// df failing must NEVER disable growth: the floor falls back to
		// keep_free 10 + fixed 2 = 12 GiB. 5 GiB unreserved must grow.
		seedWithFs(5, 0)
		statePath = diskAutoscaleState(identity)
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.grew).To(Equal([]int32{96}))
	})

	It("derives the floor proportionally on large volumes", func() {
		// The 40TB-run blind spot: a 256 GiB volume at 40 GiB unreserved
		// — ABOVE the old fixed floor (12 GiB), BELOW the proportional
		// one (48.4 GiB): the grow must trigger several ticks before the
		// disk is critical.
		seed(40)
		statePath = diskAutoscaleState(identity)
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.grower.grew).To(Equal([]int32{96}))
	})

	It("keeps the idle tick metric-free (log-only above the floor)", func() {
		seed(60) // above the proportional floor: idle
		statePath = diskAutoscaleState(identity)
		Expect(runDiskAutoscale([]string{"-state", statePath})).To(Succeed())
		Expect(fakes.pusher.pushed).To(BeEmpty(), "an idle tick must not publish grow/pressure metrics")
	})

	It("does not publish a zero reading when the probe fails", func() {
		// A failed probe must not reach the alarm side as a zero gauge —
		// the alarm's missing-data treatment owns the gap (a zero
		// would false-page while the node may be perfectly healthy).
		seed(60)
		origRun := runCommandWithEnv
		runCommandWithEnv = func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			probedArgs = append(probedArgs, args)
			probedEnv = append(probedEnv, env)
			return nil, fmt.Errorf("clickhouse-client: emulated probe failure")
		}
		DeferCleanup(func() { runCommandWithEnv = origRun })

		statePath = diskAutoscaleState(identity)
		err := runDiskAutoscale([]string{"-state", statePath})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("probe failed"))
		Expect(fakes.pusher.pushed).To(BeEmpty(), "a failed probe must not publish a zero gauge")
	})
})

// diskAutoscaleTimeout pins the tick's deadline: 15 minutes from the
// timer unit (systemd's own OnCalendar is the outer bound; the job
// self-bounds for the cloud API waits).
var _ = Describe("diskAutoscaleConfig", func() {
	It("follows the launch-template mount/device convention", func() {
		cfg := defaultDiskAutoscaleConfig()
		Expect(cfg.DataDiskDevice).To(Equal("/dev/sdb"))
		Expect(cfg.DataMountPoint).To(Equal("/var/lib/clickhouse"))
	})
})
