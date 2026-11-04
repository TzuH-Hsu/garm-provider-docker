//go:build unix

package provider

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
)

// withDefaultLoggerRestored puts slog's process-wide default back after a test
// that lets the watcher install its own logger.
func withDefaultLoggerRestored(t *testing.T) {
	t.Helper()
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// waitForLog polls path until it contains want, or fails after the timeout.
func waitForLog(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := readLogFile(t, path)
		if strings.Contains(got, want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("log file %s never contained %q; has:\n%s", path, want, got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// assertNoCredentials fails if the watcher's log output carries the instance
// token (bare or as a bearer value).
func assertNoCredentials(t *testing.T, logged string) {
	t.Helper()
	for _, secret := range []string{testInstanceToken, "Bearer"} {
		if strings.Contains(logged, secret) {
			t.Errorf("watcher log output contains %q:\n%s", secret, logged)
		}
	}
}

func TestCreateInstanceCarriesLogSettingsToTheWatcher(t *testing.T) {
	t.Setenv(logging.LevelEnvVar, "debug")
	meta := newJITMetadataServer(t)
	defer meta.Close()
	cb, _ := newStatusCallbackServer(t, nil)
	p, _ := newTestProvider(t)
	p.cfg.Logging.File = "/var/log/garm-provider-docker/provider.log"
	var got statusWatchRequest
	p.spawnStatusWatcher = func(_ context.Context, req statusWatchRequest) error {
		got = req
		return nil
	}
	if _, err := p.CreateInstance(context.Background(), statusBootstrap(meta.URL, cb.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if got.LogFile != "/var/log/garm-provider-docker/provider.log" || got.LogLevel != "DEBUG" {
		t.Fatalf("request log settings = %q / %q, want the configured file and level DEBUG", got.LogFile, got.LogLevel)
	}
}

func TestCreateInstanceSendsNoLogSettingsWithoutLogFile(t *testing.T) {
	t.Setenv(logging.LevelEnvVar, "debug")
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
	if got.LogFile != "" || got.LogLevel != "" {
		t.Fatalf("request log settings = %q / %q, want none when [logging] file is unset", got.LogFile, got.LogLevel)
	}
}

func TestStatusWatchRequestValidateIgnoresLogSettings(t *testing.T) {
	now := time.Now()
	req := statusWatchRequest{
		Instance: "i", ContainerID: "c", DockerHost: "unix:///var/run/docker.sock",
		CallbackURL: "https://garm.example.com/api/v1/callbacks", InstanceToken: "tok",
		Deadline: now.Add(90 * time.Second),
		LogFile:  "relative/provider.log", LogLevel: "not-a-level",
	}
	if err := req.validate(now); err != nil {
		t.Fatalf("a bad log file or level rejected the request, which would stop the watcher: %v", err)
	}
}

func TestInstallWatcherLoggerWritesToTheRequestedFileAtTheRequestedLevel(t *testing.T) {
	withDefaultLoggerRestored(t)
	path := filepath.Join(t.TempDir(), "provider.log")

	closer := installWatcherLogger(statusWatchRequest{LogFile: path, LogLevel: "WARN"})
	slog.Info("quiet-info")
	slog.Warn("loud-warn", "instance", "i1")
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := readLogFile(t, path)
	if strings.Contains(got, "quiet-info") || !strings.Contains(got, "msg=loud-warn instance=i1") {
		t.Errorf("log file = %q, want the warn record only", got)
	}
}

func TestInstallWatcherLoggerKeepsTheDefaultWithoutALogFile(t *testing.T) {
	withDefaultLoggerRestored(t)
	before := slog.Default()
	closer := installWatcherLogger(statusWatchRequest{LogLevel: "DEBUG"})
	defer closer.Close()
	if slog.Default() != before {
		t.Error("the watcher replaced the default logger although [logging] file is unset")
	}
}

func TestInstallWatcherLoggerSurvivesAnUnusableFile(t *testing.T) {
	withDefaultLoggerRestored(t)
	t.Chdir(t.TempDir())
	for name, path := range map[string]string{
		"relative":          "provider.log",
		"missing directory": filepath.Join(t.TempDir(), "no-such-dir", "provider.log"),
	} {
		closer := installWatcherLogger(statusWatchRequest{LogFile: path, LogLevel: "bogus"})
		slog.Warn("must-not-panic") // goes to stderr only
		if err := closer.Close(); err != nil {
			t.Errorf("%s: Close: %v", name, err)
		}
	}
	if _, err := os.Stat("provider.log"); err == nil {
		t.Error("a relative log path was created in the working directory")
	}
}

// TestDetachedWatcherLogsToTheConfiguredFile runs the real detached watcher
// (empty environment, stdout/stderr on /dev/null) with a recognisable instance
// token while the callback rejects the first report: the warning it logs must
// land in the configured file, and the file must not hold the token.
func TestDetachedWatcherLogsToTheConfiguredFile(t *testing.T) {
	dockerAPI := fakeDockerAPI(t, "watcher-log-container", "2026-10-10 17:45:14Z: Listening for Jobs")
	cb, rec := newStatusCallbackServer(t, func(string) int { return http.StatusUnauthorized })
	logPath := filepath.Join(t.TempDir(), "provider.log")

	err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "log-e2e", ContainerID: "watcher-log-container",
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"),
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(30 * time.Second),
		LogFile:  logPath, LogLevel: "INFO",
	})
	if err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}

	got := waitForLog(t, logPath, "failed to report runner status installing")
	for _, want := range []string{"level=WARN", "instance=log-e2e", "callback endpoint returned 401"} {
		if !strings.Contains(got, want) {
			t.Errorf("log file is missing %q:\n%s", want, got)
		}
	}
	assertNoCredentials(t, got)
	if s := strings.Join(rec.seen(), ","); s != "installing" {
		t.Errorf("reports = %q, want only the rejected installing report", s)
	}
}

// TestDetachedWatcherLogsItsOutcomeAtInfo: on the success path the watcher's
// info line (status, detail) is logged too, again without the token.
func TestDetachedWatcherLogsItsOutcomeAtInfo(t *testing.T) {
	dockerAPI := fakeDockerAPI(t, "watcher-log-ok", "2026-10-10 17:45:14Z: Listening for Jobs")
	cb, _ := newStatusCallbackServer(t, nil)
	logPath := filepath.Join(t.TempDir(), "provider.log")

	err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "log-ok", ContainerID: "watcher-log-ok",
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"),
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(30 * time.Second),
		LogFile:  logPath, LogLevel: "INFO",
	})
	if err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}
	got := waitForLog(t, logPath, "status watcher: reported runner status")
	if !strings.Contains(got, "status=idle") {
		t.Errorf("log file is missing status=idle:\n%s", got)
	}
	assertNoCredentials(t, got)
}

// TestDetachedWatcherHonoursTheLogLevel: at level ERROR the watcher's warning
// is not written; the file stays free of it.
func TestDetachedWatcherHonoursTheLogLevel(t *testing.T) {
	dockerAPI := fakeDockerAPI(t, "watcher-log-level", "2026-10-10 17:45:14Z: Listening for Jobs")
	var once sync.Once
	rejected := make(chan struct{})
	cb, _ := newStatusCallbackServer(t, func(string) int {
		once.Do(func() { close(rejected) })
		return http.StatusUnauthorized
	})
	logPath := filepath.Join(t.TempDir(), "provider.log")

	err := startDetachedWatcher(context.Background(), statusWatchRequest{
		Instance: "log-level", ContainerID: "watcher-log-level",
		DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"),
		CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
		Deadline: time.Now().Add(30 * time.Second),
		LogFile:  logPath, LogLevel: "ERROR",
	})
	if err != nil {
		t.Fatalf("startDetachedWatcher: %v", err)
	}
	select {
	case <-rejected:
	case <-time.After(20 * time.Second):
		t.Fatal("the watcher never made its first report")
	}
	time.Sleep(500 * time.Millisecond) // the warning would be logged right after the rejection
	if got := readLogFile(t, logPath); strings.Contains(got, "level=WARN") {
		t.Errorf("log file holds a warning although the level is ERROR:\n%s", got)
	}
}

// TestDetachedWatcherStillReportsWithAnUnusableLogFile: a log file that cannot
// be opened must never stop the watcher from doing its job.
func TestDetachedWatcherStillReportsWithAnUnusableLogFile(t *testing.T) {
	for name, logFile := range map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "no-such-dir", "provider.log"),
		"relative path":     "provider.log",
		"a directory":       t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			dockerAPI := fakeDockerAPI(t, "watcher-log-bad", "2026-10-10 17:45:14Z: Listening for Jobs")
			idle := make(chan struct{})
			var once sync.Once
			cb, _ := newStatusCallbackServer(t, func(status string) int {
				if status == "idle" {
					once.Do(func() { close(idle) })
				}
				return 0
			})
			err := startDetachedWatcher(context.Background(), statusWatchRequest{
				Instance: "log-bad", ContainerID: "watcher-log-bad",
				DockerHost:  "tcp://" + strings.TrimPrefix(dockerAPI.URL, "http://"),
				CallbackURL: cb.URL + "/api/v1/callbacks", InstanceToken: testInstanceToken,
				Deadline: time.Now().Add(30 * time.Second),
				LogFile:  logFile, LogLevel: "not-a-level",
			})
			if err != nil {
				t.Fatalf("startDetachedWatcher: %v", err)
			}
			select {
			case <-idle:
			case <-time.After(20 * time.Second):
				t.Fatal("the watcher never reported idle with an unusable log file")
			}
		})
	}
}
