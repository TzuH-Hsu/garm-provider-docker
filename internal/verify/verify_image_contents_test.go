//go:build dockerverify

// This file verifies the REAL runner images (runner-images/noble and
// runner-images/resolute: their real Dockerfiles and the shared real
// entrypoint.sh) on a live daemon:
//
//   - the shared libraries a headless Chrome needs are installed, so a Chrome
//     for Testing downloaded by a job (Puppeteer, Lighthouse CI) can start
//     without apt (jobs have no passwordless sudo);
//
//   - the CI tooling jobs expect is present (pipx, clang-format, clang-tidy,
//     cmake, ninja) and the runner user can install tools into /usr/local/bin
//     and /usr/local/lib;
//
//   - the image carries the garm.docker/os-release label the provider keys the
//     toolcache and pnpm store volumes on;
//
//   - the entrypoint raises the open-file soft limit for the runner user, and
//     clamps it to the hard limit.
//
//     go test -tags dockerverify -v -run TestVerifyRunnerImage ./internal/verify/
//
// Each image is built for the daemon's native platform and removed afterwards.
package verify

import (
	"fmt"
	"strings"
	"testing"
)

// chromeLibs are sonames that Chrome for Testing links against and that the base
// images do not ship (the apt layer in each runner-images/*/Dockerfile adds them).
var chromeLibs = []string{
	"libnss3.so", "libnspr4.so", "libatk-1.0.so.0", "libatk-bridge-2.0.so.0",
	"libcups.so.2", "libgbm.so.1", "libxkbcommon.so.0", "libasound.so.2",
	"libcairo.so.2", "libpango-1.0.so.0", "libXcomposite.so.1",
	"libXdamage.so.1", "libXrandr.so.2", "libatspi.so.0",
}

// imageExpectations is what each runner image must report beyond the shared
// checks: its os-release label and the LLVM major its unversioned
// clang-format/clang-tidy resolve to.
var imageExpectations = map[string]struct {
	osRelease string
	llvmMajor string
}{
	"noble":    {osRelease: "ubuntu-24.04", llvmMajor: "18"},
	"resolute": {osRelease: "ubuntu-26.04", llvmMajor: "22"},
}

func TestVerifyRunnerImageContents(t *testing.T) {
	root := repoRoot(t)
	for _, name := range runnerImageNames {
		t.Run(name, func(t *testing.T) {
			verifyRunnerImageContents(t, root, name)
		})
	}
}

func verifyRunnerImageContents(t *testing.T, root, name string) {
	want, ok := imageExpectations[name]
	if !ok {
		t.Fatalf("no expectations recorded for runner image %q", name)
	}
	tag := "garm-image-contents-verify-" + name + ":" + randHex(t)
	if out, err := dockerTry(append([]string{"build", "-t", tag}, runnerImageBuildArgs(root, name)...)...); err != nil {
		t.Fatalf("build real runner-images/%s image: %v\n%s", name, err, out)
	}
	defer func() { _, _ = dockerTry("rmi", "-f", tag) }()

	if got := imageOSKey(t, tag); got != want.osRelease {
		t.Errorf("OS key = %q, want %q (garm.docker/os-release label)", got, want.osRelease)
	}

	out := dockerOut(t, "run", "--rm", "--entrypoint", "ldconfig", tag, "-p")
	for _, lib := range chromeLibs {
		if !strings.Contains(out, lib) {
			t.Errorf("runner image is missing %s, which a headless Chrome needs", lib)
		}
	}

	for _, tool := range []string{"file", "fc-list", "pipx", "cmake", "ninja", "clang-format", "clang-tidy", "clang-format-" + want.llvmMajor, "clang-tidy-" + want.llvmMajor} {
		if _, err := dockerTry("run", "--rm", "--entrypoint", "sh", tag, "-c", "command -v "+tool); err != nil {
			t.Errorf("runner image is missing %s", tool)
		}
	}
	for _, tool := range []string{"clang-format", "clang-tidy"} {
		out := dockerOut(t, "run", "--rm", "--entrypoint", tool, tag, "--version")
		if !strings.Contains(out, "version "+want.llvmMajor+".") {
			t.Errorf("%s --version = %q, want LLVM %s", tool, strings.TrimSpace(out), want.llvmMajor)
		}
	}

	// The runner user can add tools to /usr/local/bin and /usr/local/lib (no
	// sudo in jobs), but the files the base ships there stay root-owned.
	probe := "set -e; for d in /usr/local/bin /usr/local/lib; do " +
		"touch \"$d/garm-write-probe\"; rm \"$d/garm-write-probe\"; done; " +
		"mkdir /usr/local/lib/garm-dir-probe; rmdir /usr/local/lib/garm-dir-probe; " +
		"test ! -w /usr/local/bin/pnpm"
	if out, err := dockerTry("run", "--rm", "--user", "runner", "--entrypoint", "bash", tag, "-c", probe); err != nil {
		t.Errorf("runner user cannot install into /usr/local/bin and /usr/local/lib as intended: %v\n%s", err, out)
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
