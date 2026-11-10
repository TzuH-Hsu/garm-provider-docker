// Package jobhooks has no production code. It holds the tests of the runner
// image's job-started hook (runner-images/common/hooks): the dispatcher, the
// drop-in checks, and the way both Dockerfiles install them. The scripts are
// executed exactly as the GitHub Actions runner executes a hook, as
// `bash --noprofile --norc -e -o pipefail <file>` (the shebang is ignored), so
// an unguarded command that fails under errexit shows up here and not as a
// failed job in production. The tests need bash, and the hanging-check test
// also needs timeout(1); they skip when those are missing. They need no Docker.
package jobhooks

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// damageExit is the exit code a check uses to report damage. It is part of
	// the drop-in contract in runner-images/README.md.
	damageExit = 10

	docsURL = "https://github.com/TzuH-Hsu/garm-provider-docker/blob/main/docs/package-store-installs.md"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve the caller for the repository root")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	return root
}

func hooksDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "runner-images", "common", "hooks")
}

func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
}

func requireTimeout(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout(1) is not installed")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustCopy(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	mustWrite(t, dst, string(data))
}

// result is what the runner would see of a hook: its output and exit code.
type result struct {
	out  string
	code int
}

// runHook runs script the way the runner does, with env as the whole
// environment apart from PATH.
func runHook(t *testing.T, script string, env map[string]string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LC_ALL=C"}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	res := result{out: buf.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	default:
		t.Fatalf("run %s: %v", script, err)
	}
	return res
}

func (r result) mustHave(t *testing.T, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(r.out, p) {
			t.Errorf("output lacks %q:\n%s", p, r.out)
		}
	}
}

func (r result) mustNotHave(t *testing.T, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(r.out, p) {
			t.Errorf("output has unexpected %q:\n%s", p, r.out)
		}
	}
}

func (r result) wantCode(t *testing.T, want int) {
	t.Helper()
	if r.code != want {
		t.Errorf("exit code = %d, want %d; output:\n%s", r.code, want, r.out)
	}
}

// --- 10-pnpm-store.sh --------------------------------------------------------

const pnpmBinaryRel = "v11/links/@pnpm/exe/11.7.0/aaaa/node_modules/@pnpm/exe/pnpm"

func pnpmCheck(t *testing.T) string {
	t.Helper()
	requireBash(t)
	return filepath.Join(hooksDir(t), "job-started.d", "10-pnpm-store.sh")
}

func TestPnpmStoreCheck(t *testing.T) {
	check := pnpmCheck(t)

	store := func(t *testing.T, binary string) string {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, pnpmBinaryRel), binary)
		return dir
	}
	const healthy, empty = "not-empty", ""

	t.Run("no store variable set is skipped silently", func(t *testing.T) {
		r := runHook(t, check, nil)
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("store directory missing is skipped silently", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": filepath.Join(t.TempDir(), "absent")})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("store path that is a file is skipped silently", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		mustWrite(t, f, "x")
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": f})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("empty store without v11 is fine", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": t.TempDir()})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("pnpm 9 layout is fine", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "v3", "files", "00", "abc"), "")
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": dir})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("v11 without the links directory is fine", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "v11", "files", "00", "abc"), "")
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": dir})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("healthy binary is fine", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": store(t, healthy)})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("0-byte pnpm binary is damage", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": store(t, empty)})
		r.wantCode(t, damageExit)
		r.mustHave(t,
			"::error title=",
			"0-byte",
			"pnpm/action-setup",
			"v6.1.0",
			"11.22.0",
			"cache volume",
			docsURL,
			"pnpm/exe/", // the offending entry, relative to the store
		)
		// An annotation is one line, so the docs link at its end is on it.
		for _, line := range strings.Split(strings.TrimSpace(r.out), "\n") {
			if strings.HasPrefix(line, "::error") && !strings.Contains(line, docsURL) {
				t.Errorf("annotation line lacks the docs link, so the message was split: %q", line)
			}
		}
	})

	t.Run("npm_config_store_dir is the fallback", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"npm_config_store_dir": store(t, empty)})
		r.wantCode(t, damageExit)
		r.mustHave(t, "::error")
	})

	t.Run("pnpm_config_store_dir wins over npm_config_store_dir", func(t *testing.T) {
		r := runHook(t, check, map[string]string{
			"pnpm_config_store_dir": store(t, healthy),
			"npm_config_store_dir":  store(t, empty),
		})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("empty pnpm_config_store_dir falls back to npm_config_store_dir", func(t *testing.T) {
		r := runHook(t, check, map[string]string{
			"pnpm_config_store_dir": "",
			"npm_config_store_dir":  store(t, empty),
		})
		r.wantCode(t, damageExit)
	})

	t.Run("0-byte files elsewhere are not damage", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "v11/files/00/abc"), "")
		mustWrite(t, filepath.Join(dir, "v11/links/@pnpm/exe/11.7.0/aaaa/node_modules/@pnpm/exe/pnpm.cjs"), "")
		mustWrite(t, filepath.Join(dir, "v11/links/@pnpm/exe/11.7.0/aaaa/node_modules/@pnpm/exe/package.json"), "")
		mustWrite(t, filepath.Join(dir, "v11/links/other/1.0.0/bbbb/node_modules/other/pnpm"), "")
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": dir})
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("store path with spaces", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "my store")
		mustWrite(t, filepath.Join(dir, pnpmBinaryRel), "")
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": dir})
		r.wantCode(t, damageExit)
	})

	t.Run("unreadable links directory is not damage and does not fail the hook", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads everything")
		}
		dir := t.TempDir()
		links := filepath.Join(dir, "v11", "links", "@pnpm")
		mustWrite(t, filepath.Join(links, "exe", "x"), "x")
		if err := os.Chmod(links, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(links, 0o755) })
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": dir})
		r.wantCode(t, 0)
	})
}

// --- job-started.sh (dispatcher) ----------------------------------------------

// stage copies the real dispatcher into a temp tree and returns its path with
// the (empty) drop-in directory next to it, which is where the dispatcher looks.
func stage(t *testing.T) (dispatcher, dropins string) {
	t.Helper()
	requireBash(t)
	root := t.TempDir()
	dispatcher = filepath.Join(root, "job-started.sh")
	mustCopy(t, filepath.Join(hooksDir(t), "job-started.sh"), dispatcher)
	dropins = filepath.Join(root, "job-started.d")
	if err := os.MkdirAll(dropins, 0o755); err != nil {
		t.Fatal(err)
	}
	return dispatcher, dropins
}

func addCheck(t *testing.T, dropins, name, body string) {
	t.Helper()
	mustWrite(t, filepath.Join(dropins, name), body)
}

const (
	passing   = "echo \"ran $0\"\n"
	detecting = "echo '::error title=Test damage::something is damaged'\nexit 10\n"
)

func TestDispatcherWithoutDropins(t *testing.T) {
	t.Run("empty directory", func(t *testing.T) {
		d, _ := stage(t)
		r := runHook(t, d, nil)
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})
	t.Run("missing directory", func(t *testing.T) {
		d, dropins := stage(t)
		if err := os.Remove(dropins); err != nil {
			t.Fatal(err)
		}
		r := runHook(t, d, nil)
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})
	t.Run("only files that are not checks", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "README", detecting)
		addCheck(t, dropins, "10-notes.txt", detecting)
		addCheck(t, dropins, "20-backup.sh~", detecting)
		if err := os.MkdirAll(filepath.Join(dropins, "30-dir.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		r := runHook(t, d, nil)
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})
}

func TestDispatcherOutcomes(t *testing.T) {
	t.Run("passing check", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-ok.sh", passing)
		r := runHook(t, d, nil)
		r.wantCode(t, 0)
		r.mustHave(t, "ran "+filepath.Join(dropins, "10-ok.sh"))
		r.mustNotHave(t, "::")
	})

	t.Run("detecting check fails the job by default", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", detecting) // mode 0644: run through bash, not exec
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, "::error title=Test damage::something is damaged", "10-bad.sh", "GARM_JOB_HOOKS")
	})

	t.Run("crashing check is a warning and the run continues", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-crash.sh", "echo partial\nexit 3\n")
		addCheck(t, dropins, "20-syntax.sh", "if then fi (\n")
		addCheck(t, dropins, "30-missing-cmd.sh", "no-such-command-garm\n")
		addCheck(t, dropins, "40-ok.sh", passing)
		r := runHook(t, d, nil)
		r.wantCode(t, 0)
		r.mustHave(t, "::warning", "10-crash.sh", "20-syntax.sh", "30-missing-cmd.sh", "ran "+filepath.Join(dropins, "40-ok.sh"))
		r.mustNotHave(t, "::error")
	})

	t.Run("crashing check does not hide damage found by another", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-crash.sh", "exit 2\n")
		addCheck(t, dropins, "20-bad.sh", detecting)
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, "::warning", "10-crash.sh", "::error title=Test damage")
	})

	t.Run("every check runs after the first detection", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", detecting)
		addCheck(t, dropins, "20-bad2.sh", "echo '::error title=Second::second damage'\nexit 10\n")
		addCheck(t, dropins, "30-ok.sh", passing)
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, "something is damaged", "second damage", "ran "+filepath.Join(dropins, "30-ok.sh"))
	})

	t.Run("checks see the job environment", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-env.sh", "echo \"store=${pnpm_config_store_dir}\"\n")
		r := runHook(t, d, map[string]string{"pnpm_config_store_dir": "/some/store"})
		r.wantCode(t, 0)
		r.mustHave(t, "store=/some/store")
	})
}

func TestDispatcherOrder(t *testing.T) {
	d, dropins := stage(t)
	// Created in a scrambled order on purpose: the dispatcher sorts by name,
	// bytewise, whatever the locale.
	for _, n := range []string{"20-b.sh", "05-a.sh", "100-late.sh", "10-c.sh", "B.sh", "a.sh"} {
		addCheck(t, dropins, n, "echo \"seq "+n+"\"\n")
	}
	r := runHook(t, d, nil)
	r.wantCode(t, 0)

	var order []string
	re := regexp.MustCompile(`^seq (\S+)$`)
	for _, line := range strings.Split(strings.TrimSpace(r.out), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			order = append(order, m[1])
		}
	}
	want := []string{"05-a.sh", "10-c.sh", "100-late.sh", "20-b.sh", "B.sh", "a.sh"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("run order = %v, want %v", order, want)
	}
}

// The per-check limit is a constant in the script (no environment knob), so
// this test shortens it in its own copy of the dispatcher.
func TestDispatcherTimeout(t *testing.T) {
	requireTimeout(t)
	d, dropins := stage(t)

	src, err := os.ReadFile(d)
	if err != nil {
		t.Fatal(err)
	}
	const shipped = "readonly CHECK_TIMEOUT_SECONDS=30\n"
	if strings.Count(string(src), shipped) != 1 {
		t.Fatalf("the dispatcher no longer declares %q", strings.TrimSpace(shipped))
	}
	mustWrite(t, d, strings.Replace(string(src), shipped, "readonly CHECK_TIMEOUT_SECONDS=1\n", 1))

	addCheck(t, dropins, "10-hang.sh", "echo started\nsleep 60\n")
	addCheck(t, dropins, "20-ok.sh", passing)

	start := time.Now()
	r := runHook(t, d, nil)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a hanging check held the hook for %s", elapsed)
	}
	r.wantCode(t, 0)
	r.mustHave(t, "::warning", "10-hang.sh", "timed out", "ran "+filepath.Join(dropins, "20-ok.sh"))
	r.mustNotHave(t, "::error")
}

// There is no environment variable that changes the limit.
func TestDispatcherHasNoTimeoutKnob(t *testing.T) {
	d, _ := stage(t)
	src, err := os.ReadFile(d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "GARM_JOB_HOOKS_TIMEOUT") {
		t.Error("the dispatcher still reads GARM_JOB_HOOKS_TIMEOUT")
	}
}

func TestDispatcherModes(t *testing.T) {
	ptr := func(s string) *string { return &s }
	cases := []struct {
		name     string
		mode     *string // nil: variable unset
		wantCode int
		wantRun  bool // the checks run at all
	}{
		{"unset defaults to fail", nil, 1, true},
		{"empty defaults to fail", ptr(""), 1, true},
		{"fail", ptr("fail"), 1, true},
		{"warn", ptr("warn"), 0, true},
		{"off", ptr("off"), 0, false},
		{"mode is case-insensitive", ptr("WARN"), 0, true},
		{"unknown value is treated as fail", ptr("bogus"), 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, dropins := stage(t)
			addCheck(t, dropins, "10-bad.sh", detecting)
			env := map[string]string{}
			if tc.mode != nil {
				env["GARM_JOB_HOOKS"] = *tc.mode
			}
			r := runHook(t, d, env)
			r.wantCode(t, tc.wantCode)
			if tc.wantRun {
				r.mustHave(t, "::error title=Test damage")
			} else {
				r.mustNotHave(t, "::")
			}
		})
	}

	t.Run("warn mode still prints the detection and says it continues", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", detecting)
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "warn"})
		r.wantCode(t, 0)
		r.mustHave(t, "something is damaged", "warn")
	})

	t.Run("off does not run the checks", func(t *testing.T) {
		d, dropins := stage(t)
		marker := filepath.Join(t.TempDir(), "ran")
		addCheck(t, dropins, "10-marker.sh", "touch '"+marker+"'\n")
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "off"})
		r.wantCode(t, 0)
		if _, err := os.Stat(marker); err == nil {
			t.Error("a check ran with GARM_JOB_HOOKS=off")
		}
	})

	t.Run("unknown value is named in a warning, also when nothing is damaged", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-ok.sh", passing)
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "wran"})
		r.wantCode(t, 0)
		r.mustHave(t, "::warning", "wran", "fail")
	})

	t.Run("unknown value cannot inject a workflow command", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-ok.sh", passing)
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "x\n::error::injected"})
		for _, line := range strings.Split(r.out, "\n") {
			if strings.HasPrefix(line, "::error::injected") {
				t.Errorf("the value started its own workflow command: %q", line)
			}
		}
	})
}

// --- the shipped hooks end to end --------------------------------------------

// TestShippedHooksAsInstalled stages the real dispatcher with the real drop-in
// directory, the layout of /opt/garm/hooks in the image.
func TestShippedHooksAsInstalled(t *testing.T) {
	requireBash(t)
	root := t.TempDir()
	mustCopy(t, filepath.Join(hooksDir(t), "job-started.sh"), filepath.Join(root, "job-started.sh"))
	entries, err := os.ReadDir(filepath.Join(hooksDir(t), "job-started.d"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		mustCopy(t, filepath.Join(hooksDir(t), "job-started.d", e.Name()), filepath.Join(root, "job-started.d", e.Name()))
	}
	hook := filepath.Join(root, "job-started.sh")

	store := t.TempDir()
	env := func(extra map[string]string) map[string]string {
		m := map[string]string{"pnpm_config_store_dir": store, "npm_config_store_dir": store}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	t.Run("clean store passes", func(t *testing.T) {
		mustWrite(t, filepath.Join(store, pnpmBinaryRel), "binary")
		r := runHook(t, hook, env(nil))
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("damaged store fails the job", func(t *testing.T) {
		mustWrite(t, filepath.Join(store, pnpmBinaryRel), "")
		r := runHook(t, hook, env(nil))
		r.wantCode(t, 1)
		r.mustHave(t, "::error", "0-byte")
	})

	t.Run("damaged store with warn continues", func(t *testing.T) {
		mustWrite(t, filepath.Join(store, pnpmBinaryRel), "")
		r := runHook(t, hook, env(map[string]string{"GARM_JOB_HOOKS": "warn"}))
		r.wantCode(t, 0)
		r.mustHave(t, "::error", "0-byte")
	})

	t.Run("damaged store with off is silent", func(t *testing.T) {
		mustWrite(t, filepath.Join(store, pnpmBinaryRel), "")
		r := runHook(t, hook, env(map[string]string{"GARM_JOB_HOOKS": "off"}))
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})

	t.Run("no store at all passes", func(t *testing.T) {
		r := runHook(t, hook, nil)
		r.wantCode(t, 0)
		r.mustNotHave(t, "::")
	})
}

// --- Dockerfiles ---------------------------------------------------------------

func TestDockerfilesInstallTheHooks(t *testing.T) {
	root := repoRoot(t)
	for _, name := range []string{"noble", "resolute"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, "runner-images", name, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			df := string(raw)

			// Root-owned (COPY's default, so no --chown) and mode 0755, so the
			// job's user cannot replace a hook before it runs.
			for _, want := range []string{
				"COPY --chmod=0755 common/hooks/job-started.sh /opt/garm/hooks/job-started.sh",
				"COPY --chmod=0755 common/hooks/job-started.d/ /opt/garm/hooks/job-started.d/",
				"ENV ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/garm/hooks/job-started.sh",
			} {
				if !strings.Contains(df, want) {
					t.Errorf("Dockerfile lacks %q", want)
				}
			}
			if regexp.MustCompile(`(?m)^\s*(COPY|ADD)\b[^\n]*--chown[^\n]*/opt/garm`).MatchString(df) {
				t.Error("the hooks must stay root-owned: no --chown on /opt/garm")
			}
			if regexp.MustCompile(`(?m)^\s*RUN\b[^\n]*(chown|chmod)[^\n]*/opt/garm`).MatchString(df) {
				t.Error("the hooks must not be re-owned or re-moded after COPY")
			}
		})
	}
}

func TestHookSourcesFollowTheRunnersRules(t *testing.T) {
	// The runner refuses a hook path that does not end in .sh, and the
	// dispatcher only runs *.sh drop-ins.
	dir := hooksDir(t)
	if _, err := os.Stat(filepath.Join(dir, "job-started.sh")); err != nil {
		t.Fatalf("dispatcher missing: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "job-started.d"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no drop-ins shipped")
	}
	nameRE := regexp.MustCompile(`^\d{2}-[a-z0-9-]+\.sh$`)
	for _, e := range entries {
		if !nameRE.MatchString(e.Name()) {
			t.Errorf("drop-in %q does not follow the NN-name.sh convention, so it may never run", e.Name())
		}
	}
}
