// Package metadata fetches a runner's credentials from GARM's per-instance
// metadata service (ADR-002). The provider performs this fetch itself, so neither
// the instance token nor the fetched credentials reach the runner container's
// environment or host disk. Results are returned as in-memory bytes only.
package metadata

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
)

// GARM bounds CreateInstance by min(exec_timeout, runner_bootstrap_timeout), so a
// fetch that cannot complete quickly should fail the create rather than hang.
const (
	defaultRequestTimeout = 30 * time.Second
	defaultMaxRetries     = 3
	defaultRetryBackoff   = 500 * time.Millisecond

	// maxCredentialBytes stops an endpoint streaming unbounded data into memory.
	// A JIT credential file is a few KiB.
	maxCredentialBytes = 1 << 20

	// maxRedirects only stops a redirect loop. GARM's metadata service has no
	// reason to redirect.
	maxRedirects = 3
)

// Client talks to one instance's GARM metadata service.
type Client struct {
	baseURL       string
	instanceToken string
	httpClient    *http.Client

	// origin is baseURL's originKey. checkRedirect refuses a hop that leaves it.
	origin string

	maxRetries   int
	retryBackoff time.Duration
}

// Option customizes a Client.
type Option func(*Client)

// WithRequestTimeout overrides the per-request HTTP timeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithRetry overrides the retry count and base backoff.
func WithRetry(maxRetries int, backoff time.Duration) Option {
	return func(c *Client) {
		c.maxRetries = maxRetries
		c.retryBackoff = backoff
	}
}

// NewClient returns a Client for baseURL that sends instanceToken as a Bearer
// token. A non-empty caCertBundle is added to the system roots, not used instead
// of them.
func NewClient(baseURL, instanceToken string, caCertBundle []byte, opts ...Option) (*Client, error) {
	return newClient(metadataURLField, baseURL, instanceToken, caCertBundle, opts...)
}

// metadataURLField and callbackURLField name the token-bearing URLs in errors.
const (
	metadataURLField = "metadata-url"
	callbackURLField = "callback-url"
)

// newClient is the constructor behind NewClient and NewStatusReporter. Both
// endpoints take the same token, so both get the same URL validation and CA bundle.
func newClient(field, baseURL, instanceToken string, caCertBundle []byte, opts ...Option) (*Client, error) {
	if err := validateTokenURL(field, baseURL); err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()

	if len(caCertBundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			// The bundle alone still covers the private-CA case.
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

	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", field, baseURL, err)
	}

	c := &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		instanceToken: instanceToken,
		httpClient:    &http.Client{Timeout: defaultRequestTimeout, Transport: transport},
		origin:        originKey(base),
		maxRetries:    defaultMaxRetries,
		retryBackoff:  defaultRetryBackoff,
	}
	// The default policy forwards the Authorization header to a same-host
	// redirect even on a scheme downgrade.
	c.httpClient.CheckRedirect = c.checkRedirect

	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// redact returns err with the instance token scrubbed from its message. An endpoint
// can reflect the token into a redirect Location, net/http puts that URL in its
// error, and main.go logs the error, so every request error returned here must pass
// through redact. It flattens the error chain: callers branch on doGet's bool only.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(logging.Redact(err.Error(), c.instanceToken))
}

// checkRedirect applies validateTokenURL's rule to every hop and refuses a hop that
// leaves the original origin, so the token never reaches a downgraded or foreign
// target.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects fetching metadata", maxRedirects)
	}
	if err := validateMetadataURL(req.URL.String()); err != nil {
		// The error embeds the redirect URL, which may hold the token.
		return c.redact(fmt.Errorf("refusing metadata redirect: %w", err))
	}
	if got := originKey(req.URL); got != c.origin {
		return fmt.Errorf("refusing cross-origin metadata redirect from %s to %s", c.origin, got)
	}
	return nil
}

// originKey returns a scheme+host+port key for u with default ports resolved and
// the host lowercased, so a scheme downgrade changes the key and host case does not.
func originKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// validateMetadataURL is validateTokenURL for the metadata-url.
func validateMetadataURL(raw string) error {
	return validateTokenURL(metadataURLField, raw)
}

// validateTokenURL checks a URL the Bearer token is sent to: https, or plain http to
// a loopback host only, and no userinfo. field names the URL in errors.
func validateTokenURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", field, raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("%s %q has no host", field, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%s %q must not embed userinfo credentials", field, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s %q uses cleartext http to a non-loopback host; https is required to protect the instance token", field, raw)
	default:
		return fmt.Errorf("%s %q must use https (or http to a loopback host), got scheme %q", field, raw, u.Scheme)
	}
}

// isLoopbackHost reports whether host is a loopback address or "localhost".
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// get returns the body of {baseURL}/{path}, retrying only transient failures.
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
			return nil, c.redact(err)
		}
	}
	return nil, c.redact(fmt.Errorf("giving up after %d attempts: %w", c.maxRetries+1, lastErr))
}

// doGet performs one GET. The bool reports whether the failure is worth retrying
// (transport error or 5xx).
func (c *Client) doGet(ctx context.Context, url string) (body []byte, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("failed to build request for %s: %w", url, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.instanceToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		// Reading one byte past the limit detects a larger body.
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
