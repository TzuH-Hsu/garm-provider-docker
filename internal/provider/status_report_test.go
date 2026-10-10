package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// statusCallbacks records what a test stand-in for GARM's callback API saw.
type statusCallbacks struct {
	mu       sync.Mutex
	statuses []string
	bodies   []map[string]any
	auths    []string
	paths    []string
}

func (s *statusCallbacks) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.statuses...)
}

// newStatusCallbackServer stands in for GARM's /api/v1/callbacks/status. For
// every report it records the request and then calls onStatus (if set), which
// returns the HTTP status to answer with (0 means 200).
func newStatusCallbackServer(t *testing.T, onStatus func(status string) int) (*httptest.Server, *statusCallbacks) {
	t.Helper()
	rec := &statusCallbacks{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("status body is not JSON: %q", raw)
		}
		status, _ := body["status"].(string)
		rec.mu.Lock()
		rec.statuses = append(rec.statuses, status)
		rec.bodies = append(rec.bodies, body)
		rec.auths = append(rec.auths, r.Header.Get("Authorization"))
		rec.paths = append(rec.paths, r.Method+" "+r.URL.Path)
		rec.mu.Unlock()
		code := http.StatusOK
		if onStatus != nil {
			if c := onStatus(status); c != 0 {
				code = c
			}
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// statusBootstrap is a JIT bootstrap with GARM's callback URL set.
func statusBootstrap(metadataURL, callbackBase string) params.BootstrapInstance {
	b := jitBootstrap(metadataURL)
	b.CallbackURL = callbackBase + "/api/v1/callbacks"
	return b
}

// printRunnerOnline appends what a real runner container prints on the way to
// being online (abridged from a live gate runner's `docker logs`), ending with
// the actions/runner listening line on stdout.
func printRunnerOnline(fake *docker.FakeClient, runner string) {
	fake.AppendLog(runner, docker.LogStdout, "[entrypoint] waiting up to 120s for the delivery marker /run/garm/.delivered")
	fake.AppendLog(runner, docker.LogStdout, "[entrypoint] dropping privileges to runner via gosu")
	fake.AppendLog(runner, docker.LogStdout, "")
	fake.AppendLog(runner, docker.LogStdout, "√ Connected to GitHub")
	fake.AppendLog(runner, docker.LogStdout, "Current runner version: '2.336.0'")
	fake.AppendLog(runner, docker.LogStdout, "2026-10-10 17:45:14Z: Listening for Jobs")
}

func intPtr(v int) *int { return &v }

// captureLogs swaps slog's default logger for a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestCreateInstanceReportsInstallingThenIdle is the happy path: installing is
// reported once credentials are delivered, and idle only after the runner has
// printed "Listening for Jobs" — each with the instance token as Bearer, GARM's
// InstanceUpdateMessage body, and no agent_id. Neither the token nor the
// callback URL reaches the runner container or the logs.
func TestCreateInstanceReportsInstallingThenIdle(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	b := statusBootstrap(meta.URL, "")
	runner := spec.RunnerContainerName(b.Name)

	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			// The runner comes online only after the provider reported it
			// installing, so idle cannot have been reported early.
			printRunnerOnline(fake, runner)
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"
	logs := captureLogs(t)

	inst, err := p.CreateInstance(context.Background(), b)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if inst.Status != params.InstanceRunning {
		t.Errorf("instance status = %q, want running", inst.Status)
	}

	if got := rec.seen(); strings.Join(got, ",") != "installing,idle" {
		t.Fatalf("reported statuses = %v, want [installing idle]", got)
	}
	rec.mu.Lock()
	for i := range rec.statuses {
		if rec.paths[i] != "POST /api/v1/callbacks/status" {
			t.Errorf("report %d went to %q, want POST /api/v1/callbacks/status", i, rec.paths[i])
		}
		if rec.auths[i] != testMetadataToken {
			t.Errorf("report %d Authorization = %q, want the Bearer instance token", i, rec.auths[i])
		}
		if _, ok := rec.bodies[i]["agent_id"]; ok {
			t.Errorf("report %d carries agent_id: %v", i, rec.bodies[i])
		}
		if msg, _ := rec.bodies[i]["message"].(string); msg == "" {
			t.Errorf("report %d has no message: %v", i, rec.bodies[i])
		}
	}
	rec.mu.Unlock()

	// The container is unchanged by status reporting: no env or label carries
	// the token or the callback URL.
	info, err := fake.ContainerInspect(context.Background(), runner)
	if err != nil {
		t.Fatalf("ContainerInspect: %v", err)
	}
	for _, e := range info.Config.Env {
		if strings.Contains(e, testInstanceToken) || strings.Contains(e, cb.URL) {
			t.Errorf("runner env carries the token or callback URL: %q", e)
		}
	}
	for k, v := range info.Config.Labels {
		if strings.Contains(v, testInstanceToken) || strings.Contains(v, cb.URL) {
			t.Errorf("runner label %s carries the token or callback URL: %q", k, v)
		}
	}
	if out := logs.String(); strings.Contains(out, testInstanceToken) {
		t.Errorf("REDACTION FAILURE: logs contain the instance token:\n%s", out)
	}
}

// TestCreateInstanceStatusDeadlineStaysInstalling: a runner that never prints
// the listening line is left "installing" (so GARM's bootstrap timeout still
// reaps it), and CreateInstance returns success after the bounded wait.
func TestCreateInstanceStatusDeadlineStaysInstalling(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, rec := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)
	p.cfg.StatusReport.TimeoutSeconds = intPtr(1)

	start := time.Now()
	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	elapsed := time.Since(start)

	if got := rec.seen(); strings.Join(got, ",") != "installing" {
		t.Fatalf("reported statuses = %v, want only [installing]", got)
	}
	if elapsed < time.Second || elapsed > 10*time.Second {
		t.Errorf("CreateInstance took %v, want about the 1s status_report timeout", elapsed)
	}
}

// TestCreateInstanceStatusIgnoresListeningLineOnStderr: detection reads the
// runner's demultiplexed stdout, where actions/runner prints the line.
func TestCreateInstanceStatusIgnoresListeningLineOnStderr(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	p.cfg.StatusReport.TimeoutSeconds = intPtr(1)
	b := statusBootstrap(meta.URL, "")
	runner := spec.RunnerContainerName(b.Name)
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			fake.AppendLog(runner, docker.LogStderr, "2026-10-10 17:45:14Z: Listening for Jobs")
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"

	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got := rec.seen(); strings.Join(got, ",") != "installing" {
		t.Fatalf("reported statuses = %v, want only [installing]", got)
	}
}

// TestCreateInstanceStatusRunnerExitsBeforeListening: if the runner container
// stops before it is listening, the log stream ends and the wait returns at
// once, leaving the runner installing for GARM to reap.
func TestCreateInstanceStatusRunnerExitsBeforeListening(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	p.cfg.StatusReport.TimeoutSeconds = intPtr(60)
	b := statusBootstrap(meta.URL, "")
	runner := spec.RunnerContainerName(b.Name)
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			fake.AppendLog(runner, docker.LogStdout, "[entrypoint] timed out waiting for the Docker daemon")
			info, err := fake.ContainerInspect(context.Background(), runner)
			if err != nil {
				t.Errorf("ContainerInspect: %v", err)
				return 0
			}
			fake.SetState(info.ID, "exited", false) // SetState is keyed by ID
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"

	start := time.Now()
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("CreateInstance waited %v for a runner that had already exited", d)
	}
	if got := rec.seen(); strings.Join(got, ",") != "installing" {
		t.Fatalf("reported statuses = %v, want only [installing]", got)
	}
}

// TestCreateInstanceStatusFailureDoesNotFailCreate: a callback endpoint that
// rejects or errors never fails (or rolls back) the allocation, and a failed
// installing report skips the wait and the idle report.
func TestCreateInstanceStatusFailureDoesNotFailCreate(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			meta := newJITMetadataServer(t)
			defer meta.Close()
			cb, rec := newStatusCallbackServer(t, func(string) int { return code })
			p, fake := newTestProvider(t)

			start := time.Now()
			inst, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL))
			if err != nil {
				t.Fatalf("CreateInstance failed because of status reporting: %v", err)
			}
			if inst.Status != params.InstanceRunning {
				t.Errorf("instance status = %q, want running", inst.Status)
			}
			for _, s := range rec.seen() {
				if s != "installing" {
					t.Errorf("reported %q after the installing report failed", s)
				}
			}
			if d := time.Since(start); d > 30*time.Second {
				t.Errorf("CreateInstance took %v; a failed installing report must not wait for the runner", d)
			}
			if _, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName(inst.Name)); err != nil {
				t.Errorf("runner container was not kept after a status-report failure: %v", err)
			}
		})
	}
}

func TestCreateInstanceStatusUnreachableCallbackDoesNotFailCreate(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb := httptest.NewServer(http.NotFoundHandler())
	dead := cb.URL
	cb.Close() // nothing listens there any more: connection refused

	p, _ := newTestProvider(t)
	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, dead)); err != nil {
		t.Fatalf("CreateInstance failed because the callback was unreachable: %v", err)
	}
}

func TestCreateInstanceStatusDisabled(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, rec := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)
	p.cfg.StatusReport.TimeoutSeconds = intPtr(0)

	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("status reporting is disabled but %v was reported", got)
	}
}

// TestCreateInstanceStatusRejectsCleartextCallback: the token is never sent
// over plain http to a non-loopback callback URL; the create still succeeds.
func TestCreateInstanceStatusRejectsCleartextCallback(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, _ := newTestProvider(t)
	logs := captureLogs(t)

	b := jitBootstrap(meta.URL)
	b.CallbackURL = "http://garm.example.com/api/v1/callbacks"
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if !strings.Contains(logs.String(), "invalid callback URL") {
		t.Errorf("expected an invalid-callback-URL warning, logs:\n%s", logs.String())
	}
}

// TestCreateInstanceStatusFailureLogsNoToken: a transport failure whose error
// embeds a token-bearing callback URL is logged with the token scrubbed.
func TestCreateInstanceStatusFailureLogsNoToken(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	}))
	defer cb.Close()
	p, _ := newTestProvider(t)
	logs := captureLogs(t)

	b := jitBootstrap(meta.URL)
	b.CallbackURL = cb.URL + "/" + testInstanceToken + "/callbacks"
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "failed to report runner status installing") {
		t.Fatalf("expected the installing-report failure to be logged, logs:\n%s", out)
	}
	if strings.Contains(out, testInstanceToken) {
		t.Fatalf("REDACTION FAILURE: logs contain the instance token:\n%s", out)
	}
}

// fastListenerFallback shortens the process-list fallback's timing.
func fastListenerFallback(t *testing.T) {
	t.Helper()
	prevPoll, prevSettle := listenerPollInterval, listenerSettle
	listenerPollInterval, listenerSettle = 20*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { listenerPollInterval, listenerSettle = prevPoll, prevSettle })
}

// TestCreateInstanceStatusNonJITSkipsReporting: a non-JIT runner registers
// inside the container, so GARM has no agent ID for it and its own bootstrap
// would send one with idle; an ID-less report would leave the row without one.
// Nothing is reported for non-JIT runners.
func TestCreateInstanceStatusNonJITSkipsReporting(t *testing.T) {
	meta := newRegistrationTokenServer(t)
	defer meta.Close()
	cb, rec := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)

	b := statusBootstrap(meta.URL, cb.URL)
	b.JitConfigEnabled = false
	start := time.Now()
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("non-JIT runner had %v reported, want nothing", got)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("CreateInstance took %v for a non-JIT runner; it must not wait", d)
	}
}

// TestCreateInstanceStatusLogsUnreadableFallsBackToListenerProcess: with the
// "none" log driver the daemon cannot return the runner's logs, so readiness
// falls back to the Runner.Listener process staying up for the settle time.
func TestCreateInstanceStatusLogsUnreadableFallsBackToListenerProcess(t *testing.T) {
	fastListenerFallback(t)
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	fake.DefaultLogDriver = "none"
	b := statusBootstrap(meta.URL, "")
	runner := spec.RunnerContainerName(b.Name)
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			fake.SetProcesses(runner, "/bin/bash /actions-runner/run.sh", "/actions-runner/bin/Runner.Listener run")
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"
	logs := captureLogs(t)

	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got := rec.seen(); strings.Join(got, ",") != "installing,idle" {
		t.Fatalf("reported statuses = %v, want [installing idle]", got)
	}
	out := logs.String()
	if !strings.Contains(out, "cannot read the runner's logs") || !strings.Contains(out, "json-file") {
		t.Errorf("expected a warning recommending a readable log driver, logs:\n%s", out)
	}
}

// TestCreateInstanceStatusLogsUnreadableListenerNeverAppears: the fallback
// keeps the overall bound, and without a listener the runner stays installing.
func TestCreateInstanceStatusLogsUnreadableListenerNeverAppears(t *testing.T) {
	fastListenerFallback(t)
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	fake.DefaultLogDriver = "none"
	p.cfg.StatusReport.TimeoutSeconds = intPtr(1)
	b := statusBootstrap(meta.URL, "")
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			fake.SetProcesses(spec.RunnerContainerName(b.Name), "/bin/bash /entrypoint.sh", "sleep 5")
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"

	start := time.Now()
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got := rec.seen(); strings.Join(got, ",") != "installing" {
		t.Fatalf("reported statuses = %v, want only [installing]", got)
	}
	if d := time.Since(start); d < time.Second || d > 10*time.Second {
		t.Errorf("CreateInstance took %v, want about the 1s status_report timeout", d)
	}
}

// TestWaitListenerProcessRequiresSettle: a listener that appears and then
// disappears (a crash loop) does not count; the container stopping ends the wait.
func TestWaitListenerProcessRequiresSettle(t *testing.T) {
	fastListenerFallback(t)
	p, fake := newTestProvider(t)
	created, err := fake.ContainerCreate(context.Background(), &container.Config{Image: "img"}, nil, nil, nil, "flappy")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := fake.ContainerStart(context.Background(), created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	go func() {
		for i := 0; i < 4; i++ { // up for less than the settle time, then gone
			fake.SetProcesses(created.ID, "/actions-runner/bin/Runner.Listener run")
			time.Sleep(listenerSettle / 4)
			fake.SetProcesses(created.ID)
			time.Sleep(listenerSettle / 4)
		}
		fake.SetState(created.ID, "exited", false)
	}()

	ok, err := p.waitListenerProcess(context.Background(), created.ID)
	if ok || !errors.Is(err, errRunnerStopped) {
		t.Fatalf("waitListenerProcess = %v, %v; want false, errRunnerStopped", ok, err)
	}
}

func TestHasProcess(t *testing.T) {
	top := container.TopResponse{
		Titles:    []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"},
		Processes: [][]string{{"runner", "7", "1", "0", "00:00", "?", "00:00:00", "/actions-runner/bin/Runner.Listener run"}},
	}
	if !hasProcess(top, runnerListenerProcess) {
		t.Error("hasProcess missed Runner.Listener in the CMD column")
	}
	if hasProcess(container.TopResponse{Titles: []string{"PID"}, Processes: [][]string{{"Runner.Listener"}}}, runnerListenerProcess) {
		t.Error("hasProcess matched outside a CMD/COMMAND column")
	}
}
