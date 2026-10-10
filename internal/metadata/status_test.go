package metadata

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordedPost is one request a test callback server received.
type recordedPost struct {
	method      string
	path        string
	auth        string
	contentType string
	body        map[string]any
}

// callbackRecorder is a test stand-in for GARM's callback API.
type callbackRecorder struct {
	mu    sync.Mutex
	posts []recordedPost
}

func (c *callbackRecorder) snapshot() []recordedPost {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedPost(nil), c.posts...)
}

// newCallbackServer records every request and answers with respond (default
// 200 with an empty body, as GARM does).
func newCallbackServer(t *testing.T, tls bool, respond func(w http.ResponseWriter)) (*httptest.Server, *callbackRecorder) {
	t.Helper()
	rec := &callbackRecorder{}
	hand := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("callback body is not JSON: %q (%v)", raw, err)
		}
		rec.mu.Lock()
		rec.posts = append(rec.posts, recordedPost{
			method:      r.Method,
			path:        r.URL.Path,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		rec.mu.Unlock()
		if respond != nil {
			respond(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(hand)
	} else {
		srv = httptest.NewServer(hand)
	}
	t.Cleanup(srv.Close)
	return srv, rec
}

// TestPostStatusSendsGarmInstanceUpdateMessage pins the wire contract against
// GARM v0.2.1's callback handler: POST to {callback-url}/status, the instance
// token as a Bearer header, and a JSON body that decodes into
// params.InstanceUpdateMessage with status and message set and NO agent_id
// (a mismatching agent_id would be rejected by GARM's validateAgentID).
func TestPostStatusSendsGarmInstanceUpdateMessage(t *testing.T) {
	srv, rec := newCallbackServer(t, true, nil)

	r, err := NewStatusReporter(srv.URL+"/api/v1/callbacks", testToken, certPEM(t, srv), fastRetry)
	if err != nil {
		t.Fatalf("NewStatusReporter: %v", err)
	}
	if err := r.PostStatus(context.Background(), RunnerStatusInstalling, "credentials delivered"); err != nil {
		t.Fatalf("PostStatus: %v", err)
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("callback server saw %d requests, want 1", len(got))
	}
	req := got[0]
	if req.method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.method)
	}
	if req.path != "/api/v1/callbacks/status" {
		t.Errorf("path = %q, want /api/v1/callbacks/status", req.path)
	}
	if req.auth != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the Bearer instance token", req.auth)
	}
	if req.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentType)
	}
	if req.body["status"] != "installing" || req.body["message"] != "credentials delivered" {
		t.Errorf("body = %v, want status=installing message=credentials delivered", req.body)
	}
	if _, ok := req.body["agent_id"]; ok {
		t.Errorf("body carries agent_id (%v); it must be omitted", req.body["agent_id"])
	}
	if len(req.body) != 2 {
		t.Errorf("body has keys %v, want exactly status and message", req.body)
	}
}

func TestStatusEndpointNormalization(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://garm.example.com/api/v1/callbacks", "https://garm.example.com/api/v1/callbacks/status"},
		{"https://garm.example.com/api/v1/callbacks/", "https://garm.example.com/api/v1/callbacks/status"},
		{"https://garm.example.com/api/v1/callbacks/status", "https://garm.example.com/api/v1/callbacks/status"},
		{"https://garm.example.com/api/v1/callbacks/status/", "https://garm.example.com/api/v1/callbacks/status"},
	}
	for _, tt := range tests {
		if got := statusEndpoint(tt.in); got != tt.want {
			t.Errorf("statusEndpoint(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestNewStatusReporterCallbackURLValidation applies the metadata-url
// transport rule to the callback URL: the token must never travel in clear
// text to a non-loopback host or be paired with embedded userinfo.
func TestNewStatusReporterCallbackURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https accepted", "https://garm.example.com/api/v1/callbacks", false},
		{"http to loopback accepted", "http://127.0.0.1:9997/api/v1/callbacks", false},
		{"http to localhost accepted", "http://localhost:9997/api/v1/callbacks", false},
		{"http to a non-loopback host rejected", "http://garm.example.com/api/v1/callbacks", true},
		{"userinfo rejected", "https://user:pass@garm.example.com/api/v1/callbacks", true},
		{"non-http(s) scheme rejected", "ftp://garm.example.com/api/v1/callbacks", true},
		{"missing host rejected", "https:///api/v1/callbacks", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewStatusReporter(tt.url, testToken, nil)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("NewStatusReporter(%q) returned unexpected error: %v", tt.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewStatusReporter(%q) succeeded, want a validation error", tt.url)
			}
			if !strings.Contains(err.Error(), "callback-url") {
				t.Errorf("error %q does not name the callback-url", err)
			}
		})
	}
}

// TestPostStatusRefusesRedirects proves no redirect is followed — not even a
// same-origin one — so the Bearer token is never re-sent and a POST is never
// silently rewritten into a GET.
func TestPostStatusRefusesRedirects(t *testing.T) {
	for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		var destHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				destHits.Add(1)
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Redirect(w, r, "/elsewhere", code)
		}))
		r, err := NewStatusReporter(srv.URL+"/api/v1/callbacks", testToken, nil, fastRetry)
		if err != nil {
			srv.Close()
			t.Fatalf("NewStatusReporter: %v", err)
		}
		err = r.PostStatus(context.Background(), RunnerStatusIdle, "listening")
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Errorf("redirect %d: PostStatus error = %v, want a refused-redirect error", code, err)
		}
		if n := destHits.Load(); n != 0 {
			t.Errorf("redirect %d: the redirect target was requested %d times, want 0", code, n)
		}
	}
}

func TestPostStatusRetries5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv, _ := newCallbackServer(t, false, func(w http.ResponseWriter) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	r, err := NewStatusReporter(srv.URL, testToken, nil, fastRetry)
	if err != nil {
		t.Fatalf("NewStatusReporter: %v", err)
	}
	if err := r.PostStatus(context.Background(), RunnerStatusIdle, "listening"); err != nil {
		t.Fatalf("PostStatus after transient 503s: %v", err)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("callback hit %d times, want 3 (two 503s then 200)", n)
	}
}

// TestPostStatusClientErrorNotRetried: a 4xx (e.g. GARM's 400 for an invalid
// status transition, or 401 once the token stopped being accepted) is final.
func TestPostStatusClientErrorNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv, _ := newCallbackServer(t, false, func(w http.ResponseWriter) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})
	r, err := NewStatusReporter(srv.URL, testToken, nil, fastRetry)
	if err != nil {
		t.Fatalf("NewStatusReporter: %v", err)
	}
	if err := r.PostStatus(context.Background(), RunnerStatusIdle, "listening"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("PostStatus error = %v, want a 400 error", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("callback hit %d times, want 1", n)
	}
}

// TestPostStatusErrorRedactsToken: a transport failure surfaces the request
// URL inside net/http's error; if that URL carries the token (a hostile or
// misconfigured callback-url), the returned error must not.
func TestPostStatusErrorRedactsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close() // drop the connection: a transport error
		}
	}))
	defer srv.Close()

	r, err := NewStatusReporter(srv.URL+"/"+testToken+"/callbacks", testToken, nil, fastRetry)
	if err != nil {
		t.Fatalf("NewStatusReporter: %v", err)
	}
	err = r.PostStatus(context.Background(), RunnerStatusInstalling, "credentials delivered")
	if err == nil {
		t.Fatal("PostStatus succeeded against a connection-dropping server, want an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("REDACTION FAILURE: PostStatus error contains the instance token: %v", err)
	}
}

func TestPostStatusUntrustedTLSFails(t *testing.T) {
	srv, _ := newCallbackServer(t, true, nil)
	r, err := NewStatusReporter(srv.URL, testToken, nil, WithRetry(0, time.Millisecond))
	if err != nil {
		t.Fatalf("NewStatusReporter: %v", err)
	}
	if err := r.PostStatus(context.Background(), RunnerStatusIdle, "listening"); err == nil {
		t.Fatal("PostStatus to a server whose CA is not trusted succeeded, want a TLS error")
	}
}

func TestPostStatusHonorsContextDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // hang until the client gives up (or the test ends)
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	r, err := NewStatusReporter(srv.URL, testToken, nil, fastRetry)
	if err != nil {
		t.Fatalf("NewStatusReporter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := r.PostStatus(ctx, RunnerStatusIdle, "listening"); err == nil {
		t.Fatal("PostStatus to a hanging server succeeded, want a deadline error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("PostStatus took %v under a 100ms deadline", d)
	}
}
