package provider

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// TestCreateInstanceDinDExternalsMount covers the externals volume a container
// job's /__e bind needs in the daemon's filesystem (ADR-001, 2026-10-29
// amendment): with the externals cache enabled, a DinD sidecar mounts the same
// digest-keyed volume the runner does, read-only, at RunnerExternalsDir — in
// both DinD modes, since they share the sidecar spec. Without DinD there is no
// sidecar, and with the cache disabled neither container has the volume.
func TestCreateInstanceDinDExternalsMount(t *testing.T) {
	tests := []struct {
		name         string
		dindMode     string
		cacheEnabled bool
		wantSidecar  bool
		wantMount    bool
	}{
		{name: "privileged-sidecar with cache", dindMode: config.DindModePrivilegedSidecar, cacheEnabled: true, wantSidecar: true, wantMount: true},
		{name: "sysbox-runc with cache", dindMode: config.DindModeSysboxRunc, cacheEnabled: true, wantSidecar: true, wantMount: true},
		{name: "privileged-sidecar without cache", dindMode: config.DindModePrivilegedSidecar, cacheEnabled: false, wantSidecar: true, wantMount: false},
		{name: "no DinD with cache", dindMode: config.DindModeNone, cacheEnabled: true, wantSidecar: false, wantMount: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newJITMetadataServer(t)
			defer srv.Close()

			fake := docker.NewFakeClient()
			fake.ImageLabels["ghcr.io/example/runner@sha256:deadbeef"] = map[string]string{spec.LabelOSRelease: testOSKey}
			cfg := config.Config{
				DockerHost:       "unix:///var/run/docker.sock",
				RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
				DindMode:         tc.dindMode,
				AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
				DindImage:        "docker:dind@sha256:beefdead",
				StorageDriver:    "overlay2",
				Resources:        config.Resources{DindMemory: "4GiB"},
				Network:          config.Network{EnableJobNetwork: true, Internal: false},
				Cache: config.Cache{
					Enabled:       tc.cacheEnabled,
					Generation:    "1",
					PnpmMajor:     "9",
					ToolcachePath: "/opt/hostedtoolcache",
					PnpmStorePath: "/opt/pnpm-store",
				},
			}
			p, err := New(fake, cfg, "controller-abc")
			if err != nil {
				t.Fatalf("New returned unexpected error: %v", err)
			}

			if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
				t.Fatalf("CreateInstance returned unexpected error: %v", err)
			}
			name := "Test-Instance-01"

			insp, _, err := fake.ImageInspectWithRaw(context.Background(), cfg.RunnerImage)
			if err != nil {
				t.Fatalf("runner image inspect: %v", err)
			}
			extName := spec.ExternalsVolumeName(insp.ID[len("sha256:"):])

			runner := inspectRunner(t, fake, name)
			if tc.cacheEnabled {
				assertReadOnlyMount(t, runner.Mounts, extName, spec.RunnerExternalsDir)
			} else {
				assertNoMountAt(t, runner.Mounts, spec.RunnerExternalsDir)
			}

			dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName(name))
			if !tc.wantSidecar {
				if err == nil {
					t.Fatal("a DinD sidecar was created in dind_mode=none")
				}
				return
			}
			if err != nil {
				t.Fatalf("DinD sidecar was not created: %v", err)
			}
			if tc.wantMount {
				// The SAME volume as the runner's, read-only, at the same path.
				assertReadOnlyMount(t, dind.Mounts, extName, spec.RunnerExternalsDir)
			} else {
				assertNoMountAt(t, dind.Mounts, spec.RunnerExternalsDir)
			}
			// The workspace mount a container job's /__w needs stays in place
			// next to the externals one.
			nonce := runner.Config.Labels[spec.LabelCreateNonce]
			assertContainerMount(t, dind.Mounts, spec.WorkspaceVolumeName(name, nonce), spec.RunnerWorkDir)
		})
	}
}

// assertReadOnlyMount asserts a read-only named-volume mount of source at dest.
func assertReadOnlyMount(t *testing.T, mounts []types.MountPoint, source, dest string) {
	t.Helper()
	for _, m := range mounts {
		if m.Name == source && m.Destination == dest {
			if m.RW {
				t.Errorf("mount %q at %q is read-write, want read-only", source, dest)
			}
			return
		}
	}
	t.Errorf("no mount with name %q at %q found in %+v", source, dest, mounts)
}

// assertNoMountAt asserts nothing is mounted at dest.
func assertNoMountAt(t *testing.T, mounts []types.MountPoint, dest string) {
	t.Helper()
	for _, m := range mounts {
		if m.Destination == dest {
			t.Errorf("unexpected mount at %q: %+v", dest, m)
		}
	}
}
