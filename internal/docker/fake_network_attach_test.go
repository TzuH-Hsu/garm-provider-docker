package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
)

// createAttachedRunner creates a running container attached to netName and
// mounting volName at /actions-runner/_work, mirroring what the WP2 topology
// layer builds. Returns the container ID.
func createAttachedRunner(t *testing.T, f *FakeClient, name, netName, volName string) string {
	t.Helper()
	resp, err := f.ContainerCreate(context.Background(), &container.Config{}, &container.HostConfig{
		NetworkMode: container.NetworkMode(netName),
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volName, Target: "/actions-runner/_work"},
		},
	}, nil, nil, name)
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if err := f.ContainerStart(context.Background(), resp.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart returned unexpected error: %v", err)
	}
	return resp.ID
}

func TestFakeContainerInspectReportsNetworkAndMounts(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()
	id := createAttachedRunner(t, f, "job-1", "job-1-net", "job-1-workspace")

	got, err := f.ContainerInspect(ctx, id)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}

	// The per-job network is reported as the container's sole attachment.
	if got.HostConfig == nil || string(got.HostConfig.NetworkMode) != "job-1-net" {
		t.Errorf("HostConfig.NetworkMode = %v, want job-1-net", got.HostConfig)
	}
	if got.NetworkSettings == nil {
		t.Fatal("NetworkSettings is nil, want the job network endpoint")
	}
	ep, ok := got.NetworkSettings.Networks["job-1-net"]
	if !ok || ep == nil || ep.IPAddress == "" {
		t.Errorf("NetworkSettings.Networks = %+v, want a job-1-net endpoint with an IP", got.NetworkSettings.Networks)
	}

	// The workspace volume is mounted at the runner workdir.
	if len(got.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (workspace)", len(got.Mounts))
	}
	m := got.Mounts[0]
	if m.Type != mount.TypeVolume || m.Name != "job-1-workspace" || m.Destination != "/actions-runner/_work" {
		t.Errorf("mount = %+v, want the workspace volume at /actions-runner/_work", m)
	}
}

func TestFakeContainerInspectDefaultBridgeHasNoEndpoint(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()
	// A container with no NetworkMode (default bridge) reports no user network.
	resp, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "plain")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	got, err := f.ContainerInspect(ctx, resp.ID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	if got.NetworkSettings != nil {
		t.Errorf("NetworkSettings = %+v, want nil for a default-bridge container", got.NetworkSettings)
	}
}

// TestFakeNetworkRemoveActiveEndpoints is the regression guard for the ADR-004
// teardown ordering: while a container is still attached, NetworkRemove must
// fail (matching the real daemon's "has active endpoints" 403), and only
// succeed once the container is removed first.
func TestFakeNetworkRemoveActiveEndpoints(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.NetworkCreate(ctx, "job-1-net", network.CreateOptions{})
	if err != nil {
		t.Fatalf("NetworkCreate returned unexpected error: %v", err)
	}
	cid := createAttachedRunner(t, f, "job-1", "job-1-net", "job-1-workspace")

	// Removing the network by ID while the container is attached must fail —
	// and NOT with NotFound (the teardown must surface an ordering bug).
	err = f.NetworkRemove(ctx, created.ID)
	if err == nil {
		t.Fatal("NetworkRemove of an in-use network succeeded, want an active-endpoints error")
	}
	if errdefs.IsNotFound(err) {
		t.Errorf("NetworkRemove error = %v, want an active-endpoints (forbidden) error, not NotFound", err)
	}
	if !errdefs.IsForbidden(err) {
		t.Errorf("NetworkRemove error = %v, want errdefs.IsForbidden (active endpoints)", err)
	}

	// After the container is removed, the network removes cleanly (by name too).
	if err := f.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove returned unexpected error: %v", err)
	}
	if err := f.NetworkRemove(ctx, "job-1-net"); err != nil {
		t.Fatalf("NetworkRemove after container removal returned unexpected error: %v", err)
	}
}
