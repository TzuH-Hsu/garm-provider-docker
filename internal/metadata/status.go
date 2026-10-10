package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Runner statuses the provider reports through GARM's instance callback. The
// strings are GARM's own params.RunnerStatus values (cloudbase/garm v0.2.1
// params/params.go). Only the two a provider can truthfully assert from the
// outside are exposed: "installing" once credentials are delivered, and
// "idle" once the runner is listening for jobs.
const (
	RunnerStatusInstalling = "installing"
	RunnerStatusIdle       = "idle"
)

// statusPath is the callback route GARM serves the status handler on
// (apiserver/routers/routers.go: callbacks "/status" and "/status/").
const statusPath = "/status"

// maxStatusResponseBytes caps how much of the callback response body is read
// (and discarded) so the connection can be reused. GARM answers with an empty
// 200; anything large is not something to buffer.
const maxStatusResponseBytes = 64 << 10

// statusUpdate is the JSON body GARM v0.2.1's InstanceStatusMessageHandler
// decodes into params.InstanceUpdateMessage (params/requests.go):
//
//	Status  RunnerStatus `json:"status,omitempty"`
//	Message string       `json:"message,omitempty"`
//	AgentID *int64       `json:"agent_id,omitempty"`
//
// agent_id is deliberately NOT part of this type. GARM forwards a non-nil
// agent_id into a non-forced UpdateInstance, whose validateAgentID rejects
// any value that differs from the one already on the row ("agent ID
// mismatch"); a scale-set runner's row already carries the agent ID GARM got
// when it generated the JIT config, and the provider has no authoritative
// source for it. Omitting the field leaves the row's agent ID untouched. That
// is only correct when GARM already stored the agent ID, i.e. for JIT runners
// (scale sets, and pools with JIT); the provider therefore reports status for
// JIT runners only (internal/provider/status_report.go).
type statusUpdate struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// StatusReporter posts runner status updates to one instance's GARM callback
// endpoint, authenticated with that instance's Bearer token — the same call
// GARM's own userdata templates make from inside a VM, made here by the
// provider process so the runner container never sees the token. Construct it
// with NewStatusReporter.
type StatusReporter struct {
	client    *Client
	statusURL string
}

// NewStatusReporter builds a StatusReporter for callbackURL (BootstrapInstance's
// callback-url). The URL gets the same transport-security validation as the
// metadata-url (https, or plain http only to a loopback host; no userinfo) and
// the same CA-bundle handling. Unlike the metadata client it follows NO
// redirects at all: GARM's callback has no legitimate reason to redirect, and
// a redirected POST is either silently rewritten into a GET (301/302/303) or
// re-sent with the Bearer token to wherever the Location points (307/308).
//
// The "/status" suffix is appended unless the URL already ends in "/status"
// (with or without a trailing slash), mirroring the normalization GARM's own
// userdata templates apply to the same value.
func NewStatusReporter(callbackURL, instanceToken string, caCertBundle []byte, opts ...Option) (*StatusReporter, error) {
	c, err := newClient(callbackURLField, callbackURL, instanceToken, caCertBundle, opts...)
	if err != nil {
		return nil, err
	}
	c.httpClient.CheckRedirect = refuseRedirect
	return &StatusReporter{client: c, statusURL: statusEndpoint(callbackURL)}, nil
}

// refuseRedirect is the StatusReporter redirect policy: never follow. Returning
// http.ErrUseLastResponse hands the 3xx response itself back to the caller,
// which PostStatus then reports as a refused redirect.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// statusEndpoint derives the status URL from GARM's callback URL.
func statusEndpoint(callbackURL string) string {
	base := strings.TrimRight(callbackURL, "/")
	if strings.HasSuffix(base, statusPath) {
		return base
	}
	return base + statusPath
}

// PostStatus reports status with a human-readable message. Transient failures
// (transport errors, 5xx) are retried with the client's bounded retry policy;
// any other non-2xx response is returned immediately. Every returned error has
// the instance token scrubbed from it.
func (r *StatusReporter) PostStatus(ctx context.Context, status, message string) error {
	body, err := json.Marshal(statusUpdate{Status: status, Message: message})
	if err != nil {
		return fmt.Errorf("failed to encode runner status update: %w", err)
	}

	c := r.client
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return c.redact(fmt.Errorf("reporting runner status %q: %w (last error: %v)", status, ctx.Err(), lastErr))
			case <-time.After(c.retryBackoff * time.Duration(attempt)):
			}
		}
		retryable, err := r.doPost(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable {
			return c.redact(fmt.Errorf("reporting runner status %q: %w", status, err))
		}
	}
	return c.redact(fmt.Errorf("reporting runner status %q: giving up after %d attempts: %w", status, c.maxRetries+1, lastErr))
}

// doPost performs one POST. The bool reports whether the failure is worth
// retrying (transport error or 5xx).
func (r *StatusReporter) doPost(ctx context.Context, body []byte) (retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.statusURL, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("failed to build callback request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.client.instanceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.httpClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("callback request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxStatusResponseBytes))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("callback endpoint returned %d", resp.StatusCode)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return false, fmt.Errorf("callback endpoint returned redirect %d; redirects are refused", resp.StatusCode)
	default:
		return false, fmt.Errorf("callback endpoint returned %d", resp.StatusCode)
	}
}
