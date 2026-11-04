package logging

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// captureStderr points os.Stderr at a temp file for the test and returns a
// reader for what was written to it. These tests are not parallel, so the
// process-wide swap is safe.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create stderr capture: %v", err)
	}
	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = orig
		_ = f.Close()
	})
	return func() string {
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatalf("read stderr capture: %v", err)
		}
		return string(b)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestNewLeavesNoFileAndWritesStderrOnly pins the unset case: New (what main
// uses when [logging] file is empty) writes only to stderr.
func TestNewLeavesNoFileAndWritesStderrOnly(t *testing.T) {
	stderr := captureStderr(t)
	dir := t.TempDir()
	t.Chdir(dir)

	New().Info("hello", "k", "v")

	if got := stderr(); !strings.Contains(got, `level=INFO msg=hello k=v`) {
		t.Errorf("stderr = %q, want the record", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("New() created files in the working directory: %v", entries)
	}
}

func TestNewWithFileWritesTheSameLineToFileAndStderr(t *testing.T) {
	stderr := captureStderr(t)
	path := filepath.Join(t.TempDir(), "provider.log")

	l, closer := NewWithFile(path)
	l.Info("teardown: DinD sidecar IO", "instance", "gha-1", "written_bytes", 42)
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	file := readFile(t, path)
	if !strings.Contains(file, `level=INFO msg="teardown: DinD sidecar IO" instance=gha-1 written_bytes=42`) {
		t.Errorf("file = %q, want the record", file)
	}
	if file != stderr() {
		t.Errorf("file and stderr differ\nfile:   %q\nstderr: %q", file, stderr())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&^0o640 != 0 {
		t.Errorf("file mode = %o, want 0640 or tighter", perm)
	}
}

func TestNewWithFileAppliesRedactionToTheFile(t *testing.T) {
	captureStderr(t)
	path := filepath.Join(t.TempDir(), "provider.log")
	const secret = "s3cr3t-instance-token"

	l, closer := NewWithFile(path)
	l.Warn("callback failed", "error", Redact("POST /status?token="+secret+" refused", secret))
	_ = closer.Close()

	file := readFile(t, path)
	if strings.Contains(file, secret) {
		t.Errorf("file leaks the secret: %q", file)
	}
	if !strings.Contains(file, redactedPlaceholder) {
		t.Errorf("file = %q, want the redaction placeholder", file)
	}
}

func TestNewWithFileHonoursTheConfiguredLevel(t *testing.T) {
	captureStderr(t)
	t.Setenv(LevelEnvVar, "warn")
	path := filepath.Join(t.TempDir(), "provider.log")

	l, closer := NewWithFile(path)
	l.Info("dropped")
	l.Warn("kept")
	_ = closer.Close()

	file := readFile(t, path)
	if strings.Contains(file, "dropped") || !strings.Contains(file, "kept") {
		t.Errorf("file = %q, want only the warn record", file)
	}
}

func TestNewWithFileAppendsAcrossInvocations(t *testing.T) {
	captureStderr(t)
	path := filepath.Join(t.TempDir(), "provider.log")

	for _, msg := range []string{"first", "second"} {
		l, closer := NewWithFile(path)
		l.Info(msg)
		_ = closer.Close()
	}

	lines := strings.Split(strings.TrimSpace(readFile(t, path)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "first") || !strings.Contains(lines[1], "second") {
		t.Errorf("lines = %q, want first then second", lines)
	}
}

// TestNewWithFileUnusablePathWarnsOnceAndKeepsLogging covers every way the
// open can fail: the call must still log to stderr and warn exactly once.
func TestNewWithFileUnusablePathWarnsOnceAndKeepsLogging(t *testing.T) {
	cases := map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "no-such-dir", "provider.log"),
		"relative path":     "provider.log",
		"path is a dir":     t.TempDir(),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			stderr := captureStderr(t)
			t.Chdir(t.TempDir())

			l, closer := NewWithFile(path)
			l.Info("still logging")
			if err := closer.Close(); err != nil {
				t.Errorf("Close on a stderr-only logger: %v", err)
			}

			got := stderr()
			if n := strings.Count(got, "level=WARN"); n != 1 {
				t.Errorf("warnings = %d, want exactly 1\n%s", n, got)
			}
			if !strings.Contains(got, "cannot open the log file") {
				t.Errorf("stderr = %q, want the open warning", got)
			}
			if !strings.Contains(got, "still logging") {
				t.Errorf("stderr = %q, want the record after the warning", got)
			}
			if _, err := os.Stat("provider.log"); err == nil {
				t.Errorf("a relative path must not create a file in the working directory")
			}
		})
	}
}

// TestNewWithFileWriteFailureWarnsOnceAndStderrContinues simulates the file
// going bad after a successful open (disk full, descriptor closed under us).
func TestNewWithFileWriteFailureWarnsOnceAndStderrContinues(t *testing.T) {
	stderr := captureStderr(t)
	path := filepath.Join(t.TempDir(), "provider.log")

	l, closer := NewWithFile(path)
	sink, ok := closer.(*fileSink)
	if !ok {
		t.Fatalf("closer is %T, want *fileSink", closer)
	}
	if err := sink.f.Close(); err != nil { // break the descriptor behind the sink's back
		t.Fatal(err)
	}

	l.Info("one")
	l.Info("two")
	l.Info("three")

	got := stderr()
	if n := strings.Count(got, "writing the log file failed"); n != 1 {
		t.Errorf("write-failure warnings = %d, want exactly 1\n%s", n, got)
	}
	for _, msg := range []string{"msg=one", "msg=two", "msg=three"} {
		if !strings.Contains(got, msg) {
			t.Errorf("stderr is missing %s after the file failed:\n%s", msg, got)
		}
	}
}

func TestNewWithFileCloseIsIdempotent(t *testing.T) {
	captureStderr(t)
	_, closer := NewWithFile(filepath.Join(t.TempDir(), "provider.log"))
	if err := closer.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

const (
	helperFileEnv  = "GARM_LOGGING_TEST_HELPER_FILE"
	helperIDEnv    = "GARM_LOGGING_TEST_HELPER_ID"
	helperLines    = 300
	helperPayloadN = 3000 // well above the 512-byte POSIX PIPE_BUF floor
)

// TestHelperAppendProcess is not a test: it is the body of the child
// processes TestConcurrentProcessesDoNotInterleaveLines starts. Run on its own
// it does nothing.
func TestHelperAppendProcess(t *testing.T) {
	path := os.Getenv(helperFileEnv)
	if path == "" {
		t.Skip("helper process body; started by TestConcurrentProcessesDoNotInterleaveLines")
	}
	id := os.Getenv(helperIDEnv)
	payload := strings.Repeat(id, helperPayloadN)
	l, closer := NewWithFile(path)
	for i := 0; i < helperLines; i++ {
		l.Info("load", "id", id, "payload", payload)
	}
	_ = closer.Close()
}

// TestConcurrentProcessesDoNotInterleaveLines pins the line-integrity
// guarantee documented for the log file: each record is one O_APPEND write, so
// two provider processes appending at once never split or mix a line, even
// for lines far larger than PIPE_BUF.
func TestConcurrentProcessesDoNotInterleaveLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.log")

	ids := []string{"A", "B"}
	cmds := make([]*exec.Cmd, len(ids))
	for i, id := range ids {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAppendProcess$")
		cmd.Env = append(os.Environ(), helperFileEnv+"="+path, helperIDEnv+"="+id)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start helper %s: %v", id, err)
		}
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %s: %v", ids[i], err)
		}
	}

	lineRE := regexp.MustCompile(`^time=\S+ level=INFO msg=load id=([AB]) payload=([AB]+)$`)
	lines := strings.Split(strings.TrimSuffix(readFile(t, path), "\n"), "\n")
	if want := helperLines * len(ids); len(lines) != want {
		t.Fatalf("lines = %d, want %d", len(lines), want)
	}
	for n, line := range lines {
		m := lineRE.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("line %d is malformed (interleaved or truncated), len %d", n+1, len(line))
		}
		if m[2] != strings.Repeat(m[1], helperPayloadN) {
			t.Fatalf("line %d mixes writers: id=%s payload len %d", n+1, m[1], len(m[2]))
		}
	}
}

// TestNewWithFileAtHonoursTheGivenLevel: the detached status watcher starts
// with an empty environment, so its level arrives as an argument. The file
// gets exactly the records that level allows, whatever the environment says.
func TestNewWithFileAtHonoursTheGivenLevel(t *testing.T) {
	captureStderr(t)
	t.Setenv(LevelEnvVar, "error")
	path := filepath.Join(t.TempDir(), "provider.log")

	l, closer := NewWithFileAt(path, slog.LevelWarn)
	l.Info("quiet-info")
	l.Warn("loud-warn")
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := readFile(t, path)
	if strings.Contains(got, "quiet-info") || !strings.Contains(got, "msg=loud-warn") {
		t.Errorf("file = %q, want the warn record only", got)
	}
}

func TestNewWithFileAtFallsBackToStderrOnlyAtTheGivenLevel(t *testing.T) {
	stderr := captureStderr(t)
	l, closer := NewWithFileAt(filepath.Join(t.TempDir(), "missing-dir", "provider.log"), slog.LevelDebug)
	defer closer.Close()
	l.Debug("still-here")
	if got := stderr(); !strings.Contains(got, "msg=still-here") {
		t.Errorf("stderr = %q, want the debug record after the file failed to open", got)
	}
}
