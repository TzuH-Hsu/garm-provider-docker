package provider

import (
	"bufio"
	"context"
	"errors"
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

// runnerListeningMarker is the line actions/runner prints on STDOUT once its
// listener session is open and it can be assigned a job (observed in a live
// runner container's `docker logs`: "2026-10-10 17:45:14Z: Listening for
// Jobs"). It is the provider's signal that the runner is truly online.
const runnerListeningMarker = "Listening for Jobs"

// statusPostTimeout bounds one status POST, retries included, so a slow or
// unreachable GARM callback cannot stretch CreateInstance beyond the
// configured wait by more than this per report.
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

// reportRunnerStatus performs provider-side runner status reporting through
// GARM's instance callback (ADR-002 amendment), the step GARM's userdata
// templates perform from inside a VM:
//
//  1. report "installing" (credentials are delivered and the runner is
//     starting);
//  2. wait, bounded by [status_report].timeout_seconds, for the runner to
//     print runnerListeningMarker on stdout;
//  3. only then report "idle".
//
// GARM v0.2.1 scale sets move a runner to "active" with a CHECKED transition
// that "pending" and "installing" may not take, so without this a scale-set
// runner that picks up a job wedges GARM's scale-set listener. "idle" is
// deliberately never reported early: GARM's bootstrap-timeout reaper only
// collects runners still pending/installing, so a runner reported idle before
// it was listening — and that then failed to come up — would never be reaped.
// On a timeout the runner is therefore left "installing".
//
// Everything here is best-effort and only logs: status reporting must never
// fail or roll back an otherwise-healthy allocation. The instance token and
// the callback URL stay in this process's memory; the runner container never
// sees either.
func (p *Provider) reportRunnerStatus(ctx context.Context, b params.BootstrapInstance, containerID string) {
	sr := p.cfg.StatusReport
	if !sr.Enabled() {
		slog.DebugContext(ctx, "CreateInstance: runner status reporting disabled ([status_report].timeout_seconds = 0)",
			"instance", b.Name)
		return
	}
	if !b.JitConfigEnabled {
		// Non-JIT runners register through config.sh inside the container, so
		// GARM has no agent ID on the row; GARM's own bootstrap reports idle
		// together with the agent_id read from .runner, which this provider
		// cannot see. An ID-less idle would leave the row without an agent ID,
		// and GARM skips removing the GitHub registration of a runner whose
		// agent ID is 0 (runner/pool/pool.go DeleteRunner). Non-JIT only occurs
		// on pools (scale sets always use JIT), and pools move runners to
		// active with forced updates, so they do not need these reports.
		slog.DebugContext(ctx, "CreateInstance: runner status reporting skipped for a non-JIT runner",
			"instance", b.Name)
		return
	}
	if b.CallbackURL == "" {
		slog.WarnContext(ctx, "CreateInstance: GARM sent no callback URL; runner status is not reported and GARM keeps the runner pending",
			"instance", b.Name)
		return
	}

	reporter, err := metadata.NewStatusReporter(b.CallbackURL, b.InstanceToken, b.CACertBundle)
	if err != nil {
		slog.WarnContext(ctx, "CreateInstance: cannot report runner status: invalid callback URL (continuing)",
			"instance", b.Name, "error", logging.Redact(err.Error(), b.InstanceToken))
		return
	}

	if err := postStatus(ctx, reporter, metadata.RunnerStatusInstalling, statusMessageInstalling); err != nil {
		// A runner GARM still has at "pending" cannot legally move to "idle",
		// so there is nothing to gain from waiting for it.
		slog.WarnContext(ctx, "CreateInstance: failed to report runner status installing; skipping the idle report (continuing)",
			"instance", b.Name, "error", err)
		return
	}
	slog.InfoContext(ctx, "CreateInstance: reported runner status", "instance", b.Name, "status", metadata.RunnerStatusInstalling)

	timeout := sr.Timeout()
	listening, err := p.waitRunnerListening(ctx, containerID, timeout)
	if !listening {
		slog.WarnContext(ctx, "CreateInstance: runner did not report listening for jobs in time; leaving it installing so GARM's bootstrap timeout still applies",
			"instance", b.Name, "timeout", timeout, "error", err)
		return
	}

	if err := postStatus(ctx, reporter, metadata.RunnerStatusIdle, statusMessageIdle); err != nil {
		slog.WarnContext(ctx, "CreateInstance: failed to report runner status idle (continuing)",
			"instance", b.Name, "error", err)
		return
	}
	slog.InfoContext(ctx, "CreateInstance: reported runner status", "instance", b.Name, "status", metadata.RunnerStatusIdle)
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
func (p *Provider) waitRunnerListening(ctx context.Context, containerID string, timeout time.Duration) (bool, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rc, err := p.cli.ContainerLogs(waitCtx, containerID, container.LogsOptions{ShowStdout: true, Follow: true})
	if err != nil {
		if waitCtx.Err() != nil {
			return false, waitCtx.Err()
		}
		// A real daemon answers 501 "configured logging driver does not
		// support reading" for the "none" driver. The container's log driver
		// is the operator's choice, so fall back instead of overriding it.
		slog.WarnContext(ctx, "CreateInstance: cannot read the runner's logs; falling back to watching for the runner listener process, which is weaker evidence that the runner is online than its \"Listening for Jobs\" line. Use a log driver the daemon can read back (json-file or local, or enable dual logging) for exact detection",
			"container_id", containerID, "error", err)
		return p.waitListenerProcess(waitCtx, containerID)
	}
	defer rc.Close()

	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		_, copyErr := stdcopy.StdCopy(pw, io.Discard, rc)
		_ = pw.CloseWithError(copyErr)
	}()

	if containsLine(pr, runnerListeningMarker) {
		return true, nil
	}
	if waitCtx.Err() != nil {
		return false, waitCtx.Err()
	}
	return false, errLogStreamEnded
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
