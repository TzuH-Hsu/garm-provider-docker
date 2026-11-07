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

// The GARM params.RunnerStatus values a provider can assert from outside the runner:
// installing once credentials are delivered, idle once it is listening for jobs.
const (
	RunnerStatusInstalling = "installing"
	RunnerStatusIdle       = "idle"
)

// statusPath is GARM's status callback route.
const statusPath = "/status"

// maxStatusResponseBytes caps how much of the response is drained so the connection
// can be reused. GARM answers with an empty 200.
const maxStatusResponseBytes = 64 << 10

// statusUpdate is the JSON form of GARM's params.InstanceUpdateMessage. agent_id
// is left out: GARM rejects a value that differs from the one on the row, and the
// provider has no authoritative source for it. Omitting it is only correct when
// GARM already stored the agent ID, so only JIT runners report status (ADR-002).
type statusUpdate struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// StatusReporter posts runner status to one instance's GARM callback endpoint.
// GARM's userdata templates make this call from inside a VM; the provider makes it
// so the runner container never sees the token.
type StatusReporter struct {
	client    *Client
	statusURL string
}

// NewStatusReporter returns a StatusReporter for callbackURL. It follows no
// redirects: a redirected POST either becomes a GET or re-sends the token to the
// Location.
func NewStatusReporter(callbackURL, instanceToken string, caCertBundle []byte, opts ...Option) (*StatusReporter, error) {
	c, err := newClient(callbackURLField, callbackURL, instanceToken, caCertBundle, opts...)
	if err != nil {
		return nil, err
	}
	c.httpClient.CheckRedirect = refuseRedirect
	return &StatusReporter{client: c, statusURL: statusEndpoint(callbackURL)}, nil
}

// refuseRedirect hands the 3xx response back for doPost to report.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// statusEndpoint appends statusPath to callbackURL unless it already ends in it, as
// GARM's userdata templates do.
func statusEndpoint(callbackURL string) string {
	base := strings.TrimRight(callbackURL, "/")
	if strings.HasSuffix(base, statusPath) {
		return base
	}
	return base + statusPath
}

// PostStatus reports status with a message, retrying transport errors and 5xx. Every
// returned error has the instance token scrubbed from it.
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

// doPost performs one POST. The bool reports whether the failure is worth retrying.
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
