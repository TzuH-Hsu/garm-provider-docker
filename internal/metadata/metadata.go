// Package metadata fetches a runner's credentials from GARM's per-instance
// metadata service (ADR-002, research.md §1.C). The provider performs this
// fetch itself, holding the instance token that authenticates it and never
// passing that token — or the fetched credentials — into the runner
// container's environment or onto host disk. Results are returned as
// in-memory bytes only.
package metadata

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Default HTTP behavior. These are deliberately conservative: the metadata
// service is GARM's own local-ish endpoint, and CreateInstance is bounded
// upstream by min(exec_timeout, runner_bootstrap_timeout) anyway
// (research.md §1.H), so a fetch that cannot complete quickly should fail
// the create cleanly rather than hang.
const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxRetries     = 3
	defaultRetryBackoff   = 500 * time.Millisecond

	// maxCredentialBytes caps a single credential file, guarding against a
	// misbehaving or hostile metadata endpoint streaming unbounded data
	// into the provider's memory. A JIT credential file is a few KiB; 1
	// MiB is comfortably above that and well below anything alarming.
	maxCredentialBytes = 1 << 20
)

// Client talks to one instance's GARM metadata service. Construct it with
// NewClient; it is safe for the sequential use CreateInstance makes of it.
type Client struct {
	baseURL       string
	instanceToken string
	httpClient    *http.Client

	maxRetries   int
	retryBackoff time.Duration
}

// Option customizes a Client. The defaults suit production; tests use these
// to keep timeout/retry paths fast.
type Option func(*Client)

// WithRequestTimeout overrides the per-request HTTP timeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithRetry overrides the retry count and base backoff for transient
// (network / 5xx) failures.
func WithRetry(maxRetries int, backoff time.Duration) Option {
	return func(c *Client) {
		c.maxRetries = maxRetries
		c.retryBackoff = backoff
	}
}

// NewClient builds a metadata Client for baseURL (BootstrapInstance's
// metadata-url), authenticating every request with instanceToken as a
// Bearer token.
//
// If caCertBundle is non-empty it is added to the HTTP client's root CA
// pool — on top of the system roots, not instead of them — so that GARM
// deployments fronted by a private CA verify correctly (ADR-002 F16). An
// empty bundle uses the system roots alone.
func NewClient(baseURL, instanceToken string, caCertBundle []byte, opts ...Option) (*Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	if len(caCertBundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			// A missing system pool is not fatal: fall back to a pool
			// containing only the supplied bundle, which is exactly the
			// private-CA case this branch exists for.
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(caCertBundle) {
			return nil, fmt.Errorf("failed to parse ca-cert-bundle: no valid certificates found")
		}
		transport.TLSClientConfig = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
	}

	c := &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		instanceToken: instanceToken,
		httpClient:    &http.Client{Timeout: defaultRequestTimeout, Transport: transport},
		maxRetries:    defaultMaxRetries,
		retryBackoff:  defaultRetryBackoff,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// get fetches {baseURL}/{path} with the Bearer instance token, retrying
// only transient failures. The returned bytes are the raw response body.
func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	url := c.baseURL + "/" + path

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.retryBackoff * time.Duration(attempt)):
			}
		}

		body, retryable, err := c.doGet(ctx, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", c.maxRetries+1, lastErr)
}

// doGet performs one HTTP GET. The bool reports whether the failure is
// worth retrying (transport error or 5xx); auth failures (401/403) and
// 404s are not.
func (c *Client) doGet(ctx context.Context, url string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to build request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.instanceToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Transport-level failures (including timeouts) are transient.
		return nil, true, fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// LimitReader at max+1 so a body of exactly the limit still reads
		// fully while anything larger is detected and rejected.
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCredentialBytes+1))
		if readErr != nil {
			return nil, true, fmt.Errorf("failed to read response from %s: %w", url, readErr)
		}
		if len(b) > maxCredentialBytes {
			return nil, false, fmt.Errorf("response from %s exceeds %d bytes", url, maxCredentialBytes)
		}
		return b, false, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("metadata service returned %d (unauthorized) for %s", resp.StatusCode, url)
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, fmt.Errorf("metadata service returned 404 (not found) for %s", url)
	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("metadata service returned %d for %s", resp.StatusCode, url)
	default:
		return nil, false, fmt.Errorf("metadata service returned unexpected status %d for %s", resp.StatusCode, url)
	}
}
