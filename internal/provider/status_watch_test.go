//go:build unix

package provider

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// TestMain lets this test binary stand in for the provider binary when
// startDetachedWatcher re-executes itself (os.Executable is the test binary).
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == StatusWatcherArg {
		os.Exit(RunStatusWatcher(os.Stdin))
	}
	os.Exit(m.Run())
}

func TestWatcherArgvCarriesNoRequestData(t *testing.T) {
	argv := watcherArgv("/opt/garm/providers.d/garm-provider-docker")
	if len(argv) != 2 || argv[0] != "/opt/garm/providers.d/garm-provider-docker" || argv[1] != StatusWatcherArg {
		t.Fatalf("watcher argv = %q, want exactly [binary %s]", argv, StatusWatcherArg)
	}
}

// TestWatcherProcAttr pins how the watcher is detached: request on a private
// stdin pipe, stdout/stderr on /dev/null (never GARM's pipes, which GARM reads
// to EOF), an empty environment, its own session, and "/" as working dir.
func TestWatcherProcAttr(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	attr := watcherProcAttr(pr, devNull)
	if attr.Env == nil || len(attr.Env) != 0 {
		t.Errorf("watcher env = %v, want empty and non-nil (nil would inherit the provider's environment)", attr.Env)
	}
	if len(attr.Files) != 3 || attr.Files[0] != pr || attr.Files[1] != devNull || attr.Files[2] != devNull {
		t.Fatalf("watcher files = %v, want [request pipe, /dev/null, /dev/null]", attr.Files)
	}
	for _, f := range attr.Files {
		if f == os.Stdout || f == os.Stderr || f == os.Stdin {
			t.Errorf("watcher inherits the provider's own stdio %s", f.Name())
		}
	}
	if attr.Dir != "/" {
		t.Errorf("watcher dir = %q, want /", attr.Dir)
	}
	if attr.Sys == nil || !attr.Sys.Setsid {
		t.Error("watcher is not started in its own session (Setsid)")
	}
}

func TestStatusWatchRequestValidate(t *testing.T) {
	now := time.Now()
	good := statusWatchRequest{
		Instance: "i", ContainerID: "c", DockerHost: "unix:///var/run/docker.sock",
		CallbackURL: "https://garm.example.com/api/v1/callbacks", InstanceToken: "tok",
		Deadline: now.Add(90 * time.Second),
	}
	if err := good.validate(now); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	past := good
	past.Deadline = now.Add(-time.Second)
	far := good
	far.Deadline = now.Add(config.MaxStatusReportTimeoutSeconds*time.Second + 2*time.Minute)
	noToken := good
	noToken.InstanceToken = ""
	for name, r := range map[string]statusWatchRequest{"past deadline": past, "deadline too far": far, "no token": noToken} {
		if err := r.validate(now); err == nil {
			t.Errorf("%s: request accepted, want rejection", name)
		}
	}
}

func TestRunStatusWatcherRejectsBadRequests(t *testing.T) {
	for name, in := range map[string]string{
		"not json":      "nope",
		"past deadline": `{"instance":"i","container_id":"c","docker_host":"unix:///x","callback_url":"https://g/cb","instance_token":"t","deadline":"2001-01-01T00:00:00Z"}`,
	} {
		start := time.Now()
		if code := RunStatusWatcher(strings.NewReader(in)); code == 0 {
			t.Errorf("%s: exit code 0, want a failure", name)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("%s: took %v, want an immediate exit", name, d)
		}
	}
}

// TestCreateInstanceDoesNotWaitForRunner is the core of the GARM exec-timeout
// fix: CreateInstance reports installing, hands the rest to the watcher, and
// returns at once even with the largest timeout and a runner that never comes
// online. The handoff carries the token, callback URL, container and deadline.
func TestCreateInstanceDoesNotWaitForRunner(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, rec := newStatusCallbackServer(t, nil)
	p, fake := newTestProvider(t)
	p.cfg.StatusReport.TimeoutSeconds = intPtr(config.MaxStatusReportTimeoutSeconds)
	var got []statusWatchRequest
	p.spawnStatusWatcher = func(_ context.Context, req statusWatchRequest) error {
		got = append(got, req)
		return nil
	}

	b := statusBootstrap(meta.URL, cb.URL)
	start := time.Now()
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("CreateInstance took %v; it must not wait for the runner", d)
	}
	if s := rec.seen(); strings.Join(s, ",") != "installing" {
		t.Fatalf("reported statuses = %v, want [installing] from CreateInstance itself", s)
	}
	if len(got) != 1 {
		t.Fatalf("watcher started %d times, want 1", len(got))
	}
	req := got[0]
	info, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName(b.Name))
	if err != nil {
		t.Fatal(err)
	}
	if req.ContainerID != info.ID || req.InstanceToken != testInstanceToken || req.CallbackURL != b.CallbackURL || req.DockerHost == "" || req.Instance != b.Name {
		t.Errorf("watcher request = %+v, missing fields", req)
	}
	if want := start.Add(config.MaxStatusReportTimeoutSeconds * time.Second); req.Deadline.Before(want.Add(-5*time.Second)) || req.Deadline.After(want.Add(5*time.Second)) {
		t.Errorf("watcher deadline = %v, want about %v", req.Deadline, want)
	}
}

func TestCreateInstanceWatcherStartFailureDoesNotFailCreate(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, _ := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)
	p.spawnStatusWatcher = func(context.Context, statusWatchRequest) error {
		return errors.New("cannot start watcher for token " + testInstanceToken)
	}
	logs := captureLogs(t)

	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL)); err != nil {
		t.Fatalf("CreateInstance failed because the watcher could not start: %v", err)
	}
	if out := logs.String(); !strings.Contains(out, "could not start the runner status watcher") || strings.Contains(out, testInstanceToken) {
		t.Errorf("want a redacted watcher-start warning, logs:\n%s", out)
	}
}

// startFakeRunner creates and starts a bare running container in the fake.
func startFakeRunner(t *testing.T, fake *docker.FakeClient) string {
	t.Helper()
	created, err := fake.ContainerCreate(context.Background(), &container.Config{Image: "img"}, nil, nil, nil, "watched-runner")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := fake.ContainerStart(context.Background(), created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return created.ID
}

// TestRunStatusWatchStopsAtDeadline: the watcher gives up at the request
// deadline, leaving the runner installing with an explanatory message.
func TestRunStatusWatchStopsAtDeadline(t *testing.T) {
	cb, rec := newStatusCallbackServer(t, nil)
	p, fake := newTestProvider(t)
	id := startFakeRunner(t, fake)

	start := time.Now()
	runStatusWatch(context.Background(), p, statusWatchRequest{
		Instance: "i", ContainerID: id, DockerHost: "unix:///unused",
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(300 * time.Millisecond),
	})
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("watcher ran %v past a 300ms deadline", d)
	}
	if s := rec.seen(); strings.Join(s, ",") != "installing" || rec.lastMessage() != statusMessageNotListening {
		t.Fatalf("reports = %v / %q, want one installing with the not-listening message", s, rec.lastMessage())
	}
}

// fakeDockerAPI is a minimal Docker Engine API for the detached watcher: it
// answers the SDK's version negotiation and serves one container's logs as a
// real daemon does for a non-TTY container (stdcopy frames, stream kept open).
func fakeDockerAPI(t *testing.T, containerID, line string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/"+containerID+"/logs"):
			w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
			payload := []byte(line + "\n")
			hdr := make([]byte, 8)
			hdr[0] = byte(stdcopy.Stdout)
			binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
			_, _ = w.Write(append(hdr, payload...))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done() // a follow stream stays open
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDetachedWatcherReportsIdleAfterParentReturns exercises the real
// handoff: the provider re-executes itself (here, the test binary via
// TestMain) as a detached watcher, returns as soon as the request is written,
// and the watcher — reading the request from its stdin pipe — follows the
// runner's logs through the Docker API and reports idle to GARM on its own.
func TestDetachedWatcherReportsIdleAfterParentReturns(t *testing.T) {
	const containerID = "watcher-e2e-container"
	dockerAPI := fakeDockerAPI(t, containerID, "2026-10-10 17:45:14Z: Listening for Jobs")
	idle := make(chan struct{})
	var once sync.Once
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "idle" {
			once.Do(func() { close(idle) })
		}
		return 0
	})

	start := time.Now()
	err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "e2e", ContainerID: containerID,
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"),
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(30 * time.Second),
	})
	if err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("startDetachedWatcher took %v; it must return once the request is handed over", d)
	}
	select {
	case <-idle:
	case <-time.After(20 * time.Second):
		t.Fatalf("the detached watcher never reported idle (reports so far: %v)", rec.seen())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.auths) == 0 || rec.auths[len(rec.auths)-1] != testMetadataToken {
		t.Errorf("idle report Authorization = %v, want the instance token from the stdin request", rec.auths)
	}
}
