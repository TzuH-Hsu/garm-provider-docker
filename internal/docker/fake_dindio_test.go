package docker

import (
	"bytes"
	"context"
	"io"
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

	f.SetState(id, "exited", false)
	if _, err := f.ContainerStatsOneShot(ctx, id); !errdefs.IsConflict(err) {
		t.Fatalf("stopped container err = %v, want Conflict", err)
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
