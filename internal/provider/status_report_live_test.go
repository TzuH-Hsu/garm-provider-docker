//go:build dockerverify

package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
)

// liveImage is a small image the dockerverify suite already uses.
const liveImage = "alpine:3.20"

// newLiveProvider builds a Provider over the real daemon at $DOCKER_HOST (or
// the default local socket). Only the docker client is needed here.
func newLiveProvider(t *testing.T) *Provider {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	cli, err := docker.NewMobyClient(host)
	if err != nil {
		t.Fatalf("NewMobyClient: %v", err)
	}
	return &Provider{cli: cli}
}

// runLiveContainer starts a non-TTY container (as the runner is) running the
// shell script, and removes it when the test ends.
func runLiveContainer(t *testing.T, p *Provider, script string) string {
	t.Helper()
	ctx := context.Background()
	if err := p.ensureImage(ctx, liveImage); err != nil {
		t.Fatalf("ensureImage: %v", err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	name := "garm-status-live-" + hex.EncodeToString(b[:])
	created, err := p.cli.ContainerCreate(ctx, &container.Config{
		Image: liveImage,
		Cmd:   []string{"sh", "-c", script},
		Tty:   false,
	}, nil, nil, nil, name)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	t.Cleanup(func() {
		_ = p.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})
	})
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return created.ID
}

// TestLiveContainerLogsNonTTYIsMultiplexed confirms what docker.FakeClient
// models: a non-TTY container's log stream is stdcopy-framed (first byte is
// the stream id, 1 or 2), and StdCopy splits stdout from stderr.
func TestLiveContainerLogsNonTTYIsMultiplexed(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `echo out-line; echo err-line >&2; sleep 30`)
	time.Sleep(time.Second)

	rc, err := p.cli.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	raw, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("reading logs: %v", err)
	}
	if len(raw) < 8 || (raw[0] != byte(stdcopy.Stdout) && raw[0] != byte(stdcopy.Stderr)) || raw[1] != 0 || raw[2] != 0 || raw[3] != 0 {
		t.Fatalf("log stream is not stdcopy-framed: % x", raw[:min(16, len(raw))])
	}
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(raw)); err != nil {
		t.Fatalf("StdCopy: %v", err)
	}
	if stdout.String() != "out-line\n" || stderr.String() != "err-line\n" {
		t.Fatalf("demuxed stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// TestLiveWaitRunnerListening drives the production detector against a real
// daemon: a marker on stderr is ignored, the stdout marker printed later (as
// actions/runner prints it) is found by the follow stream.
func TestLiveWaitRunnerListening(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `echo "[entrypoint] starting"; echo "Listening for Jobs" >&2; sleep 2; echo "$(date -u '+%Y-%m-%d %H:%M:%SZ'): Listening for Jobs"; sleep 60`)

	start := time.Now()
	ok, err := p.waitRunnerListening(context.Background(), id, 30*time.Second)
	if !ok {
		t.Fatalf("waitRunnerListening = false (%v), want true", err)
	}
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Errorf("matched after %v, before the stdout marker was printed (stderr marker must be ignored)", d)
	}
}

func TestLiveWaitRunnerListeningStreamEndsWhenContainerExits(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `echo "[entrypoint] giving up"; sleep 1; exit 1`)

	start := time.Now()
	ok, err := p.waitRunnerListening(context.Background(), id, 30*time.Second)
	if ok || !errors.Is(err, errLogStreamEnded) {
		t.Fatalf("waitRunnerListening = %v, %v; want false, errLogStreamEnded", ok, err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("waited %v for a container that exited after 1s", d)
	}
}

func TestLiveWaitRunnerListeningTimesOut(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `echo "[entrypoint] waiting"; sleep 60`)

	start := time.Now()
	ok, err := p.waitRunnerListening(context.Background(), id, 2*time.Second)
	if ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitRunnerListening = %v, %v; want false, deadline exceeded", ok, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("a 2s wait took %v", d)
	}
}
