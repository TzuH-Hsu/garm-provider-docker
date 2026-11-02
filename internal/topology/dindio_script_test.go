package topology

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// stubDocker is a POSIX sh stand-in for the docker CLI, steered by environment
// variables so each probe step can be made to succeed, print nothing, or fail.
const stubDocker = `#!/bin/sh
case "$1 $2" in
"info --format")
  [ -z "$STUB_INFO_FAIL" ] || exit 1
  echo "overlay2 [[Backing Filesystem extfs]]" ;;
"image ls")
  [ -z "$STUB_LS_FAIL" ] || { echo "Cannot connect to the Docker daemon" >&2; exit 1; }
  printf '%s' "$STUB_IDS" ;;
"image inspect")
  [ -z "$STUB_INSPECT_FAIL" ] || exit 1
  echo "image sha256:aa 1000"
  echo "image sha256:bb 2500" ;;
esac
`

// runProbeScript runs imageProbeScript under sh with a stub docker first on
// PATH and returns its stdout and exit code.
func runProbeScript(t *testing.T, env ...string) (string, int) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stubDocker), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write stub docker: %v", err)
	}
	cmd := exec.Command("sh", "-c", imageProbeScript)
	cmd.Env = append([]string{"PATH=" + dir + ":" + os.Getenv("PATH")}, env...)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("run probe script: %v", err)
		return "", 0
	}
}

func TestProbeScriptSuccess(t *testing.T) {
	out, code := runProbeScript(t, "STUB_IDS=sha256:bb\nsha256:aa\nsha256:bb\n")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (output %q)", code, out)
	}
	p, err := parseImageProbe(out)
	if err != nil {
		t.Fatalf("parseImageProbe(%q): %v", out, err)
	}
	if p.Driver != "overlay2" || p.Store != "classic" || p.Images != 2 || p.ImageBytes != 3500 {
		t.Errorf("probe = %+v", p)
	}
}

// TestProbeScriptEmptyDaemonIsValid keeps the legitimate zero-image answer
// distinguishable from a failed list (below): exit 0, driver record, no images.
func TestProbeScriptEmptyDaemonIsValid(t *testing.T) {
	out, code := runProbeScript(t, "STUB_IDS=")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (output %q)", code, out)
	}
	p, err := parseImageProbe(out)
	if err != nil || p.Images != 0 || p.Driver != "overlay2" {
		t.Fatalf("probe = %+v, err = %v, want an empty overlay2 daemon", p, err)
	}
}

// TestProbeScriptFailuresAreNotMasked is the regression test for a failed
// image list being swallowed by the `| sort` that followed it and reported as
// a valid zero-image job: every failing step must fail the script.
func TestProbeScriptFailuresAreNotMasked(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		wantCode int
	}{
		{"docker info fails", "STUB_INFO_FAIL=1", 3},
		{"image list fails", "STUB_LS_FAIL=1", 4},
		{"image inspect fails", "STUB_INSPECT_FAIL=1", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, code := runProbeScript(t, tc.env, "STUB_IDS=sha256:aa\n")
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (output %q)", code, tc.wantCode, out)
			}
			if strings.Contains(out, "image ") && tc.wantCode == 4 {
				t.Errorf("a failed list must not print image records: %q", out)
			}
		})
	}
}
