package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/metadata"
)

// runnerListeningMarker is the line actions/runner prints on stdout once its listener
// session is open and it can be assigned a job.
const runnerListeningMarker = "Listening for Jobs"

// statusPostTimeout bounds one status POST by the watcher, retries included.
const statusPostTimeout = 20 * time.Second

// maxLogLineBytes is the read buffer for scanning runner output. Longer lines
// are seen in fragments of this size, which is harmless: the marker is a
// short line of its own.
const maxLogLineBytes = 64 << 10

// Messages recorded in GARM's instance event log alongside each status.
const (
	statusMessageInstalling = "runner container started and credentials delivered by the provider"
	statusMessageIdle       = "runner is listening for jobs"
)

// reportRunnerStatus starts the watcher that reports the runner through GARM's instance
// callback: installing, then idle once it is listening, or installing again with the
// reason if it never is (status_watch.go, ADR-002, Status reporting). The watcher is
// detached because GARM can kill a slow CreateInstance; only where orphans would not be
// reaped does it run inside this call (startStatusWatcher). This only logs: status
// reporting never fails an allocation.
func (p *Provider) reportRunnerStatus(ctx context.Context, b params.BootstrapInstance, containerID string) {
	sr := p.cfg.StatusReport
	if !sr.Enabled() {
		slog.DebugContext(ctx, "CreateInstance: runner status reporting disabled ([status_report].timeout_seconds = 0)",
			"instance", b.Name)
		return
	}
	if !b.JitConfigEnabled {
		// GARM has no agent ID for a non-JIT runner, and an ID-less idle would leave it
		// unable to remove the GitHub registration (runner/pool/pool.go DeleteRunner).
		// Non-JIT runners exist only on pools, which move runners to active with forced
		// updates and need no reports.
		slog.DebugContext(ctx, "CreateInstance: runner status reporting skipped for a non-JIT runner",
			"instance", b.Name)
		return
	}
	if b.CallbackURL == "" {
		slog.WarnContext(ctx, "CreateInstance: GARM sent no callback URL; runner status is not reported and GARM keeps the runner pending",
			"instance", b.Name)
		return
	}

	// Validate here so a bad URL reaches this call's logger. The watcher's output is
	// discarded unless [logging] file is set.
	if _, err := metadata.NewStatusReporter(b.CallbackURL, b.InstanceToken, b.CACertBundle); err != nil {
		slog.WarnContext(ctx, "CreateInstance: cannot report runner status: invalid callback URL (continuing)",
			"instance", b.Name, "error", logging.Redact(err.Error(), b.InstanceToken))
		return
	}

	req := statusWatchRequest{
		Instance:      b.Name,
		ContainerID:   containerID,
		DockerHost:    p.cfg.DockerHost,
		CallbackURL:   b.CallbackURL,
		InstanceToken: b.InstanceToken,
		CACertBundle:  b.CACertBundle,
		Deadline:      time.Now().Add(sr.Timeout()),
		ProxyEnv:      currentProxyEnv(),
	}
	if file := p.cfg.Logging.File; file != "" {
		req.LogFile = file
		req.LogLevel = logging.LevelFromEnv().String()
	}
	if err := p.spawnStatusWatcher(ctx, req); err != nil {
		slog.WarnContext(ctx, "CreateInstance: could not start the runner status watcher; the runner stays pending (continuing)",
			"instance", b.Name, "error", logging.Redact(err.Error(), b.InstanceToken))
		return
	}
	slog.InfoContext(ctx, "CreateInstance: started the runner status watcher; it reports installing, then idle once the runner is listening for jobs",
		"instance", b.Name, "timeout", sr.Timeout())
}

// postStatus sends one status report under its own bounded context.
func postStatus(ctx context.Context, r *metadata.StatusReporter, status, message string) error {
	postCtx, cancel := context.WithTimeout(ctx, statusPostTimeout)
	defer cancel()
	return r.PostStatus(postCtx, status, message)
}

// Readiness fallback when the runner's logs cannot be read (log driver "none",
// or a remote driver with the daemon's dual-logging cache disabled): poll the
// container's process list for the actions/runner listener and require it to
// stay up for listenerSettle. Variables so tests can shorten them.
var (
	listenerPollInterval = time.Second
	listenerSettle       = 5 * time.Second
)

// runnerListenerProcess is the actions/runner listener binary; run.sh starts
// it as /actions-runner/bin/Runner.Listener.
const runnerListenerProcess = "Runner.Listener"

// errRunnerStopped reports that the runner container stopped (or vanished)
// while the provider was waiting for it to come online.
var errRunnerStopped = errors.New("runner container is no longer running")

// errLogStreamEnded reports that the runner's log stream closed (the container
// stopped) before the listening marker appeared.
var errLogStreamEnded = errors.New("runner log stream ended before the runner was listening")

// waitRunnerListening follows the runner container's stdout until it prints
// runnerListeningMarker (true), the stream ends because the container stopped,
// or timeout elapses (false, with the reason). The stream is the daemon's
// stdcopy-multiplexed framing (the runner has no TTY), demultiplexed here.
// When the daemon cannot return the logs at all, it falls back to
// waitListenerProcess within the same timeout.
//
// viaProcess reports that the fallback (not the log line) decided readiness.
func (p *Provider) waitRunnerListening(ctx context.Context, containerID string, timeout time.Duration) (ok, viaProcess bool, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rc, err := p.cli.ContainerLogs(waitCtx, containerID, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		if waitCtx.Err() != nil {
			return false, false, waitCtx.Err()
		}
		// A real daemon answers 501 "configured logging driver does not
		// support reading" for the "none" driver. The container's log driver
		// is the operator's choice, so fall back instead of overriding it.
		slog.WarnContext(ctx, "cannot read the runner's logs; falling back to watching for the runner listener process, which is weaker evidence that the runner is online than its \"Listening for Jobs\" line. Use a log driver the daemon can read back (json-file or local, or enable dual logging) for exact detection",
			"container_id", containerID, "error", err)
		ok, err := p.waitListenerProcess(waitCtx, containerID)
		if ok {
			ok, err = p.confirmRunning(waitCtx, containerID)
		}
		return ok, true, err
	}
	defer rc.Close()

	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		_, copyErr := stdcopy.StdCopy(pw, io.Discard, rc)
		_ = pw.CloseWithError(copyErr)
	}()

	if containsLine(pr, runnerListeningMarker) {
		// A stopped container's logs still replay its buffered lines, so the
		// marker alone does not prove the runner is still up.
		ok, err := p.confirmRunning(waitCtx, containerID)
		return ok, false, err
	}
	if waitCtx.Err() != nil {
		return false, false, waitCtx.Err()
	}
	return false, false, errLogStreamEnded
}

// confirmRunning reports whether the runner is still running after a readiness signal.
// A runner that exited must not be reported idle: GARM's scale-set reaper only collects
// pending and installing runners, so that report would strand it.
func (p *Provider) confirmRunning(ctx context.Context, containerID string) (bool, error) {
	info, err := p.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, errRunnerStopped
		}
		return false, fmt.Errorf("inspecting the runner container: %w", err)
	}
	st := info.State
	if st == nil || !st.Running || st.Restarting || st.Paused || st.Dead || st.OOMKilled {
		return false, errRunnerStopped
	}
	return true, nil
}

// waitListenerProcess polls the container's process list until the
// actions/runner listener has been running continuously for listenerSettle
// (true), the container stops (errRunnerStopped), or ctx ends.
func (p *Provider) waitListenerProcess(ctx context.Context, containerID string) (bool, error) {
	ticker := time.NewTicker(listenerPollInterval)
	defer ticker.Stop()

	var firstSeen time.Time
	for {
		top, err := p.cli.ContainerTop(ctx, containerID, nil)
		switch {
		case err != nil && (errdefs.IsNotFound(err) || errdefs.IsConflict(err)):
			// The daemon answers 409 Conflict for top on a stopped container
			// and 404 once it is removed.
			return false, errRunnerStopped
		case err == nil && hasProcess(top, runnerListenerProcess):
			if firstSeen.IsZero() {
				firstSeen = time.Now()
			}
			if time.Since(firstSeen) >= listenerSettle {
				return true, nil
			}
		default:
			// Not up yet (or a transient top failure): restart the settle.
			firstSeen = time.Time{}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// hasProcess reports whether any process's command line (the CMD/COMMAND
// column of a top response) contains name.
func hasProcess(top container.TopResponse, name string) bool {
	col := slices.IndexFunc(top.Titles, func(t string) bool { return t == "CMD" || t == "COMMAND" })
	if col < 0 {
		return false
	}
	for _, proc := range top.Processes {
		if col < len(proc) && strings.Contains(proc[col], name) {
			return true
		}
	}
	return false
}

// containsLine reports whether any line read from r contains marker. It stops
// at the first match or at the first read error (EOF included).
func containsLine(r io.Reader, marker string) bool {
	br := bufio.NewReaderSize(r, maxLogLineBytes)
	for {
		line, _, err := br.ReadLine()
		if strings.Contains(string(line), marker) {
			return true
		}
		if err != nil {
			return false
		}
	}
}
