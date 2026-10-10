//go:build unix

package provider

import (
	"context"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

	attr := watcherProcAttr(pr, devNull, nil)
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
// fix: CreateInstance makes no callback request at all, hands everything to
// the watcher, and returns at once even with the largest timeout and a runner that never comes
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
	// No callback I/O inside CreateInstance: even installing is the watcher's.
	if s := rec.seen(); len(s) != 0 {
		t.Fatalf("CreateInstance itself reported %v, want nothing (the watcher reports)", s)
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
	if s := rec.seen(); strings.Join(s, ",") != "installing,installing" || rec.lastMessage() != statusMessageNotListening {
		t.Fatalf("reports = %v / %q, want installing, then installing with the not-listening message", s, rec.lastMessage())
	}
}

// fakeDockerAPI is a minimal Docker Engine API for the detached watcher: it
// answers the SDK's version negotiation, reports one container as running, and
// serves its logs as a real daemon does for a non-TTY container (stdcopy
// frames, stream kept open).
func fakeDockerAPI(t *testing.T, containerID, line string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.47")
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/containers/"+containerID+"/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Id":"` + containerID + `","State":{"Status":"running","Running":true}}`))
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

// TestCreateInstanceRunnerExitsAfterListening: a runner that printed the
// listening line and then exited is not reported idle — a stopped
// container's logs still replay the line, so readiness is confirmed against
// the container's state.
func TestCreateInstanceRunnerExitsAfterListening(t *testing.T) {
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	b := statusBootstrap(meta.URL, "")
	runner := spec.RunnerContainerName(b.Name)
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			printRunnerOnline(fake, runner)
			if info, err := fake.ContainerInspect(context.Background(), runner); err == nil {
				fake.SetState(info.ID, "exited", false)
			}
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"

	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got := rec.seen(); strings.Join(got, ",") != "installing,installing" || rec.lastMessage() != statusMessageRunnerStopped {
		t.Fatalf("reports = %v / %q, want [installing installing] with the stopped message", got, rec.lastMessage())
	}
}

func TestConfirmRunning(t *testing.T) {
	p, fake := newTestProvider(t)
	for _, tt := range []struct {
		name  string
		state string
		oom   bool
		want  bool
	}{
		{"running", "running", false, true},
		{"exited", "exited", false, false},
		{"dead", "dead", false, false},
		{"oom killed", "running", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := startFakeRunnerNamed(t, fake, "confirm-"+strings.ReplaceAll(tt.name, " ", "-"))
			fake.SetState(id, tt.state, tt.oom)
			ok, err := p.confirmRunning(context.Background(), id)
			if ok != tt.want {
				t.Fatalf("confirmRunning = %v (%v), want %v", ok, err, tt.want)
			}
			if !ok && !errors.Is(err, errRunnerStopped) {
				t.Errorf("error = %v, want errRunnerStopped", err)
			}
		})
	}
	if ok, err := p.confirmRunning(context.Background(), "missing"); ok || !errors.Is(err, errRunnerStopped) {
		t.Errorf("missing container: %v, %v; want false, errRunnerStopped", ok, err)
	}
}

func startFakeRunnerNamed(t *testing.T, fake *docker.FakeClient, name string) string {
	t.Helper()
	created, err := fake.ContainerCreate(context.Background(), &container.Config{Image: "img"}, nil, nil, nil, name)
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	if err := fake.ContainerStart(context.Background(), created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("ContainerStart: %v", err)
	}
	return created.ID
}

// TestCreateInstanceCarriesProxyEnv: the provider's proxy settings travel to
// the watcher inside the stdin request.
func TestCreateInstanceCarriesProxyEnv(t *testing.T) {
	for _, k := range proxyEnvVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("HTTPS_PROXY", "http://proxyuser:proxy-secret@proxy.example.test:3128")
	t.Setenv("NO_PROXY", "localhost,127.0.0.1,.internal")

	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, _ := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)
	var got statusWatchRequest
	p.spawnStatusWatcher = func(_ context.Context, req statusWatchRequest) error {
		got = req
		return nil
	}
	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	want := map[string]string{
		"HTTPS_PROXY": "http://proxyuser:proxy-secret@proxy.example.test:3128",
		"NO_PROXY":    "localhost,127.0.0.1,.internal",
	}
	if len(got.ProxyEnv) != len(want) || got.ProxyEnv["HTTPS_PROXY"] != want["HTTPS_PROXY"] || got.ProxyEnv["NO_PROXY"] != want["NO_PROXY"] {
		t.Fatalf("request proxy env = %v, want %v", got.ProxyEnv, want)
	}
}

func TestStatusWatchRequestRedactsProxyCredentials(t *testing.T) {
	req := statusWatchRequest{
		InstanceToken: testInstanceToken,
		ProxyEnv:      map[string]string{"HTTPS_PROXY": "http://proxyuser:p%40ss-secret@proxy.example.test:3128"},
	}
	msg := req.redact(`Post "https://garm/status": proxyconnect tcp: http://proxyuser:p%40ss-secret@proxy.example.test:3128 refused; password p@ss-secret; token ` + testInstanceToken)
	for _, secret := range []string{"p@ss-secret", "p%40ss-secret", testInstanceToken} {
		if strings.Contains(msg, secret) {
			t.Errorf("redacted message still contains %q: %s", secret, msg)
		}
	}
}

// TestDetachedWatcherUsesCarriedProxy runs the real detached watcher with an
// empty exec environment and a proxy only in its stdin request: its idle
// report to a non-loopback GARM must go through that proxy (a CONNECT with
// the proxy credentials), exactly as the provider's own requests would.
func TestDetachedWatcherUsesCarriedProxy(t *testing.T) {
	const containerID = "watcher-proxy-container"
	dockerAPI := fakeDockerAPI(t, containerID, "2026-10-10 17:45:14Z: Listening for Jobs")
	connects := make(chan string, 8)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			connects <- r.Host + " " + r.Header.Get("Proxy-Authorization")
		}
		w.WriteHeader(http.StatusBadGateway) // refuse the tunnel; reaching us is the point
	}))
	defer proxy.Close()
	proxyURL := "http://proxyuser:proxy-secret@" + strings.TrimPrefix(proxy.URL, "http://")

	err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "proxy", ContainerID: containerID,
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"), // loopback: never proxied
		CallbackURL: "https://garm.example.test/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(30 * time.Second),
		ProxyEnv: map[string]string{"HTTPS_PROXY": proxyURL},
	})
	if err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}
	select {
	case got := <-connects:
		if !strings.HasPrefix(got, "garm.example.test:443 Basic ") {
			t.Fatalf("CONNECT = %q, want garm.example.test:443 with Basic proxy credentials", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the watcher's idle report never went through the carried proxy")
	}
}

// TestWatcherExecEnvCarriesOnlyTrustAndRuntimeSettings: the watcher's exec
// environment holds SSL_CERT_FILE, SSL_CERT_DIR and GODEBUG when set, and
// nothing else — not the proxy variables (possibly credentialed; they go in
// the stdin request) and not anything else GARM set.
func TestWatcherExecEnvCarriesOnlyTrustAndRuntimeSettings(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "/etc/garm/ca.pem")
	t.Setenv("SSL_CERT_DIR", "/etc/garm/certs")
	t.Setenv("GODEBUG", "x509negativeserial=1")
	t.Setenv("HTTPS_PROXY", "http://proxyuser:proxy-secret@proxy.example.test:3128")
	t.Setenv("GARM_COMMAND", "CreateInstance")

	got := strings.Join(watcherExecEnv(), "\n")
	want := "SSL_CERT_FILE=/etc/garm/ca.pem\nSSL_CERT_DIR=/etc/garm/certs\nGODEBUG=x509negativeserial=1"
	if got != want {
		t.Fatalf("watcher exec env = %q, want %q", got, want)
	}
}

// TestDetachedWatcherTrustsSSLCertFile: a GARM callback whose CA is trusted
// only through SSL_CERT_FILE (no bootstrap CA bundle) stays trusted in the
// detached watcher. Go honours SSL_CERT_FILE on Unix systems other than
// macOS, where the platform verifier is used instead.
func TestDetachedWatcherTrustsSSLCertFile(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Go ignores SSL_CERT_FILE on macOS (platform verifier)")
	}
	const containerID = "watcher-sslcert-container"
	dockerAPI := fakeDockerAPI(t, containerID, "2026-10-10 17:45:14Z: Listening for Jobs")
	reports := make(chan string, 4)
	cb := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		reports <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer cb.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cb.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSL_CERT_FILE", caFile)
	t.Setenv("SSL_CERT_DIR", t.TempDir()) // empty: only the file above is trusted

	if err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "sslcert", ContainerID: containerID,
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"),
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(30 * time.Second),
	}); err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}
	for _, want := range []string{`"status":"installing"`, `"status":"idle"`} {
		select {
		case got := <-reports:
			if !strings.Contains(got, want) {
				t.Fatalf("report = %s, want %s", got, want)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("no %s report: the watcher did not trust the callback through SSL_CERT_FILE", want)
		}
	}
}

func TestReaperVerdict(t *testing.T) {
	for _, tt := range []struct {
		comm string
		err  error
		want bool
	}{
		{"systemd\n", nil, true},
		{"docker-init\n", nil, true},
		{"tini\n", nil, true},
		{"catatonit\n", nil, true},
		{"garm\n", nil, false},
		{"garm-provider-d\n", nil, false},
		{"sleep\n", nil, false},
		{"", errors.New("permission denied"), true},
	} {
		if got := reaperVerdict(tt.comm, tt.err); got != tt.want {
			t.Errorf("reaperVerdict(%q, %v) = %v, want %v", tt.comm, tt.err, got, tt.want)
		}
	}
}

// withReaperCheck overrides the reaper check and the detach function.
func withReaperCheck(t *testing.T, reaped bool, pid1 string) *[]statusWatchRequest {
	t.Helper()
	prevCheck, prevDetach := orphanReaperCheck, detachWatcher
	var detached []statusWatchRequest
	orphanReaperCheck = func() (bool, string) { return reaped, pid1 }
	detachWatcher = func(_ context.Context, req statusWatchRequest) error {
		detached = append(detached, req)
		return nil
	}
	t.Cleanup(func() { orphanReaperCheck, detachWatcher = prevCheck, prevDetach })
	return &detached
}

func TestStartStatusWatcherDetachesWhenOrphansAreReaped(t *testing.T) {
	detached := withReaperCheck(t, true, "docker-init")
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, rec := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)
	p.spawnStatusWatcher = p.startStatusWatcher

	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(*detached) != 1 {
		t.Fatalf("detached watchers = %d, want 1", len(*detached))
	}
	if s := rec.seen(); len(s) != 0 {
		t.Errorf("CreateInstance reported %v itself, want nothing", s)
	}
}

// TestStartStatusWatcherRunsInProcessWithoutReaper: with GARM as PID 1 and no
// init, the watcher would become a zombie, so it runs inside CreateInstance.
func TestStartStatusWatcherRunsInProcessWithoutReaper(t *testing.T) {
	detached := withReaperCheck(t, false, "garm")
	meta := newJITMetadataServer(t)
	defer meta.Close()
	p, fake := newTestProvider(t)
	p.spawnStatusWatcher = p.startStatusWatcher
	b := statusBootstrap(meta.URL, "")
	runner := spec.RunnerContainerName(b.Name)
	cb, rec := newStatusCallbackServer(t, func(status string) int {
		if status == "installing" {
			printRunnerOnline(fake, runner)
		}
		return 0
	})
	b.CallbackURL = cb.URL + "/api/v1/callbacks"
	logs := captureLogs(t)

	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if len(*detached) != 0 {
		t.Fatalf("a watcher was detached although orphans are not reaped")
	}
	if s := rec.seen(); strings.Join(s, ",") != "installing,idle" {
		t.Fatalf("reports = %v, want installing, idle from the in-process watcher", s)
	}
	if out := logs.String(); !strings.Contains(out, "docker run --init") || !strings.Contains(out, "pid1=garm") {
		t.Errorf("want a warning naming PID 1 and recommending --init, logs:\n%s", out)
	}
}
