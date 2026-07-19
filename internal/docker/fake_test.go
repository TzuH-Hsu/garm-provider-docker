package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"strings"
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

func TestFakeClientContainerStopAndSetState(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "c1")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if err := f.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart returned unexpected error: %v", err)
	}

	if err := f.ContainerStop(ctx, created.ID, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop returned unexpected error: %v", err)
	}
	got, err := f.ContainerInspect(ctx, created.ID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	if got.State.Running || got.State.Status != "exited" {
		t.Errorf("after stop: state = %+v, want exited/not-running", got.State)
	}

	// SetState reaches states start/stop cannot.
	f.SetState(created.ID, "dead", true)
	got, _ = f.ContainerInspect(ctx, created.ID)
	if !got.State.Dead || !got.State.OOMKilled {
		t.Errorf("after SetState(dead, oom): state = %+v", got.State)
	}

	// Stop on a missing container is NotFound.
	if err := f.ContainerStop(ctx, "nope", container.StopOptions{}); !errdefs.IsNotFound(err) {
		t.Errorf("ContainerStop(missing): err = %v, want NotFound", err)
	}
}

func TestFakeClientImageInspectAndPull(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	// Absent image -> NotFound.
	if _, _, err := f.ImageInspectWithRaw(ctx, "ghcr.io/example/runner:latest"); !errdefs.IsNotFound(err) {
		t.Fatalf("ImageInspectWithRaw on absent image: err = %v, want NotFound", err)
	}

	// After pull, it is present.
	rc, err := f.ImagePull(ctx, "ghcr.io/example/runner:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("ImagePull returned unexpected error: %v", err)
	}
	_ = rc.Close()
	if _, _, err := f.ImageInspectWithRaw(ctx, "ghcr.io/example/runner:latest"); err != nil {
		t.Fatalf("ImageInspectWithRaw after pull: unexpected error %v", err)
	}

	// PullErr short-circuits and leaves the image absent.
	f2 := NewFakeClient()
	f2.PullErr = errors.New("pull boom")
	if _, err := f2.ImagePull(ctx, "ghcr.io/example/x:1", image.PullOptions{}); err == nil {
		t.Fatal("expected ImagePull to fail when PullErr is set")
	}
	if _, _, err := f2.ImageInspectWithRaw(ctx, "ghcr.io/example/x:1"); !errdefs.IsNotFound(err) {
		t.Fatalf("image should remain absent after failed pull: err = %v", err)
	}
}

func TestFakeClientExecStream(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "c1")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}

	// Exec into a not-yet-started container fails (exec requires a running
	// container, matching the real daemon).
	if _, err := f.ExecStream(ctx, created.ID, []string{"tar", "-x"}, strings.NewReader("x")); err == nil {
		t.Fatal("expected ExecStream into a stopped container to fail")
	}

	if err := f.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart returned unexpected error: %v", err)
	}

	// A successful exec of a tar records the call and makes the extracted
	// files visible in the container's tmpfs model.
	archive := tarBytes(t, map[string]string{"runner": "R", ".delivered": ""})
	cmd := []string{"tar", "-x", "-p", "-C", "/run/garm"}
	code, err := f.ExecStream(ctx, created.ID, cmd, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("ExecStream returned unexpected error: %v", err)
	}
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if len(f.Execs) != 1 {
		t.Fatalf("Execs = %d, want 1", len(f.Execs))
	}
	if f.Execs[0].ContainerID != created.ID || strings.Join(f.Execs[0].Cmd, " ") != strings.Join(cmd, " ") {
		t.Errorf("unexpected exec record: %+v", f.Execs[0])
	}
	// Exec writes ARE visible in the tmpfs (the F1 regression model).
	tmpfs := f.Tmpfs(created.ID)
	if tmpfs["runner"] != "R" {
		t.Errorf("tmpfs[runner] = %q, want R (exec write must be visible)", tmpfs["runner"])
	}
	if _, ok := tmpfs[".delivered"]; !ok {
		t.Error("tmpfs missing the .delivered marker")
	}

	// A non-zero exit is reported and suppresses the tmpfs write.
	f.ExecExitCode = 2
	code, err = f.ExecStream(ctx, created.ID, cmd, bytes.NewReader(tarBytes(t, map[string]string{"other": "X"})))
	if err != nil {
		t.Fatalf("ExecStream returned unexpected error: %v", err)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if _, ok := f.Tmpfs(created.ID)["other"]; ok {
		t.Error("a non-zero exec must not write into the tmpfs")
	}
	f.ExecExitCode = 0

	// ExecErr forces a plumbing failure regardless of container state.
	f.ExecErr = errors.New("exec boom")
	if _, err := f.ExecStream(ctx, created.ID, cmd, strings.NewReader("y")); err == nil {
		t.Fatal("expected ExecStream to fail when ExecErr is set")
	}
}

// TestFakeClientContainerCreateNameConflict proves the fake models the
// daemon's Docker-name uniqueness: a second create with an in-use name is
// rejected with an errdefs.IsConflict error, and no second container is
// recorded (NEW-5).
func TestFakeClientContainerCreateNameConflict(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	if _, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "dupe"); err != nil {
		t.Fatalf("first ContainerCreate returned unexpected error: %v", err)
	}

	_, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "dupe")
	if err == nil {
		t.Fatal("expected a name-conflict error on the second create, got nil")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("second create err = %v, want errdefs.IsConflict", err)
	}

	// Only the first container exists.
	out, err := f.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		t.Fatalf("ContainerList returned unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("container count = %d, want 1 (the conflicting create recorded nothing)", len(out))
	}

	// A distinct name still creates fine.
	if _, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "other"); err != nil {
		t.Errorf("create with a distinct name returned unexpected error: %v", err)
	}
}

// TestFakeClientExecStreamMalformedTar proves a malformed archive reports a
// non-zero exit (like `tar -x` failing) rather than silently succeeding, and
// writes nothing into the tmpfs (NEW-5).
func TestFakeClientExecStreamMalformedTar(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "c1")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if err := f.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart returned unexpected error: %v", err)
	}

	cmd := []string{"tar", "-x", "-p", "-C", "/run/garm"}
	code, err := f.ExecStream(ctx, created.ID, cmd, strings.NewReader("this is not a tar archive"))
	if err != nil {
		t.Fatalf("ExecStream returned unexpected transport error: %v", err)
	}
	if code == 0 {
		t.Error("a malformed tar must report a non-zero exit, got 0")
	}
	if len(f.Tmpfs(created.ID)) != 0 {
		t.Errorf("a malformed tar must write nothing, got tmpfs %v", f.Tmpfs(created.ID))
	}
}

// TestFakeClientExecStreamRejectsWrongCommand proves the fake only models the
// EXACT credential-delivery argv (`tar -x -p -C /run/garm`) and surfaces any
// other command shape as an error rather than pretending to run it (NEW-5).
// This includes degenerate shapes a looser "-x and -C <dir> appear somewhere"
// scan would have wrongly accepted: isCredentialTarExtract requires an exact
// argv match, not a scan.
func TestFakeClientExecStreamRejectsWrongCommand(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.ContainerCreate(ctx, &container.Config{}, nil, nil, nil, "c1")
	if err != nil {
		t.Fatalf("ContainerCreate returned unexpected error: %v", err)
	}
	if err := f.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart returned unexpected error: %v", err)
	}

	cases := map[string][]string{
		"non-tar command entirely":             {"sh", "-c", "rm -rf /"},
		"tar extraction into the wrong dir":    {"tar", "-x", "-C", "/tmp"},
		"missing -p (old loose scan accepted)": {"tar", "-x", "-C", "/run/garm"},
		"extra trailing argument":              {"tar", "-x", "-p", "-C", "/run/garm", "--checkpoint-action=exec=sh -c evil"},
		"reordered flags":                      {"tar", "-C", "/run/garm", "-x", "-p"},
	}
	for name, cmd := range cases {
		if _, err := f.ExecStream(ctx, created.ID, cmd, strings.NewReader("")); err == nil {
			t.Errorf("%s: expected ExecStream to reject %v, got nil", name, cmd)
		}
	}
}

// tarBytes builds a tar archive from a name→contents map for exec-stdin
// tests.
func tarBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
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
