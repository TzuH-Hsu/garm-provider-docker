package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

// startFakeContainer creates and starts a container with the given TTY mode.
func startFakeContainer(t *testing.T, f *FakeClient, name string, tty bool) string {
	t.Helper()
	created, err := f.ContainerCreate(context.Background(), &container.Config{Image: "img", Tty: tty}, nil, nil, nil, name)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := f.ContainerStart(context.Background(), created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return created.ID
}

// TestFakeContainerLogsNonTTYIsStdcopyMultiplexed pins the real daemon's wire
// format for a non-TTY container (this provider's runner): each log line is one
// frame with an 8-byte header [stream, 0, 0, 0, uint32 big-endian length], and
// stdcopy.StdCopy splits stdout from stderr.
func TestFakeContainerLogsNonTTYIsStdcopyMultiplexed(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "runner", false)
	f.AppendLog(id, LogStdout, "hello")
	f.AppendLog(id, LogStderr, "oops")

	rc, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}

	wantFirst := make([]byte, 8)
	wantFirst[0] = byte(stdcopy.Stdout)
	binary.BigEndian.PutUint32(wantFirst[4:], uint32(len("hello\n")))
	if !bytes.HasPrefix(raw, wantFirst) {
		t.Fatalf("first frame header = %v, want %v", raw[:min(8, len(raw))], wantFirst)
	}

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(raw)); err != nil {
		t.Fatalf("StdCopy: %v", err)
	}
	if stdout.String() != "hello\n" || stderr.String() != "oops\n" {
		t.Fatalf("demuxed stdout=%q stderr=%q, want %q / %q", stdout.String(), stderr.String(), "hello\n", "oops\n")
	}
}

func TestFakeContainerLogsTTYIsRaw(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "tty", true)
	f.AppendLog(id, LogStdout, "plain")

	rc, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	raw, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(raw) != "plain\n" {
		t.Fatalf("TTY logs = %q, want raw %q", raw, "plain\n")
	}
}

func TestFakeContainerLogsStreamSelection(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "runner", false)
	f.AppendLog(id, LogStdout, "out")
	f.AppendLog(id, LogStderr, "err")

	rc, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	var stdout, stderr bytes.Buffer
	_, _ = stdcopy.StdCopy(&stdout, &stderr, rc)
	_ = rc.Close()
	if stdout.String() != "out\n" || stderr.Len() != 0 {
		t.Fatalf("ShowStdout-only gave stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	if _, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{}); err == nil {
		t.Fatal("ContainerLogs with neither stream selected succeeded; the daemon rejects it")
	}
	if _, err := f.ContainerLogs(context.Background(), "missing", container.LogsOptions{ShowStdout: true}); !errdefs.IsNotFound(err) {
		t.Fatalf("ContainerLogs of a missing container error = %v, want NotFound", err)
	}
}

// TestFakeContainerLogsFollowStreamsUntilStop models `docker logs -f`: lines
// printed after the call arrive on the open stream, which ends (EOF) when the
// container stops.
func TestFakeContainerLogsFollowStreamsUntilStop(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "runner", false)
	f.AppendLog(id, LogStdout, "early")

	rc, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer rc.Close()

	done := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		_, _ = stdcopy.StdCopy(&out, io.Discard, rc)
		done <- out.String()
	}()

	f.AppendLog(id, LogStdout, "late")
	select {
	case <-done:
		t.Fatal("follow stream ended while the container is still running")
	case <-time.After(50 * time.Millisecond):
	}
	if err := f.ContainerStop(context.Background(), id, container.StopOptions{}); err != nil {
		t.Fatalf("ContainerStop: %v", err)
	}
	select {
	case got := <-done:
		if got != "early\nlate\n" {
			t.Fatalf("followed output = %q, want %q", got, "early\nlate\n")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow stream did not end after the container stopped")
	}
}

// TestFakeContainerLogsFollowEndsOnCancel models the SDK's cancelled request:
// the body read fails with the context error.
func TestFakeContainerLogsFollowEndsOnCancel(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "runner", false)

	ctx, cancel := context.WithCancel(context.Background())
	rc, err := f.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	defer rc.Close()
	cancel()
	_, err = io.ReadAll(rc)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read after cancel error = %v, want context.Canceled", err)
	}
}

func TestFakeContainerLogsFollowOfStoppedContainerEnds(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "runner", false)
	f.AppendLog(id, LogStdout, "bye")
	f.SetState(id, "exited", false)

	rc, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	var out strings.Builder
	_, err = stdcopy.StdCopy(&out, io.Discard, rc)
	_ = rc.Close()
	if err != nil || out.String() != "bye\n" {
		t.Fatalf("follow of an exited container = %q, %v; want %q and EOF", out.String(), err, "bye\n")
	}
}

// TestFakeContainerLogsNoneDriverIsNotImplemented models what a real daemon
// returns for a container using the "none" log driver: HTTP 501, surfaced by
// the SDK as errdefs.NotImplemented, and no stream.
func TestFakeContainerLogsNoneDriverIsNotImplemented(t *testing.T) {
	f := NewFakeClient()
	f.DefaultLogDriver = "none"
	id := startFakeContainer(t, f, "runner", false)

	rc, err := f.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true, Follow: true})
	if rc != nil {
		_ = rc.Close()
		t.Fatal("ContainerLogs returned a stream for a container using the none log driver")
	}
	if !errdefs.IsNotImplemented(err) || !strings.Contains(err.Error(), "does not support reading") {
		t.Fatalf("ContainerLogs error = %v, want NotImplemented \"configured logging driver does not support reading\"", err)
	}
}

func TestFakeContainerTop(t *testing.T) {
	f := NewFakeClient()
	id := startFakeContainer(t, f, "runner", false)
	f.SetProcesses(id, "/bin/bash /actions-runner/run.sh", "/actions-runner/bin/Runner.Listener run")

	top, err := f.ContainerTop(context.Background(), id, nil)
	if err != nil {
		t.Fatalf("ContainerTop: %v", err)
	}
	if len(top.Titles) == 0 || top.Titles[len(top.Titles)-1] != "CMD" || len(top.Processes) != 2 {
		t.Fatalf("ContainerTop = %+v, want two rows under titles ending in CMD", top)
	}
	if got := top.Processes[1][len(top.Titles)-1]; got != "/actions-runner/bin/Runner.Listener run" {
		t.Errorf("CMD column = %q", got)
	}

	f.SetState(id, "exited", false)
	if _, err := f.ContainerTop(context.Background(), id, nil); !errdefs.IsConflict(err) {
		t.Errorf("ContainerTop of a stopped container error = %v, want Conflict", err)
	}
	if _, err := f.ContainerTop(context.Background(), "missing", nil); !errdefs.IsNotFound(err) {
		t.Errorf("ContainerTop of a missing container error = %v, want NotFound", err)
	}
}
