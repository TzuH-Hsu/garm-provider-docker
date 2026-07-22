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

	// maxRedirects caps how many redirect hops the metadata client follows
	// before giving up. GARM's metadata service has no legitimate reason to
	// redirect at all, so this is deliberately small; it exists only so a
	// redirect loop cannot spin.
	maxRedirects = 3
)

// Client talks to one instance's GARM metadata service. Construct it with
// NewClient; it is safe for the sequential use CreateInstance makes of it.
type Client struct {
	baseURL       string
	instanceToken string
	httpClient    *http.Client

	// origin is the scheme+host+port of baseURL, used by checkRedirect to
	// reject any redirect that would leave the metadata service's own origin.
	origin string

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
	// The instance token is sent as a Bearer header on every request, so the
	// transport must not be cleartext: require https, permitting plain http
	// only to a loopback host (which keeps httptest-based tests working and
	// covers a co-located metadata service), and reject any userinfo in the
	// URL (ADR-002 F5).
	if err := validateMetadataURL(baseURL); err != nil {
		return nil, err
	}

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

	// baseURL already passed validateMetadataURL, so it re-parses cleanly;
	// capture its origin so checkRedirect can reject any cross-origin hop.
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid metadata-url %q: %w", baseURL, err)
	}

	c := &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		instanceToken: instanceToken,
		httpClient:    &http.Client{Timeout: defaultRequestTimeout, Transport: transport},
		origin:        originKey(base),
		maxRetries:    defaultMaxRetries,
		retryBackoff:  defaultRetryBackoff,
	}
	// Guard every redirect hop: the Bearer instance token must never be
	// forwarded to a downgraded (http) or cross-origin target. Go's default
	// CheckRedirect would forward the Authorization header to a same-host
	// redirect regardless of scheme downgrade, so this is not optional
	// (ADR-002 F5).
	c.httpClient.CheckRedirect = c.checkRedirect

	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// redact returns err with the client's bearer instance token scrubbed from its
// message (ADR-002 F5, H4). A hostile or misbehaving metadata endpoint can
// reflect the Bearer token into a redirect Location, and net/http surfaces that
// redirect URL inside the transport/redirect error it returns; embedding such an
// error verbatim in a wrapped error — which main.go then logs to stderr — would
// leak the token into logs. EVERY error this Client hands back to a caller is
// passed through redact first, so no metadata error can carry the raw token
// regardless of what the endpoint echoes. It flattens the (possibly token-
// bearing) wrapped error into a token-free message; the %w chain is not needed
// for control flow, because retryability is signaled out of band by doGet's
// bool, not by error identity.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(logging.Redact(err.Error(), c.instanceToken))
}

// checkRedirect is the http.Client redirect policy: it re-applies the
// initial-URL transport-security invariant (https required; plain http only
// for a loopback host; no userinfo) on every hop AND rejects any redirect
// that leaves the original metadata origin (scheme+host+port). Cross-origin
// redirects are refused outright: GARM's metadata service has no legitimate
// cross-origin redirect, and a token-bearing client must never follow one.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects fetching metadata", maxRedirects)
	}
	if err := validateMetadataURL(req.URL.String()); err != nil {
		// validateMetadataURL embeds the raw redirect URL, which a hostile
		// endpoint may have stuffed the bearer token into — redact it before it
		// becomes part of any error net/http surfaces to us (H4).
		return c.redact(fmt.Errorf("refusing metadata redirect: %w", err))
	}
	if got := originKey(req.URL); got != c.origin {
		return fmt.Errorf("refusing cross-origin metadata redirect from %s to %s", c.origin, got)
	}
	return nil
}

// originKey returns a scheme+host+port key for u with default ports resolved,
// so two URLs compare as the same origin iff their scheme, host, and
// effective port all match. A scheme downgrade (https→http) therefore yields
// a different key, which is exactly what the cross-origin redirect check
// relies on. The hostname is lowercased before comparison: DNS hostnames are
// case-insensitive, so a redirect target that differs from the original only
// in host letter-casing is still the same origin — without this, checkRedirect
// would fail closed and reject a legitimate same-origin redirect.
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

// validateMetadataURL enforces the transport-security invariant on the
// metadata-url (ADR-002 F5): https is required to protect the Bearer instance
// token, with a loopback-only exception for plain http, and no userinfo
// credentials embedded in the URL.
func validateMetadataURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid metadata-url %q: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("metadata-url %q has no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("metadata-url %q must not embed userinfo credentials", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("metadata-url %q uses cleartext http to a non-loopback host; https is required to protect the instance token", raw)
	default:
		return fmt.Errorf("metadata-url %q must use https (or http to a loopback host), got scheme %q", raw, u.Scheme)
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
			// H4: doGet's error can wrap a net/http redirect error carrying a
			// token-bearing redirect URL — scrub the token before returning.
			return nil, c.redact(err)
		}
	}
	// H4: lastErr (a transport/redirect failure) can likewise carry a
	// token-bearing redirect URL — scrub before returning.
	return nil, c.redact(fmt.Errorf("giving up after %d attempts: %w", c.maxRetries+1, lastErr))
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
