package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"
)

func TestFakeClientImagePull(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	rc, err := f.ImagePull(ctx, "ghcr.io/example/runner:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("ImagePull returned unexpected error: %v", err)
	}
	defer rc.Close()

	if _, err := f.ImagePull(ctx, "ghcr.io/example/other:latest", image.PullOptions{}); err != nil {
		t.Fatalf("ImagePull returned unexpected error: %v", err)
	}

	want := []string{"ghcr.io/example/runner:latest", "ghcr.io/example/other:latest"}
	if len(f.PulledImages) != len(want) {
		t.Fatalf("PulledImages = %v, want %v", f.PulledImages, want)
	}
	for i, w := range want {
		if f.PulledImages[i] != w {
			t.Errorf("PulledImages[%d] = %q, want %q", i, f.PulledImages[i], w)
		}
	}
}

func TestFakeClientContainerLifecycle(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	cfg := &container.Config{
		Image:  "ghcr.io/example/runner:latest",
		Env:    []string{"FOO=bar"},
		Labels: map[string]string{"garm.docker/managed": "true", "garm.docker/instance-name": "my-instance"},
	}

	created, err := f.ContainerCreate(ctx, cfg, nil, nil, nil, "my-instance")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if created.ID == "" {
		t.Fatal("ContainerCreate returned an empty ID")
	}

	// Inspect before start: not yet running.
	inspected, err := f.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	if inspected.State == nil || inspected.State.Running {
		t.Fatalf("expected a not-yet-running container, got state %+v", inspected.State)
	}
	if inspected.Name != "/my-instance" {
		t.Errorf("Name = %q, want %q", inspected.Name, "/my-instance")
	}

	if err := f.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart returned unexpected error: %v", err)
	}

	inspected, err = f.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	if inspected.State == nil || !inspected.State.Running {
		t.Fatalf("expected a running container after start, got state %+v", inspected.State)
	}

	if err := f.ContainerRemove(ctx, created.ID, container.RemoveOptions{}); err != nil {
		t.Fatalf("ContainerRemove returned unexpected error: %v", err)
	}

	if _, err := f.ContainerInspect(ctx, created.ID); !errdefs.IsNotFound(err) {
		t.Fatalf("ContainerInspect after remove: err = %v, want errdefs.IsNotFound", err)
	}
}

func TestFakeClientNotFoundErrors(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	if _, err := f.ContainerInspect(ctx, "does-not-exist"); !errdefs.IsNotFound(err) {
		t.Errorf("ContainerInspect: err = %v, want errdefs.IsNotFound", err)
	}
	if err := f.ContainerStart(ctx, "does-not-exist", container.StartOptions{}); !errdefs.IsNotFound(err) {
		t.Errorf("ContainerStart: err = %v, want errdefs.IsNotFound", err)
	}
	if err := f.ContainerRemove(ctx, "does-not-exist", container.RemoveOptions{}); !errdefs.IsNotFound(err) {
		t.Errorf("ContainerRemove: err = %v, want errdefs.IsNotFound", err)
	}
}

func TestFakeClientContainerListLabelFilter(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	mustCreate := func(name string, labels map[string]string) string {
		t.Helper()
		resp, err := f.ContainerCreate(ctx, &container.Config{Labels: labels}, nil, nil, nil, name)
		if err != nil {
			t.Fatalf("ContainerCreate(%q) returned unexpected error: %v", name, err)
		}
		return resp.ID
	}

	managedID := mustCreate("managed-instance", map[string]string{
		"garm.docker/managed":       "true",
		"garm.docker/instance-name": "managed-instance",
	})
	_ = mustCreate("unmanaged-instance", map[string]string{
		"some.other/label": "true",
	})

	tests := []struct {
		name    string
		filters filters.Args
		wantIDs []string
	}{
		{
			name:    "no filter returns everything",
			filters: filters.NewArgs(),
			wantIDs: nil, // checked by count below, not identity
		},
		{
			name:    "managed=true matches only the managed container",
			filters: filters.NewArgs(filters.Arg("label", "garm.docker/managed=true")),
			wantIDs: []string{managedID},
		},
		{
			name:    "unmatched label returns nothing",
			filters: filters.NewArgs(filters.Arg("label", "garm.docker/managed=false")),
			wantIDs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := f.ContainerList(ctx, container.ListOptions{Filters: tt.filters})
			if err != nil {
				t.Fatalf("ContainerList returned unexpected error: %v", err)
			}
			if tt.name == "no filter returns everything" {
				if len(out) != 2 {
					t.Fatalf("got %d containers, want 2", len(out))
				}
				return
			}
			if len(out) != len(tt.wantIDs) {
				t.Fatalf("got %d containers, want %d", len(out), len(tt.wantIDs))
			}
			for i, want := range tt.wantIDs {
				if out[i].ID != want {
					t.Errorf("containers[%d].ID = %q, want %q", i, out[i].ID, want)
				}
			}
		})
	}
}
