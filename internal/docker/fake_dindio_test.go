package docker

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

func runningFakeContainer(t *testing.T, f *FakeClient) string {
	t.Helper()
	created, err := f.ContainerCreate(context.Background(), &container.Config{Image: "img"}, &container.HostConfig{}, nil, nil, "c1")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	f.SetState(created.ID, "running", false)
	return created.ID
}

func TestFakeContainerStatsOneShot(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	if _, err := f.ContainerStatsOneShot(ctx, "nope"); !errdefs.IsNotFound(err) {
		t.Fatalf("missing container err = %v, want NotFound", err)
	}

	id := runningFakeContainer(t, f)
	f.SetStats(id, `{"blkio_stats":{}}`)
	r, err := f.ContainerStatsOneShot(ctx, id)
	if err != nil {
		t.Fatalf("ContainerStatsOneShot: %v", err)
	}
	defer r.Body.Close()
	if body, _ := io.ReadAll(r.Body); string(body) != `{"blkio_stats":{}}` {
		t.Errorf("body = %q", body)
	}

	f.SetState(id, "paused", false)
	paused, err := f.ContainerStatsOneShot(ctx, id)
	if err != nil {
		t.Fatalf("ContainerStatsOneShot of a paused container: %v", err)
	}
	defer paused.Body.Close()
	if body, _ := io.ReadAll(paused.Body); string(body) != `{"blkio_stats":{}}` {
		t.Errorf("paused body = %q, want the sample", body)
	}

	for _, state := range []string{"exited", "restarting"} {
		f.SetState(id, state, false)
		stopped, err := f.ContainerStatsOneShot(ctx, id)
		if err != nil {
			t.Fatalf("%s container err = %v, want an empty sample", state, err)
		}
		body, _ := io.ReadAll(stopped.Body)
		stopped.Body.Close()
		if strings.Contains(string(body), "blkio_stats") || !strings.Contains(string(body), id) {
			t.Errorf("%s body = %q, want name and ID only", state, body)
		}
	}
}

// TestFakeStatsOfJoinedNetns pins the daemon behavior the DinD IO report must
// work around: a container in another container's network namespace gets an
// empty sample once that container is removed, and an error while it is
// stopped, although the joined container itself still runs.
func TestFakeStatsOfJoinedNetns(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()
	owner := runningFakeContainer(t, f)
	created, err := f.ContainerCreate(ctx, &container.Config{Image: "img"},
		&container.HostConfig{NetworkMode: container.NetworkMode("container:" + owner)}, nil, nil, "c2")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	joined := created.ID
	f.SetState(joined, "running", false)
	f.SetStats(joined, `{"blkio_stats":{}}`)

	read := func() (string, error) {
		r, err := f.ContainerStatsOneShot(ctx, joined)
		if err != nil {
			return "", err
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		return string(body), err
	}

	if body, err := read(); err != nil || body != `{"blkio_stats":{}}` {
		t.Fatalf("owner running: (%q, %v), want the set sample", body, err)
	}
	if err := f.ContainerStop(ctx, owner, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	if _, err := read(); !errdefs.IsInvalidParameter(err) {
		t.Fatalf("owner stopped: err = %v, want InvalidParameter", err)
	}
	if err := f.ContainerRemove(ctx, owner, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("ContainerRemove: %v", err)
	}
	body, err := read()
	if err != nil || strings.Contains(body, "blkio_stats") || !strings.Contains(body, joined) {
		t.Fatalf("owner removed: (%q, %v), want an empty sample naming the container", body, err)
	}
}

func TestFakeExecOutput(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()
	f.ExecOutputText, f.ExecOutputExit = "hello\n", 7

	if _, _, err := f.ExecOutput(ctx, "nope", []string{"true"}); !errdefs.IsNotFound(err) {
		t.Fatalf("missing container err = %v, want NotFound", err)
	}

	id := runningFakeContainer(t, f)
	out, code, err := f.ExecOutput(ctx, id, []string{"echo", "hello"})
	if err != nil || out != "hello\n" || code != 7 {
		t.Fatalf("ExecOutput = (%q, %d, %v), want (hello, 7, nil)", out, code, err)
	}
	if len(f.OutputExecs) != 1 || f.OutputExecs[0].ContainerID != id {
		t.Errorf("OutputExecs = %+v", f.OutputExecs)
	}

	f.SetState(id, "exited", false)
	if _, _, err := f.ExecOutput(ctx, id, []string{"true"}); err == nil {
		t.Fatal("ExecOutput on a stopped container should fail")
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{limit: 5}
	if n, err := b.Write([]byte("abc")); n != 3 || err != nil || b.overflowed {
		t.Fatalf("first write = (%d, %v), overflowed=%v", n, err, b.overflowed)
	}
	// The writer reports the full length even when it truncates, so the exec
	// output pump never sees a short write and aborts.
	if n, err := b.Write([]byte("defg")); n != 4 || err != nil {
		t.Fatalf("overflowing write = (%d, %v), want (4, nil)", n, err)
	}
	if !b.overflowed || b.b.String() != "abcde" {
		t.Errorf("buffer = %q overflowed=%v, want abcde/true", b.b.String(), b.overflowed)
	}
	if n, _ := b.Write([]byte("h")); n != 1 || b.b.String() != "abcde" {
		t.Errorf("write after full: n=%d buffer=%q", n, b.b.String())
	}
}

// TestStreamExecToCapturesOutput proves the exec output is demultiplexed from
// the stdcopy framing and handed to the caller's writer.
func TestStreamExecToCapturesOutput(t *testing.T) {
	conn := newPipeExecConn()
	go func() {
		w := stdcopy.NewStdWriter(conn.outPeer, stdcopy.Stdout)
		_, _ = w.Write([]byte("hel"))
		_, _ = w.Write([]byte("lo"))
		conn.outPeer.Close()
	}()

	var out bytes.Buffer
	if err := streamExecTo(context.Background(), "c1", conn, nil, &out); err != nil {
		t.Fatalf("streamExecTo: %v", err)
	}
	if out.String() != "hello" {
		t.Errorf("captured output = %q, want hello", out.String())
	}
	if got := conn.closes.Load(); got != 1 {
		t.Errorf("Close called %d times, want 1", got)
	}
}
