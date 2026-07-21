package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/errdefs"
)

// waitResult drains the two ContainerWait channels once, returning the status
// (or an error), so a test reads the run-to-completion outcome deterministically.
func waitResult(t *testing.T, statusCh <-chan container.WaitResponse, errCh <-chan error) (container.WaitResponse, error) {
	t.Helper()
	select {
	case s := <-statusCh:
		return s, nil
	case err := <-errCh:
		return container.WaitResponse{}, err
	}
}

// TestFakeContainerWaitRunToCompletion: a started container is transitioned to
// exited and its status carries the configured BatchExitCode — the model of a
// helper (seed/prune) running to completion.
func TestFakeContainerWaitRunToCompletion(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.ContainerCreate(ctx, &container.Config{Image: "x"}, &container.HostConfig{}, nil, nil, "helper")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := f.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}

	f.BatchExitCode = 0
	sc, ec := f.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	s, werr := waitResult(t, sc, ec)
	if werr != nil {
		t.Fatalf("ContainerWait err: %v", werr)
	}
	if s.StatusCode != 0 {
		t.Errorf("exit code = %d, want 0", s.StatusCode)
	}
	// The container is now exited (run to completion).
	insp, _ := f.ContainerInspect(ctx, created.ID)
	if insp.State == nil || insp.State.Running {
		t.Error("helper container should be exited after ContainerWait")
	}
}

// TestFakeContainerWaitNonZeroExit: a helper that ran but failed reports its
// non-zero exit code (the seed/prune-failure path).
func TestFakeContainerWaitNonZeroExit(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()
	created, _ := f.ContainerCreate(ctx, &container.Config{Image: "x"}, &container.HostConfig{}, nil, nil, "helper")
	_ = f.ContainerStart(ctx, created.ID, container.StartOptions{})

	f.BatchExitCode = 2
	sc, ec := f.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	s, werr := waitResult(t, sc, ec)
	if werr != nil {
		t.Fatalf("ContainerWait err: %v", werr)
	}
	if s.StatusCode != 2 {
		t.Errorf("exit code = %d, want 2", s.StatusCode)
	}
}

// TestFakeContainerWaitErrors: an injected wait error and a missing container
// both surface on the error channel.
func TestFakeContainerWaitErrors(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	sentinel := errors.New("daemon wait boom")
	f.BatchWaitErr = sentinel
	sc1, ec1 := f.ContainerWait(ctx, "nope", container.WaitConditionNotRunning)
	if _, err := waitResult(t, sc1, ec1); !errors.Is(err, sentinel) {
		t.Errorf("injected wait error = %v, want %v", err, sentinel)
	}

	f.BatchWaitErr = nil
	sc2, ec2 := f.ContainerWait(ctx, "does-not-exist", container.WaitConditionNotRunning)
	if _, err := waitResult(t, sc2, ec2); !errdefs.IsNotFound(err) {
		t.Errorf("wait on a missing container = %v, want NotFound", err)
	}
}

// TestFakeCreatedRecordsHelperShape: the Created log retains a helper's
// entrypoint/mounts even after the container is removed, so a test can assert
// the seed/prune invocation shape post-hoc.
func TestFakeCreatedRecordsHelperShape(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	cfg := &container.Config{
		Image:      "runner",
		Entrypoint: strslice.StrSlice{"/bin/sh", "-ec", "flock 9; cp -a /src/. /dest/"},
		Labels:     map[string]string{"garm.docker/role": "cache-helper"},
	}
	host := &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: "vol", Target: "/dest"}}}
	created, err := f.ContainerCreate(ctx, cfg, host, nil, nil, "garm-seed-1")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	// Remove it — the live container is gone, but Created retains its shape.
	if err := f.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}

	if len(f.Created) != 1 {
		t.Fatalf("Created has %d entries, want 1", len(f.Created))
	}
	rec := f.Created[0]
	if rec.Name != "garm-seed-1" {
		t.Errorf("recorded name = %q, want garm-seed-1", rec.Name)
	}
	if len(rec.Entrypoint) != 3 || rec.Entrypoint[0] != "/bin/sh" {
		t.Errorf("recorded entrypoint = %v, want the sh override", rec.Entrypoint)
	}
	if rec.Labels["garm.docker/role"] != "cache-helper" {
		t.Errorf("recorded labels = %v, want role=cache-helper", rec.Labels)
	}
	if len(rec.Mounts) != 1 || rec.Mounts[0].Source != "vol" {
		t.Errorf("recorded mounts = %v, want the vol mount", rec.Mounts)
	}
}
