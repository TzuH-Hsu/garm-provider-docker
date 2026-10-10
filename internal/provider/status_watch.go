package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
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
	statusMessageRunnerStopped  = "runner container stopped before it was ready for jobs"
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

	// ProxyEnv carries the provider process's proxy environment (the
	// proxyEnvVars that are set). The watcher starts with an empty
	// environment, but both its HTTP clients — the GARM callback and, for a
	// tcp:// docker_host, the Docker API — resolve proxies from the
	// environment exactly as the provider's do; without these the idle
	// report could take a different network path than the installing one.
	// Proxy URLs can embed credentials, so they travel here, not in the
	// watcher's exec environment.
	ProxyEnv map[string]string `json:"proxy_env,omitempty"`
}

// proxyEnvVars are the variables net/http's ProxyFromEnvironment reads (the
// Docker SDK's TCP transport uses the same function). The Docker client takes
// nothing else from the environment: it is built from docker_host alone.
var proxyEnvVars = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"}

// currentProxyEnv captures the set proxy variables of this process.
func currentProxyEnv() map[string]string {
	var env map[string]string
	for _, k := range proxyEnvVars {
		if v, ok := os.LookupEnv(k); ok {
			if env == nil {
				env = make(map[string]string, len(proxyEnvVars))
			}
			env[k] = v
		}
	}
	return env
}

// applyProxyEnv installs the carried proxy variables into this (watcher)
// process before any HTTP client is used — net/http reads them once. It sets
// only the known proxy variables. Setting them in-process does not change the
// environment the kernel exposes for the process, which is the empty one it
// was started with.
func applyProxyEnv(env map[string]string) error {
	for _, k := range proxyEnvVars {
		if v, ok := env[k]; ok {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// redact scrubs the instance token and any credentials embedded in the proxy
// URLs from s, for logging.
func (r statusWatchRequest) redact(s string) string {
	s = logging.Redact(s, r.InstanceToken)
	for _, v := range r.ProxyEnv {
		u, err := url.Parse(v)
		if err != nil || u.User == nil {
			continue
		}
		if pw, ok := u.User.Password(); ok && pw != "" {
			s = logging.Redact(s, pw)
			s = logging.Redact(s, url.QueryEscape(pw))
		}
		if name := u.User.Username(); name != "" {
			s = logging.Redact(s, u.User.String())
		}
	}
	return s
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
	if err := applyProxyEnv(req.ProxyEnv); err != nil {
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
			"instance", req.Instance, "error", req.redact(err.Error()))
		return
	}

	if err := postStatus(ctx, reporter, metadata.RunnerStatusInstalling, statusMessageInstalling); err != nil {
		// A runner GARM still has at "pending" cannot legally move to "idle",
		// so there is nothing to gain from waiting for it.
		slog.WarnContext(ctx, "status watcher: failed to report runner status installing; not waiting for the runner",
			"instance", req.Instance, "error", req.redact(err.Error()))
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
			"instance", req.Instance, "status", status, "error", req.redact(err.Error()))
		return
	}
	slog.InfoContext(ctx, "status watcher: reported runner status",
		"instance", req.Instance, "status", status, "detail", message, "wait_error", req.redact(fmt.Sprint(waitErr)))
}
