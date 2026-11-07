//go:build unix

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// watcherHandoffTimeout bounds writing the request into the watcher's stdin.
const watcherHandoffTimeout = 5 * time.Second

// watcherArgv is the watcher's full command line. The request, with its token and
// callback URL, never appears in argv.
func watcherArgv(exe string) []string {
	return []string{exe, StatusWatcherArg}
}

// watcherExecEnvVars are the only variables the watcher's exec environment carries,
// when set. They hold no secret and must be present at start: Go loads root CAs from
// SSL_CERT_FILE and SSL_CERT_DIR once, on first use, so a callback trusted through them
// must stay trusted, and the runtime reads GODEBUG at start-up. Proxy variables can
// carry credentials, so they travel in the stdin request instead.
var watcherExecEnvVars = []string{"SSL_CERT_FILE", "SSL_CERT_DIR", "GODEBUG"}

// watcherExecEnv returns the watcher's environment: the set
// watcherExecEnvVars of this process, and nothing else. Never nil, because a
// nil environment would make the child inherit this process's.
func watcherExecEnv() []string {
	env := []string{}
	for _, k := range watcherExecEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// watcherProcAttr is how the watcher is started: stdin is the private request pipe, and
// only watcherExecEnvVars reach its environment.
func watcherProcAttr(stdin, devNull *os.File, env []string) *os.ProcAttr {
	if env == nil {
		env = []string{}
	}
	return &os.ProcAttr{
		Dir: "/", // so it pins no directory
		Env: env,
		// GARM reads the provider's stdout and stderr until EOF, so a watcher holding either
		// would keep CreateInstance open. Nothing else is inherited: the runtime opens
		// every other descriptor close-on-exec.
		Files: []*os.File{stdin, devNull, devNull},
		// A new session: signals aimed at the provider's process group do not reach the
		// watcher, and it has no controlling terminal.
		Sys: &syscall.SysProcAttr{Setsid: true},
	}
}

// startDetachedWatcher re-executes the provider binary as a detached watcher
// and hands it req over its stdin. It returns as soon as the request is
// written; it does not wait for the watcher, which outlives this process and
// is reparented when it exits.
func startDetachedWatcher(_ context.Context, req statusWatchRequest) (err error) {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the provider binary: %w", err)
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encoding the status watch request: %w", err)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", os.DevNull, err)
	}
	defer devNull.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating the watcher pipe: %w", err)
	}
	defer pw.Close()

	proc, err := os.StartProcess(exe, watcherArgv(exe), watcherProcAttr(pr, devNull, watcherExecEnv()))
	_ = pr.Close() // the watcher holds its own copy
	if err != nil {
		return fmt.Errorf("starting the status watcher: %w", err)
	}
	defer func() {
		if err != nil {
			_ = proc.Kill()
			_, _ = proc.Wait()
			return
		}
		_ = proc.Release()
	}()

	if err := pw.SetWriteDeadline(time.Now().Add(watcherHandoffTimeout)); err != nil && !errors.Is(err, os.ErrNoDeadline) {
		return fmt.Errorf("handing the request to the status watcher: %w", err)
	}
	if _, err := pw.Write(payload); err != nil {
		return fmt.Errorf("handing the request to the status watcher: %w", err)
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("handing the request to the status watcher: %w", err)
	}
	return nil
}
