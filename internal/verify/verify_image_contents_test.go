//go:build dockerverify

// This file verifies two properties of the REAL runner image
// (runner-images/noble: its real Dockerfile and real entrypoint.sh) on a live
// daemon:
//
//   - the shared libraries a headless Chrome needs are installed, so a Chrome
//     for Testing downloaded by a job (Puppeteer, Lighthouse CI) can start
//     without apt (jobs have no passwordless sudo);
//
//   - the entrypoint raises the open-file soft limit for the runner user, and
//     clamps it to the hard limit.
//
//     go test -tags dockerverify -v -run TestVerifyRunnerImage ./internal/verify/
//
// The image is built for the daemon's native platform and removed afterwards.
package verify

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// chromeLibs are sonames that Chrome for Testing links against and that the base
// image does not ship (the apt layer in runner-images/noble/Dockerfile adds them).
var chromeLibs = []string{
	"libnss3.so", "libnspr4.so", "libatk-1.0.so.0", "libatk-bridge-2.0.so.0",
	"libcups.so.2", "libgbm.so.1", "libxkbcommon.so.0", "libasound.so.2",
	"libcairo.so.2", "libpango-1.0.so.0", "libXcomposite.so.1",
	"libXdamage.so.1", "libXrandr.so.2", "libatspi.so.0",
}

func TestVerifyRunnerImageContents(t *testing.T) {
	root := repoRoot(t)
	tag := "garm-image-contents-verify:" + randHex(t)
	if out, err := dockerTry("build", "-t", tag, filepath.Join(root, "runner-images", "noble")); err != nil {
		t.Fatalf("build real runner-images/noble image: %v\n%s", err, out)
	}
	defer func() { _, _ = dockerTry("rmi", "-f", tag) }()

	out := dockerOut(t, "run", "--rm", "--entrypoint", "ldconfig", tag, "-p")
	for _, lib := range chromeLibs {
		if !strings.Contains(out, lib) {
			t.Errorf("runner image is missing %s, which a headless Chrome needs", lib)
		}
	}

	for _, tool := range []string{"file", "fc-list"} {
		if _, err := dockerTry("run", "--rm", "--entrypoint", "sh", tag, "-c", "command -v "+tool); err != nil {
			t.Errorf("runner image is missing %s", tool)
		}
	}

	// Open-file soft limit, against the REAL raise_nofile_soft_limit (entrypoint
	// sourced, main not run). The entrypoint's own effect is asserted exactly
	// (shell); the runner user's limit after the gosu drop is asserted as a floor,
	// because a gosu build may itself lift the soft limit to the hard limit (Go
	// does that at startup) - never below what the entrypoint set.
	cases := []struct {
		name, ulimit string
		wantBefore   int
		want         int
	}{
		{"docker default raised to the target", "1024:524288", 1024, 65536},
		{"clamped to a lower hard limit", "1024:2048", 1024, 2048},
		{"already at the hard limit", "1024:1024", 1024, 1024},
	}
	script := "before=$(ulimit -Sn); source /entrypoint.sh; raise_nofile_soft_limit 2>/dev/null; " +
		"echo \"${before} $(ulimit -Sn) $(gosu runner sh -c 'ulimit -Sn')\""
	for _, tc := range cases {
		out := dockerOut(t, "run", "--rm", "--ulimit", "nofile="+tc.ulimit, "--entrypoint", "bash", tag, "-c", script)
		var before, inShell, asRunner int
		if _, err := fmt.Sscan(out, &before, &inShell, &asRunner); err != nil {
			t.Fatalf("%s: unexpected probe output %q: %v", tc.name, out, err)
		}
		if before != tc.wantBefore {
			// The daemon did not apply the requested soft limit, so there is
			// nothing for the entrypoint to raise.
			t.Logf("%s: daemon started the container at soft nofile %d, not %d; skipping", tc.name, before, tc.wantBefore)
			continue
		}
		if inShell != tc.want {
			t.Errorf("%s: soft nofile after raise_nofile_soft_limit = %d, want %d", tc.name, inShell, tc.want)
		}
		if asRunner < tc.want {
			t.Errorf("%s: runner user's soft nofile = %d, want at least %d", tc.name, asRunner, tc.want)
		}
	}
}
