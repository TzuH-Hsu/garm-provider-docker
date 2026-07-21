package docker

import (
	"context"
	"slices"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// TestFakeModelsDindSidecarFields asserts the fake faithfully records and
// surfaces the container.Config.Cmd and the HostConfig.Privileged/Runtime
// fields the DinD sidecar sets (ADR-001), so provider/topology unit tests can
// assert the sidecar was built with the right dockerd argv, privilege, and
// runtime — the same real-daemon contract WP3's live verification checks.
func TestFakeModelsDindSidecarFields(t *testing.T) {
	f := NewFakeClient()

	cmd := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2"}
	resp, err := f.ContainerCreate(context.Background(),
		&container.Config{
			Image: "docker:dind@sha256:abc",
			Env:   []string{"DOCKER_TLS_CERTDIR="},
			Cmd:   cmd,
		},
		&container.HostConfig{
			Privileged: true,
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: "job-1-socket", Target: "/run"},
				{Type: mount.TypeVolume, Source: "job-1-dind-state", Target: "/var/lib/docker"},
			},
			NetworkMode: container.NetworkMode("job-1-net"),
		}, nil, nil, "job-1-dind")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}

	got, err := f.ContainerInspect(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}

	// dockerd argv round-trips.
	if !slices.Equal([]string(got.Config.Cmd), cmd) {
		t.Errorf("Config.Cmd = %v, want %v", got.Config.Cmd, cmd)
	}
	// Privileged is surfaced.
	if got.HostConfig == nil || !got.HostConfig.Privileged {
		t.Errorf("HostConfig.Privileged = %v, want true", got.HostConfig)
	}
	// Default runtime (privileged-sidecar) is empty.
	if got.HostConfig.Runtime != "" {
		t.Errorf("HostConfig.Runtime = %q, want empty (default runtime)", got.HostConfig.Runtime)
	}
	// Both DinD mounts surface under .Mounts.
	haveSocket, haveState := false, false
	for _, m := range got.Mounts {
		if m.Name == "job-1-socket" && m.Destination == "/run" {
			haveSocket = true
		}
		if m.Name == "job-1-dind-state" && m.Destination == "/var/lib/docker" {
			haveState = true
		}
	}
	if !haveSocket || !haveState {
		t.Errorf("Mounts = %+v, want socket at /run and dind-state at /var/lib/docker", got.Mounts)
	}
}

// TestFakeRecordsRuntimeForSysbox confirms a non-empty runtime (WP4's
// sysbox-runc) is recorded and surfaced, so the same fake serves both DinD
// modes unchanged.
func TestFakeRecordsRuntimeForSysbox(t *testing.T) {
	f := NewFakeClient()
	resp, err := f.ContainerCreate(context.Background(),
		&container.Config{Image: "docker:dind@sha256:abc"},
		&container.HostConfig{Privileged: false, Runtime: "sysbox-runc"},
		nil, nil, "job-2-dind")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	got, err := f.ContainerInspect(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	if got.HostConfig.Privileged {
		t.Error("Privileged = true, want false for sysbox-runc")
	}
	if got.HostConfig.Runtime != "sysbox-runc" {
		t.Errorf("Runtime = %q, want sysbox-runc", got.HostConfig.Runtime)
	}
}
