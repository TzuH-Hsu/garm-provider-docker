package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/metadata"
)

// StatusWatcherArg is the hidden first argument that makes the provider binary
// run as the detached runner status watcher instead of a GARM command. GARM
// never passes arguments to an external provider, so it cannot collide with a
// real invocation. The argument carries no secret; the request arrives on stdin.
const StatusWatcherArg = "__garm-provider-docker-status-watcher"

// maxStatusWatchRequestBytes bounds the request read from stdin (the CA bundle
// is the only variable-size field).
const maxStatusWatchRequestBytes = 1 << 20

// statusWatchGrace is how far past the request deadline the watcher may run to
// send its final report before it exits unconditionally.
const statusWatchGrace = statusPostTimeout + 5*time.Second

// Messages the watcher records in GARM's instance event log.
const (
	statusMessageIdleViaProcess = "runner listener process is up; the runner's logs could not be read with the container's log driver (use json-file or local for exact detection)"
	statusMessageNotListening   = "runner was not listening for jobs before the status-report timeout; left installing for GARM's bootstrap timeout"
	statusMessageRunnerStopped  = "runner container stopped before it was listening for jobs"
)

// statusWatchRequest is everything the watcher needs. It travels from
// CreateInstance to the watcher process over a private stdin pipe only — never
// argv or the environment, which other processes of the same user can read.
type statusWatchRequest struct {
	Instance      string    `json:"instance"`
	ContainerID   string    `json:"container_id"`
	DockerHost    string    `json:"docker_host"`
	CallbackURL   string    `json:"callback_url"`
	InstanceToken string    `json:"instance_token"`
	CACertBundle  []byte    `json:"ca_cert_bundle,omitempty"`
	Deadline      time.Time `json:"deadline"`
}

// validate rejects a malformed request, or one whose deadline is already past
// or further out than the largest configurable timeout allows.
func (r statusWatchRequest) validate(now time.Time) error {
	switch {
	case r.Instance == "", r.ContainerID == "", r.DockerHost == "", r.CallbackURL == "", r.InstanceToken == "":
		return errors.New("incomplete status watch request")
	case !r.Deadline.After(now):
		return errors.New("status watch deadline already passed")
	case r.Deadline.After(now.Add(config.MaxStatusReportTimeoutSeconds*time.Second + time.Minute)):
		return errors.New("status watch deadline is beyond the largest allowed timeout")
	}
	return nil
}

// statusWatcherFunc starts idle detection for one runner.
type statusWatcherFunc func(ctx context.Context, req statusWatchRequest) error

// Option customizes a Provider.
type Option func(*Provider)

// WithInProcessStatusWatcher runs idle detection inside CreateInstance itself,
// synchronously, through the provider's own Docker client, instead of in a
// detached process. It exists for tests and embedding, where re-executing the
// binary is not possible; production uses the detached watcher.
func WithInProcessStatusWatcher() Option {
	return func(p *Provider) {
		p.spawnStatusWatcher = func(ctx context.Context, req statusWatchRequest) error {
			watchCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), req.Deadline.Add(statusWatchGrace))
			defer cancel()
			runStatusWatch(watchCtx, p, req)
			return nil
		}
	}
}

// RunStatusWatcher is the detached watcher's entry point (main.go dispatches
// to it on StatusWatcherArg). It reads the request from in, waits for the
// runner to come online within the request's deadline, reports the outcome to
// GARM, and returns the process exit code. It cannot outlive the deadline by
// more than statusWatchGrace: past that it exits regardless of what is still
// in flight.
func RunStatusWatcher(in io.Reader) int {
	raw, err := io.ReadAll(io.LimitReader(in, maxStatusWatchRequestBytes))
	if err != nil {
		return 2
	}
	var req statusWatchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 2
	}
	if err := req.validate(time.Now()); err != nil {
		return 2
	}

	hardStop := req.Deadline.Add(statusWatchGrace)
	time.AfterFunc(time.Until(hardStop)+time.Second, func() { os.Exit(3) })
	ctx, cancel := context.WithDeadline(context.Background(), hardStop)
	defer cancel()

	cli, err := docker.NewMobyClient(req.DockerHost)
	if err != nil {
		return 2
	}
	runStatusWatch(ctx, &Provider{cli: cli}, req)
	return 0
}

// runStatusWatch waits until req.Deadline for the runner to come online, then
// reports idle — or, if it never did, records why in GARM's event log while
// leaving the runner installing. Every error is logged with the token redacted.
func runStatusWatch(ctx context.Context, p *Provider, req statusWatchRequest) {
	reporter, err := metadata.NewStatusReporter(req.CallbackURL, req.InstanceToken, req.CACertBundle)
	if err != nil {
		slog.WarnContext(ctx, "status watcher: invalid callback URL",
			"instance", req.Instance, "error", logging.Redact(err.Error(), req.InstanceToken))
		return
	}

	ok, viaProcess, waitErr := p.waitRunnerListening(ctx, req.ContainerID, time.Until(req.Deadline))
	status, message := metadata.RunnerStatusIdle, statusMessageIdle
	switch {
	case ok && viaProcess:
		message = statusMessageIdleViaProcess
	case !ok && (errors.Is(waitErr, errRunnerStopped) || errors.Is(waitErr, errLogStreamEnded)):
		status, message = metadata.RunnerStatusInstalling, statusMessageRunnerStopped
	case !ok:
		status, message = metadata.RunnerStatusInstalling, statusMessageNotListening
	}

	if err := postStatus(ctx, reporter, status, message); err != nil {
		slog.WarnContext(ctx, "status watcher: failed to report runner status",
			"instance", req.Instance, "status", status, "error", err)
		return
	}
	slog.InfoContext(ctx, "status watcher: reported runner status",
		"instance", req.Instance, "status", status, "detail", message, "wait_error", fmt.Sprint(waitErr))
}
