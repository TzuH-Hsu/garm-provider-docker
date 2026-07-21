package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
)

// createAttachedRunner creates a running container attached to netName and
// mounting volName at /actions-runner/_work, mirroring what the WP2 topology
// layer builds. It first ensures the network and volume exist, since
// ContainerStart now (F5) rejects a start whose attached network or required
// named volume is missing. Returns the container ID.
func createAttachedRunner(t *testing.T, f *FakeClient, name, netName, volName string) string {
	t.Helper()
	// NetworkCreate 409s if the caller already created the network; tolerate it.
	_, _ = f.NetworkCreate(context.Background(), netName, network.CreateOptions{})
	if _, err := f.VolumeCreate(context.Background(), volume.CreateOptions{Name: volName}); err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}
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

// TestFakeVolumeRemoveRejectsInUse is the F13(a) guard: the real daemon rejects
// removing a volume still referenced by ANY container (running OR stopped),
// even with force. The fake models that with a Conflict, so a teardown that
// tried to remove a volume before its container fails in units too — and only
// succeeds once the container is gone.
func TestFakeVolumeRemoveRejectsInUse(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()
	cid := createAttachedRunner(t, f, "job-1", "job-1-net", "job-1-workspace")

	// Referenced by a running container → rejected (even with force).
	if err := f.VolumeRemove(ctx, "job-1-workspace", true); err == nil {
		t.Fatal("VolumeRemove of an in-use volume succeeded, want a Conflict (volume in use)")
	} else if !errdefs.IsConflict(err) {
		t.Errorf("VolumeRemove in-use error = %v, want errdefs.IsConflict", err)
	}

	// Stop it — still referenced (stopped counts too) → still rejected.
	if err := f.ContainerStop(ctx, cid, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop returned unexpected error: %v", err)
	}
	if err := f.VolumeRemove(ctx, "job-1-workspace", true); err == nil {
		t.Error("VolumeRemove of a volume referenced by a STOPPED container succeeded, want a Conflict")
	}

	// Remove the container → the volume removes cleanly.
	if err := f.ContainerRemove(ctx, cid, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove returned unexpected error: %v", err)
	}
	if err := f.VolumeRemove(ctx, "job-1-workspace", false); err != nil {
		t.Errorf("VolumeRemove after container removal returned unexpected error: %v", err)
	}
}

// TestFakeContainerStartRejectsMissingNetworkOrVolume is the F5 fake-fidelity
// guard: ContainerStart fails when the attached user-defined network or a
// required named volume has been removed out from under the container (e.g. by
// a concurrent sweep), matching the real daemon — so that class of bug can no
// longer pass a unit test.
func TestFakeContainerStartRejectsMissingNetworkOrVolume(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	// Attached network missing.
	if _, err := f.VolumeCreate(ctx, volume.CreateOptions{Name: "j-workspace"}); err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}
	respA, err := f.ContainerCreate(ctx, &container.Config{}, &container.HostConfig{
		NetworkMode: container.NetworkMode("j-net-gone"),
		Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: "j-workspace", Target: "/w"}},
	}, nil, nil, "start-no-net")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if err := f.ContainerStart(ctx, respA.ID, container.StartOptions{}); err == nil {
		t.Error("ContainerStart with a missing attached network succeeded, want an error")
	}

	// Required named volume missing (network present).
	if _, err := f.NetworkCreate(ctx, "j-net", network.CreateOptions{}); err != nil {
		t.Fatalf("NetworkCreate returned unexpected error: %v", err)
	}
	respB, err := f.ContainerCreate(ctx, &container.Config{}, &container.HostConfig{
		NetworkMode: container.NetworkMode("j-net"),
		Mounts:      []mount.Mount{{Type: mount.TypeVolume, Source: "j-vol-gone", Target: "/w"}},
	}, nil, nil, "start-no-vol")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if err := f.ContainerStart(ctx, respB.ID, container.StartOptions{}); err == nil {
		t.Error("ContainerStart with a missing named volume succeeded, want an error")
	}
}
