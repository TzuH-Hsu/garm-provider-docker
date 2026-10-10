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
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
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
	return runLiveContainerWithLogDriver(t, p, script, "")
}

// runLiveContainerWithLogDriver is runLiveContainer with an explicit log
// driver ("" keeps the daemon default).
func runLiveContainerWithLogDriver(t *testing.T, p *Provider, script, logDriver string) string {
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
	}, &container.HostConfig{LogConfig: container.LogConfig{Type: logDriver}}, nil, nil, name)
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
	ok, _, err := p.waitRunnerListening(context.Background(), id, 30*time.Second)
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
	ok, _, err := p.waitRunnerListening(context.Background(), id, 30*time.Second)
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
	ok, _, err := p.waitRunnerListening(context.Background(), id, 2*time.Second)
	if ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitRunnerListening = %v, %v; want false, deadline exceeded", ok, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("a 2s wait took %v", d)
	}
}

// TestLiveContainerLogsNoneDriverIsNotImplemented records what the real daemon
// returns for the "none" log driver, which docker.FakeClient models: no stream,
// an errdefs.NotImplemented error "configured logging driver does not support
// reading" (HTTP 501).
func TestLiveContainerLogsNoneDriverIsNotImplemented(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainerWithLogDriver(t, p, `echo hidden; sleep 30`, "none")

	rc, err := p.cli.ContainerLogs(context.Background(), id, container.LogsOptions{ShowStdout: true, Follow: true})
	if rc != nil {
		_ = rc.Close()
	}
	t.Logf("ContainerLogs with log driver none: %v", err)
	if err == nil || !errdefs.IsNotImplemented(err) {
		t.Fatalf("ContainerLogs error = %v, want errdefs.NotImplemented", err)
	}
}

// TestLiveContainerTopOfStoppedContainerIsConflict pins the 409 the fallback
// treats as "the runner stopped".
func TestLiveContainerTopOfStoppedContainerIsConflict(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `exit 0`)
	time.Sleep(time.Second)
	_, err := p.cli.ContainerTop(context.Background(), id, nil)
	t.Logf("ContainerTop of a stopped container: %v", err)
	if !errdefs.IsConflict(err) {
		t.Fatalf("ContainerTop error = %v, want errdefs.Conflict", err)
	}
}

// TestLiveWaitRunnerListeningFallsBackWhenLogsUnreadable drives the production
// detector against a "none"-driver container whose process list gains a
// Runner.Listener command line after a second.
func TestLiveWaitRunnerListeningFallsBackWhenLogsUnreadable(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainerWithLogDriver(t, p, `sleep 1; n=Runner; sh -c "sleep 60; : /actions-runner/bin/$n.Listener run"`, "none")

	start := time.Now()
	ok, _, err := p.waitRunnerListening(context.Background(), id, 30*time.Second)
	if !ok {
		t.Fatalf("waitRunnerListening = false (%v), want true via the process fallback", err)
	}
	// The listener command line only exists from t≈1s (the outer shell's own
	// argv spells it "$n.Listener"), so a match needs 1s plus the settle time.
	if d := time.Since(start); d < listenerSettle+500*time.Millisecond {
		t.Errorf("matched after %v, before the listener had been up for the %v settle time", d, listenerSettle)
	}
}

func TestLiveWaitRunnerListeningFallbackTimesOutWithoutListener(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainerWithLogDriver(t, p, `sleep 60`, "none")

	ok, _, err := p.waitRunnerListening(context.Background(), id, 3*time.Second)
	if ok || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitRunnerListening = %v, %v; want false, deadline exceeded", ok, err)
	}
}

// TestLiveDetachedWatcherReportsIdle runs the real handoff against a real
// daemon: the provider (this test binary, via TestMain) starts the detached
// watcher and returns at once; the watcher, in its own session and with the
// request only on its stdin, follows the runner's logs and reports idle.
func TestLiveDetachedWatcherReportsIdle(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `sleep 3; echo "$(date -u '+%Y-%m-%d %H:%M:%SZ'): Listening for Jobs"; sleep 60`)
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	idle := make(chan struct{})
	var once sync.Once
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "idle" {
			once.Do(func() { close(idle) })
		}
		return 0
	})

	start := time.Now()
	if err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "live", ContainerID: id, DockerHost: host,
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(60 * time.Second),
	}); err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("startDetachedWatcher took %v", d)
	}

	// While it waits, the watcher is its own session/process-group leader and
	// its command line carries no request data.
	time.Sleep(500 * time.Millisecond)
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, StatusWatcherArg) {
			continue
		}
		found = true
		f := strings.Fields(line)
		if len(f) < 3 || f[0] != f[1] {
			t.Errorf("watcher is not a process-group leader: %q", line)
		}
		if strings.Contains(line, testInstanceToken) || strings.Contains(line, cb.URL) {
			t.Errorf("watcher command line exposes request data: %q", line)
		}
	}
	if !found {
		t.Error("no watcher process found while it should be waiting for the runner")
	}

	select {
	case <-idle:
	case <-time.After(30 * time.Second):
		t.Fatalf("the detached watcher never reported idle (reports: %v)", rec.seen())
	}
	if d := time.Since(start); d < 2*time.Second {
		t.Errorf("idle after %v, before the runner printed the listening line", d)
	}
}

// TestLiveStoppedContainerLogsReplayThenEOF pins what docker.FakeClient
// models and why readiness is confirmed against the container state: a
// container that printed the listening line and exited still returns that
// buffered line from a follow request, then EOF — so the marker alone would
// report a dead runner idle.
func TestLiveStoppedContainerLogsReplayThenEOF(t *testing.T) {
	p := newLiveProvider(t)
	id := runLiveContainer(t, p, `echo "2026-10-10 17:45:14Z: Listening for Jobs"; exit 0`)
	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rc, err := p.cli.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		t.Fatalf("ContainerLogs: %v", err)
	}
	var stdout bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, io.Discard, rc)
	_ = rc.Close()
	if err != nil || ctx.Err() != nil {
		t.Fatalf("follow of a stopped container did not end with EOF: %v (ctx %v)", err, ctx.Err())
	}
	if !strings.Contains(stdout.String(), "Listening for Jobs") {
		t.Fatalf("stopped container's logs = %q, want the buffered listening line", stdout.String())
	}

	ok, _, err := p.waitRunnerListening(context.Background(), id, 10*time.Second)
	if ok || !errors.Is(err, errRunnerStopped) {
		t.Fatalf("waitRunnerListening = %v, %v; want false, errRunnerStopped for an exited runner", ok, err)
	}
}

// TestLivePID1WithoutInitLeavesZombies grounds the reaper check on a real
// daemon: in a container whose PID 1 is not an init (here `sleep`, which
// never waits for children), an orphaned process stays a zombie after it
// exits, and /proc/1/comm names that process; with --init, PID 1 is
// docker-init and the orphan is reaped.
func TestLivePID1WithoutInitLeavesZombies(t *testing.T) {
	// The subshell starts a short sleep and exits, orphaning it to PID 1;
	// PID 1 then execs a long sleep.
	const script = `(sleep 0.2 &); exec sleep 30`
	for _, tt := range []struct {
		name       string
		args       []string
		wantComm   string
		wantReaped bool
		wantZombie bool
	}{
		{"without init", nil, "sleep", false, true},
		{"with --init", []string{"--init"}, "docker-init", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"run", "-d"}, tt.args...)
			args = append(args, liveImage, "sh", "-c", script)
			out, err := exec.Command("docker", args...).CombinedOutput()
			if err != nil {
				t.Fatalf("docker run: %v\n%s", err, out)
			}
			id := strings.TrimSpace(string(out))
			t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
			time.Sleep(1500 * time.Millisecond)

			out, err = exec.Command("docker", "exec", id, "sh", "-c", `cat /proc/1/comm; ps -o stat | grep -c "^Z" || true`).CombinedOutput()
			if err != nil {
				t.Fatalf("docker exec: %v\n%s", err, out)
			}
			fields := strings.Fields(string(out))
			if len(fields) != 2 {
				t.Fatalf("unexpected output %q", out)
			}
			comm, zombies := fields[0], fields[1]
			t.Logf("PID 1 = %s, zombies = %s", comm, zombies)
			if comm != tt.wantComm {
				t.Errorf("/proc/1/comm = %q, want %q", comm, tt.wantComm)
			}
			if got := reaperVerdict(comm+"\n", nil); got != tt.wantReaped {
				t.Errorf("reaperVerdict(%q) = %v, want %v", comm, got, tt.wantReaped)
			}
			if (zombies != "0") != tt.wantZombie {
				t.Errorf("zombie count = %s, want zombies=%v", zombies, tt.wantZombie)
			}
		})
	}
}
