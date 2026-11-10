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

	// failTitle and warnTitle are the annotation prefixes the dispatcher uses
	// for a check named 10-bad.sh. The colon in a title is escaped as %3A.
	failPrefix = "::error title=Job-started check failed%3A bad::"
	warnPrefix = "::warning title=Job-started check found damage%3A bad::"
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

// linesWithPrefix returns the output lines that start with prefix.
func (r result) linesWithPrefix(prefix string) []string {
	var out []string
	for _, l := range strings.Split(r.out, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
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
		if strings.TrimSpace(r.out) != "" {
			t.Errorf("output = %q, want none", r.out)
		}
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

	t.Run("0-byte pnpm binary is damage, reported as a plain message", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"pnpm_config_store_dir": store(t, empty)})
		r.wantCode(t, damageExit)
		r.mustHave(t,
			"0-byte",
			"pnpm/action-setup",
			"v6.1.0",
			"11.22.0",
			"cache volume",
			docsURL,
			"pnpm/exe/", // the offending entry, relative to the store
		)
		// The dispatcher owns the annotation level: the check prints no
		// workflow command.
		r.mustNotHave(t, "::error", "::warning")
		if n := len(strings.Split(strings.TrimSpace(r.out), "\n")); n != 1 {
			t.Errorf("message spans %d lines, want 1:\n%s", n, r.out)
		}
	})

	t.Run("npm_config_store_dir is the fallback", func(t *testing.T) {
		r := runHook(t, check, map[string]string{"npm_config_store_dir": store(t, empty)})
		r.wantCode(t, damageExit)
		r.mustHave(t, "0-byte")
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

// A healthy check's output is discarded, so tests that need to know a check
// ran have it append to the file named by TRACE.
func traceEnv(t *testing.T) (path string, env map[string]string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "trace")
	return path, map[string]string{"TRACE": path}
}

func readTrace(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func traceCheck(name string) string { return "echo " + name + " >> \"$TRACE\"\n" }

const (
	passing   = "exit 0\n"
	detecting = "echo 'something is damaged'\nexit 10\n"
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
	t.Run("passing check is silent", func(t *testing.T) {
		d, dropins := stage(t)
		trace, env := traceEnv(t)
		addCheck(t, dropins, "10-ok.sh", traceCheck("ok")+"echo 'chatter that is dropped'\n")
		r := runHook(t, d, env)
		r.wantCode(t, 0)
		r.mustNotHave(t, "::", "chatter")
		if got := readTrace(t, trace); len(got) != 1 {
			t.Errorf("the check ran %d times, want 1", len(got))
		}
	})

	t.Run("detecting check fails the job by default, as an error", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", detecting) // mode 0644: run through bash, not exec
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, failPrefix+"something is damaged", "::error title=Job stopped by a job-started check::", "10-bad.sh")
		r.mustNotHave(t, "::warning")
	})

	t.Run("crashing check is a warning and the run continues", func(t *testing.T) {
		d, dropins := stage(t)
		trace, env := traceEnv(t)
		addCheck(t, dropins, "10-crash.sh", "echo partial\nexit 3\n")
		addCheck(t, dropins, "20-syntax.sh", "if then fi (\n")
		addCheck(t, dropins, "30-missing-cmd.sh", "no-such-command-garm\n")
		addCheck(t, dropins, "40-ok.sh", traceCheck("ok"))
		r := runHook(t, d, env)
		r.wantCode(t, 0)
		r.mustHave(t, "::warning title=Job-started check malfunctioned%3A crash::", "10-crash.sh exited with status 3", "20-syntax.sh", "30-missing-cmd.sh")
		r.mustNotHave(t, "::error")
		if got := readTrace(t, trace); strings.Join(got, ",") != "ok" {
			t.Errorf("trace = %v, want the last check to have run", got)
		}
	})

	t.Run("crashing check does not hide damage found by another", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-crash.sh", "exit 2\n")
		addCheck(t, dropins, "20-bad.sh", detecting)
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, "::warning", "10-crash.sh", "::error title=Job-started check failed%3A bad::")
	})

	t.Run("every check runs after the first detection", func(t *testing.T) {
		d, dropins := stage(t)
		trace, env := traceEnv(t)
		addCheck(t, dropins, "10-bad.sh", detecting)
		addCheck(t, dropins, "20-bad2.sh", "echo 'second damage'\nexit 10\n")
		addCheck(t, dropins, "30-ok.sh", traceCheck("ok"))
		r := runHook(t, d, env)
		r.wantCode(t, 1)
		r.mustHave(t, "something is damaged", "second damage")
		if got := readTrace(t, trace); strings.Join(got, ",") != "ok" {
			t.Errorf("trace = %v, want the third check to have run", got)
		}
	})

	t.Run("a detection with no message still says something", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", "exit 10\n")
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, failPrefix+"The check reported damage but printed no message.")
	})

	t.Run("checks see the job environment", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", "echo \"store=${pnpm_config_store_dir}\"\nexit 10\n")
		r := runHook(t, d, map[string]string{"pnpm_config_store_dir": "/some/store", "GARM_JOB_HOOKS": "warn"})
		r.wantCode(t, 0)
		r.mustHave(t, "store=/some/store")
	})
}

// The dispatcher owns the annotation level: the same check yields an error in
// fail mode and a warning in warn mode.
func TestDispatcherAnnotationLevelFollowsMode(t *testing.T) {
	t.Run("fail", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", detecting)
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "fail"})
		r.wantCode(t, 1)
		r.mustHave(t, failPrefix+"something is damaged")
		r.mustNotHave(t, "::warning")
	})

	t.Run("warn", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", detecting)
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "warn"})
		r.wantCode(t, 0)
		r.mustHave(t, warnPrefix+"something is damaged", "the job continues")
		r.mustNotHave(t, "::error")
		if n := len(r.linesWithPrefix("::")); n != 1 {
			t.Errorf("warn mode printed %d annotations, want exactly 1:\n%s", n, r.out)
		}
	})

	t.Run("a green job never shows an error annotation", func(t *testing.T) {
		// The shipped pnpm check, through the real dispatcher, in warn mode.
		requireBash(t)
		root := t.TempDir()
		mustCopy(t, filepath.Join(hooksDir(t), "job-started.sh"), filepath.Join(root, "job-started.sh"))
		mustCopy(t, filepath.Join(hooksDir(t), "job-started.d", "10-pnpm-store.sh"), filepath.Join(root, "job-started.d", "10-pnpm-store.sh"))
		store := t.TempDir()
		mustWrite(t, filepath.Join(store, pnpmBinaryRel), "")
		r := runHook(t, filepath.Join(root, "job-started.sh"), map[string]string{"pnpm_config_store_dir": store, "GARM_JOB_HOOKS": "warn"})
		r.wantCode(t, 0)
		r.mustHave(t, "::warning title=Job-started check found damage%3A pnpm-store::", "0-byte")
		r.mustNotHave(t, "::error")
	})
}

func TestDispatcherEscapesTheMessage(t *testing.T) {
	t.Run("percent and line breaks", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", "printf '100%% full\\nsecond line\\r\\nthird'\nexit 10\n")
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, failPrefix+"100%25 full%0Asecond line%0D%0Athird")
	})

	t.Run("already-escaped looking text is escaped again", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", "echo '%0A%25'\nexit 10\n")
		r := runHook(t, d, nil)
		r.mustHave(t, failPrefix+"%250A%2525")
	})

	t.Run("title escapes colon and comma", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-a,b:c.sh", detecting)
		r := runHook(t, d, nil)
		r.wantCode(t, 1)
		r.mustHave(t, "::error title=Job-started check failed%3A a%2Cb%3Ac::something is damaged")
	})

	t.Run("a check cannot start a workflow command of its own", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-bad.sh", strings.Join([]string{
			"echo '::error::injected'",
			"echo '::set-env name=EVIL::1'",
			"echo '::add-mask::secret'",
			"echo '::stop-commands::token'",
			"echo '::warning::injected'",
			"exit 10",
		}, "\n")+"\n")
		addCheck(t, dropins, "20-crash.sh", "echo '::set-env name=EVIL::2'\nexit 3\n")
		addCheck(t, dropins, "30-healthy.sh", "echo '::set-env name=EVIL::3'\nexit 0\n")
		r := runHook(t, d, nil)
		r.wantCode(t, 1)

		for _, l := range strings.Split(r.out, "\n") {
			if !strings.HasPrefix(l, "::") {
				continue
			}
			ok := strings.HasPrefix(l, failPrefix) ||
				strings.HasPrefix(l, "::warning title=Job-started check malfunctioned%3A crash::") ||
				strings.HasPrefix(l, "::error title=Job stopped by a job-started check::")
			if !ok {
				t.Errorf("unexpected workflow command at the start of a line: %q", l)
			}
		}
		// The text itself survives, inside the escaped annotation.
		r.mustHave(t, failPrefix+"::error::injected%0A::set-env name=EVIL::1%0A")
		if n := len(r.linesWithPrefix("::set-env")) + len(r.linesWithPrefix("::add-mask")) + len(r.linesWithPrefix("::stop-commands")); n != 0 {
			t.Errorf("%d injected commands at line start:\n%s", n, r.out)
		}
	})
}

func TestDispatcherCapsTheMessage(t *testing.T) {
	d, dropins := stage(t)
	trace, env := traceEnv(t)
	// Far more output than the cap, then a marker proving the check was not
	// killed by a closed pipe while it was still writing.
	addCheck(t, dropins, "10-bad.sh", "head -c 1000000 /dev/zero | tr '\\0' 'A'\ntouch \"$TRACE.done\"\nexit 10\n")
	addCheck(t, dropins, "20-ok.sh", traceCheck("ok"))
	r := runHook(t, d, env)
	r.wantCode(t, 1)

	lines := r.linesWithPrefix(failPrefix)
	if len(lines) != 1 {
		t.Fatalf("got %d failure annotations, want 1", len(lines))
	}
	if n := strings.Count(strings.TrimPrefix(lines[0], failPrefix), "A"); n != 4096 {
		t.Errorf("message keeps %d bytes of output, want exactly the 4096-byte cap", n)
	}
	if !strings.Contains(lines[0], "[output truncated]") {
		t.Errorf("a cut message must say so: %q", lines[0][len(lines[0])-60:])
	}
	if len(r.out) > 8192 {
		t.Errorf("the hook printed %d bytes for a runaway check, want it capped", len(r.out))
	}
	if _, err := os.Stat(trace + ".done"); err != nil {
		t.Error("the chatty check was killed before it finished")
	}
	if got := readTrace(t, trace); strings.Join(got, ",") != "ok" {
		t.Errorf("later checks did not run: %v", got)
	}
}

func TestDispatcherDoesNotTruncateAMessageAtTheCap(t *testing.T) {
	d, dropins := stage(t)
	addCheck(t, dropins, "10-bad.sh", "head -c 4096 /dev/zero | tr '\\0' 'A'\nexit 10\n")
	r := runHook(t, d, nil)
	r.wantCode(t, 1)
	r.mustNotHave(t, "[output truncated]")
	if n := strings.Count(strings.TrimPrefix(r.linesWithPrefix(failPrefix)[0], failPrefix), "A"); n != 4096 {
		t.Errorf("message keeps %d bytes, want all 4096", n)
	}
}

func TestDispatcherOrder(t *testing.T) {
	d, dropins := stage(t)
	trace, env := traceEnv(t)
	// Created in a scrambled order on purpose: the dispatcher sorts by name,
	// bytewise, whatever the locale.
	for _, n := range []string{"20-b.sh", "05-a.sh", "100-late.sh", "10-c.sh", "B.sh", "a.sh"} {
		addCheck(t, dropins, n, traceCheck(n))
	}
	r := runHook(t, d, env)
	r.wantCode(t, 0)

	want := []string{"05-a.sh", "10-c.sh", "100-late.sh", "20-b.sh", "B.sh", "a.sh"}
	if got := readTrace(t, trace); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("run order = %v, want %v", got, want)
	}
}

// The per-check limit is a constant in the script (no environment knob), so
// this test shortens it in its own copy of the dispatcher.
func TestDispatcherTimeout(t *testing.T) {
	requireTimeout(t)
	d, dropins := stage(t)
	trace, env := traceEnv(t)

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
	addCheck(t, dropins, "20-ok.sh", traceCheck("ok"))

	start := time.Now()
	r := runHook(t, d, env)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("a hanging check held the hook for %s", elapsed)
	}
	r.wantCode(t, 0)
	r.mustHave(t, "::warning title=Job-started check timed out%3A hang::", "10-hang.sh", "timed out")
	r.mustNotHave(t, "::error")
	if got := readTrace(t, trace); strings.Join(got, ",") != "ok" {
		t.Errorf("the check after the hanging one did not run: %v", got)
	}
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
		wantLvl  string // "error", "warning", or "" for no annotation at all
	}{
		{"unset defaults to fail", nil, 1, "error"},
		{"empty defaults to fail", ptr(""), 1, "error"},
		{"fail", ptr("fail"), 1, "error"},
		{"warn", ptr("warn"), 0, "warning"},
		{"off", ptr("off"), 0, ""},
		{"mode is case-insensitive", ptr("WARN"), 0, "warning"},
		{"unknown value is treated as fail", ptr("bogus"), 1, "error"},
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
			switch tc.wantLvl {
			case "error":
				r.mustHave(t, failPrefix)
			case "warning":
				r.mustHave(t, warnPrefix)
				r.mustNotHave(t, "::error")
			default:
				r.mustNotHave(t, "::")
			}
		})
	}

	t.Run("off does not run the checks", func(t *testing.T) {
		d, dropins := stage(t)
		trace, env := traceEnv(t)
		env["GARM_JOB_HOOKS"] = "off"
		addCheck(t, dropins, "10-marker.sh", traceCheck("ran"))
		r := runHook(t, d, env)
		r.wantCode(t, 0)
		if got := readTrace(t, trace); len(got) != 0 {
			t.Errorf("a check ran with GARM_JOB_HOOKS=off: %v", got)
		}
	})

	t.Run("unknown value is named in a warning, also when nothing is damaged", func(t *testing.T) {
		d, dropins := stage(t)
		addCheck(t, dropins, "10-ok.sh", passing)
		r := runHook(t, d, map[string]string{"GARM_JOB_HOOKS": "wran"})
		r.wantCode(t, 0)
		r.mustHave(t, "::warning title=Unknown GARM_JOB_HOOKS value::", "wran", "fail")
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
		r.mustHave(t, "::error title=Job-started check failed%3A pnpm-store::", "0-byte")
	})

	t.Run("damaged store with warn continues, with a warning only", func(t *testing.T) {
		mustWrite(t, filepath.Join(store, pnpmBinaryRel), "")
		r := runHook(t, hook, env(map[string]string{"GARM_JOB_HOOKS": "warn"}))
		r.wantCode(t, 0)
		r.mustHave(t, "::warning title=Job-started check found damage%3A pnpm-store::", "0-byte")
		r.mustNotHave(t, "::error")
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
