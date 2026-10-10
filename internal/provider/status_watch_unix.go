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

// watcherArgv is the watcher's full command line: the provider binary and the
// hidden StatusWatcherArg, nothing else — the request (instance token and
// callback URL included) never appears in argv.
func watcherArgv(exe string) []string {
	return []string{exe, StatusWatcherArg}
}

// watcherProcAttr is how the watcher is started:
//
//   - stdin is the read end of a private pipe the request is written into;
//   - stdout and stderr are /dev/null. GARM reads the provider's stdout and
//     stderr until EOF, so a watcher holding either pipe would keep GARM's
//     CreateInstance call open until the watcher exits. No other descriptor
//     is inherited (the runtime opens everything close-on-exec);
//   - an empty, non-nil environment: nothing from GARM's environment is
//     passed on, and the request is not in it either;
//   - Setsid: a new session and process group, so signals aimed at the
//     provider process or its group do not reach the watcher, and the
//     watcher has no controlling terminal;
//   - working directory "/", so it pins no directory.
func watcherProcAttr(stdin, devNull *os.File) *os.ProcAttr {
	return &os.ProcAttr{
		Dir:   "/",
		Env:   []string{},
		Files: []*os.File{stdin, devNull, devNull},
		Sys:   &syscall.SysProcAttr{Setsid: true},
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

	proc, err := os.StartProcess(exe, watcherArgv(exe), watcherProcAttr(pr, devNull))
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
