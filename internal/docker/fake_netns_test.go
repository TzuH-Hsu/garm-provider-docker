package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
)

// createJoiner creates (but does not start) a container that joins the
// network namespace of ownerRef via NetworkMode "container:<ownerRef>".
func createJoiner(t *testing.T, f *FakeClient, name, ownerRef string) string {
	t.Helper()
	resp, err := f.ContainerCreate(context.Background(), &container.Config{}, &container.HostConfig{
		NetworkMode: container.NetworkMode("container:" + ownerRef),
	}, nil, nil, name)
	if err != nil {
		t.Fatalf("ContainerCreate(%s) returned unexpected error: %v", name, err)
	}
	return resp.ID
}

func TestFakeJoinedNetnsStartsOnlyWithRunningOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("missing owner", func(t *testing.T) {
		f := NewFakeClient()
		id := createJoiner(t, f, "job-1-dind", "no-such-runner")
		err := f.ContainerStart(ctx, id, container.StartOptions{})
		if err == nil || !errdefs.IsNotFound(err) || !strings.Contains(err.Error(), "No such container: no-such-runner") {
			t.Fatalf("ContainerStart = %v, want a NotFound naming the missing owner", err)
		}
	})

	t.Run("owner created but not started", func(t *testing.T) {
		f := NewFakeClient()
		owner, err := f.ContainerCreate(ctx, &container.Config{}, &container.HostConfig{}, nil, nil, "job-1")
		if err != nil {
			t.Fatalf("ContainerCreate(owner) returned unexpected error: %v", err)
		}
		id := createJoiner(t, f, "job-1-dind", owner.ID)
		err = f.ContainerStart(ctx, id, container.StartOptions{})
		if err == nil || !strings.Contains(err.Error(), "cannot join network namespace of a non running container") {
			t.Fatalf("ContainerStart = %v, want the non-running-owner refusal", err)
		}
	})

	t.Run("running owner", func(t *testing.T) {
		f := NewFakeClient()
		ownerID := createAttachedRunner(t, f, "job-1", "job-1-net", "job-1-workspace")
		id := createJoiner(t, f, "job-1-dind", ownerID)
		if err := f.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			t.Fatalf("ContainerStart returned unexpected error: %v", err)
		}
		got, err := f.ContainerInspect(ctx, id)
		if err != nil {
			t.Fatalf("ContainerInspect returned unexpected error: %v", err)
		}
		if got.HostConfig.NetworkMode != container.NetworkMode("container:"+ownerID) {
			t.Errorf("NetworkMode = %q, want container:%s", got.HostConfig.NetworkMode, ownerID)
		}
		// A joiner has no network attachment of its own.
		if got.NetworkSettings != nil && len(got.NetworkSettings.Networks) != 0 {
			t.Errorf("NetworkSettings.Networks = %v, want none for a joined namespace", got.NetworkSettings.Networks)
		}
	})
}

// TestFakeJoinedNetnsDoesNotHoldTheNetwork mirrors the real daemon: a container
// that joined another's namespace is not an endpoint of the owner's network, so
// once the owner is gone the network can be removed even while the joiner
// still exists (verified on Docker Engine 29.8.2).
func TestFakeJoinedNetnsDoesNotHoldTheNetwork(t *testing.T) {
	ctx := context.Background()
	f := NewFakeClient()
	ownerID := createAttachedRunner(t, f, "job-1", "job-1-net", "job-1-workspace")
	joinerID := createJoiner(t, f, "job-1-dind", ownerID)
	if err := f.ContainerStart(ctx, joinerID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart(joiner) returned unexpected error: %v", err)
	}
	if err := f.ContainerRemove(ctx, ownerID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove(owner) returned unexpected error: %v", err)
	}
	if err := f.NetworkRemove(ctx, "job-1-net"); err != nil {
		t.Errorf("NetworkRemove with only a joiner left = %v, want success", err)
	}
}

func TestFakeScopedCreateAndStartErrors(t *testing.T) {
	ctx := context.Background()
	f := NewFakeClient()
	boom := errors.New("boom")
	f.CreateErr = boom
	f.CreateErrContainerName = "target"

	other, err := f.ContainerCreate(ctx, &container.Config{}, &container.HostConfig{}, nil, nil, "other")
	if err != nil {
		t.Fatalf("ContainerCreate(other) = %v, want success: the error is scoped to %q", err, f.CreateErrContainerName)
	}
	if _, err := f.ContainerCreate(ctx, &container.Config{}, &container.HostConfig{}, nil, nil, "target"); !errors.Is(err, boom) {
		t.Errorf("ContainerCreate(target) = %v, want the injected error", err)
	}

	f.CreateErr = nil
	target, err := f.ContainerCreate(ctx, &container.Config{}, &container.HostConfig{}, nil, nil, "target")
	if err != nil {
		t.Fatalf("ContainerCreate(target) returned unexpected error: %v", err)
	}
	f.StartErr = boom
	f.StartErrContainerName = "target"
	if err := f.ContainerStart(ctx, other.ID, container.StartOptions{}); err != nil {
		t.Errorf("ContainerStart(other) = %v, want success: the error is scoped to %q", err, f.StartErrContainerName)
	}
	if err := f.ContainerStart(ctx, target.ID, container.StartOptions{}); !errors.Is(err, boom) {
		t.Errorf("ContainerStart(target) = %v, want the injected error", err)
	}
}
