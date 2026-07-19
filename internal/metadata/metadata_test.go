package metadata

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testToken = "test-instance-token"

// fastRetry keeps the retry path from sleeping in tests that don't care
// about retry timing.
var fastRetry = WithRetry(2, time.Millisecond)

// newTestClient builds a Client pointed at srv, trusting srv's TLS cert via
// the CA-bundle path when srv is a TLS server.
func newTestClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	var caBundle []byte
	if srv.TLS != nil {
		caBundle = certPEM(t, srv)
	}
	c, err := NewClient(srv.URL, testToken, caBundle, opts...)
	if err != nil {
		t.Fatalf("NewClient returned unexpected error: %v", err)
	}
	return c
}

// certPEM PEM-encodes the httptest server's self-signed leaf certificate so
// it can be handed to NewClient as a ca-cert-bundle.
func certPEM(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("server has no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

// readTar decodes a tar archive into a name→contents map for assertions.
func readTar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		var body bytes.Buffer
		if _, err := io.Copy(&body, tr); err != nil {
			t.Fatalf("reading tar body: %v", err)
		}
		out[hdr.Name] = body.String()
	}
	return out
}

// jitHandler serves the three JIT credential files and asserts the Bearer
// token on every request.
func jitHandler(t *testing.T, seen map[string]bool) http.HandlerFunc {
	t.Helper()
	bodies := map[string]string{
		"/credentials/runner":                "runner-file-contents",
		"/credentials/credentials":           "credentials-file-contents",
		"/credentials/credentials_rsaparams": "rsaparams-file-contents",
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q, want %q", got, "Bearer "+testToken)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		seen[r.URL.Path] = true
		_, _ = w.Write([]byte(body))
	}
}

func TestFetchJITCredentialsHappyPath(t *testing.T) {
	seen := map[string]bool{}
	srv := httptest.NewServer(jitHandler(t, seen))
	defer srv.Close()

	c := newTestClient(t, srv, fastRetry)
	files, err := c.FetchJITCredentials(context.Background())
	if err != nil {
		t.Fatalf("FetchJITCredentials returned unexpected error: %v", err)
	}

	want := []CredentialFileContent{
		{Name: "runner", Bytes: []byte("runner-file-contents")},
		{Name: "credentials", Bytes: []byte("credentials-file-contents")},
		{Name: "credentials_rsaparams", Bytes: []byte("rsaparams-file-contents")},
	}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d", len(files), len(want))
	}
	for i, w := range want {
		if files[i].Name != w.Name {
			t.Errorf("files[%d].Name = %q, want %q", i, files[i].Name, w.Name)
		}
		if !bytes.Equal(files[i].Bytes, w.Bytes) {
			t.Errorf("files[%d].Bytes = %q, want %q", i, files[i].Bytes, w.Bytes)
		}
	}
	// All three remote paths must have been hit (path-segment mapping).
	for _, p := range []string{"/credentials/runner", "/credentials/credentials", "/credentials/credentials_rsaparams"} {
		if !seen[p] {
			t.Errorf("expected the server to receive a request for %q", p)
		}
	}
}

func TestFetchRegistrationTokenHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q, want Bearer token", got)
		}
		if r.URL.Path != "/runner-registration-token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("AAABBBCCC-registration-token\n"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, fastRetry)
	tok, err := c.FetchRegistrationToken(context.Background())
	if err != nil {
		t.Fatalf("FetchRegistrationToken returned unexpected error: %v", err)
	}
	if tok.Name != RegistrationTokenFile {
		t.Errorf("Name = %q, want %q", tok.Name, RegistrationTokenFile)
	}
	// Trailing newline must be trimmed.
	if string(tok.Bytes) != "AAABBBCCC-registration-token" {
		t.Errorf("Bytes = %q, want trimmed token", tok.Bytes)
	}
}

func TestFetchJITCredentialsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, fastRetry)
	if _, err := c.FetchJITCredentials(context.Background()); err == nil {
		t.Fatal("expected an error on 401, got nil")
	}
}

func TestFetchJITCredentialsNotFound(t *testing.T) {
	// Server authorizes but has no such file → 404. 404 is not retryable.
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(t, srv, fastRetry)
	if _, err := c.FetchJITCredentials(context.Background()); err == nil {
		t.Fatal("expected an error on 404, got nil")
	}
	if attempts != 1 {
		t.Errorf("404 should not be retried: got %d attempts, want 1", attempts)
	}
}

func TestGet5xxIsRetriedThenSucceeds(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("runner-file-contents"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv, WithRetry(3, time.Millisecond))
	b, err := c.get(context.Background(), "credentials/runner")
	if err != nil {
		t.Fatalf("get returned unexpected error: %v", err)
	}
	if string(b) != "runner-file-contents" {
		t.Errorf("body = %q, want runner-file-contents", b)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3 (two 503s then success)", attempts)
	}
}

func TestGetTimeout(t *testing.T) {
	// Server sleeps past the client's request timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("too-slow"))
	}))
	defer srv.Close()

	// No retries + a short timeout keeps this test fast while still
	// exercising the transport-timeout error path.
	c := newTestClient(t, srv, WithRetry(0, time.Millisecond), WithRequestTimeout(20*time.Millisecond))
	if _, err := c.get(context.Background(), "credentials/runner"); err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
}

func TestTLSTrustsCABundleAndRejectsWithout(t *testing.T) {
	srv := httptest.NewTLSServer(jitHandler(t, map[string]bool{}))
	defer srv.Close()

	// With the CA bundle: the self-signed server cert is trusted.
	trusting := newTestClient(t, srv, fastRetry)
	if _, err := trusting.FetchJITCredentials(context.Background()); err != nil {
		t.Fatalf("with ca-cert-bundle: unexpected error: %v", err)
	}

	// Without the CA bundle: the system roots don't include the cert, so
	// TLS verification must fail.
	untrusting, err := NewClient(srv.URL, testToken, nil, WithRetry(0, time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient returned unexpected error: %v", err)
	}
	if _, err := untrusting.FetchJITCredentials(context.Background()); err == nil {
		t.Fatal("without ca-cert-bundle: expected a TLS verification error, got nil")
	}
}

func TestNewClientRejectsBadCABundle(t *testing.T) {
	if _, err := NewClient("https://example.test", testToken, []byte("not a pem cert")); err == nil {
		t.Fatal("expected an error for an unparseable ca-cert-bundle, got nil")
	}
}

func TestNewClientCABundleTrustsAdditionalCA(t *testing.T) {
	// A well-formed CA PEM must parse (AppendCertsFromPEM succeeds),
	// proving the bundle is added rather than rejected.
	srv := httptest.NewTLSServer(jitHandler(t, map[string]bool{}))
	defer srv.Close()
	block := &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}
	if _, err := NewClient("https://example.test", testToken, pem.EncodeToMemory(block)); err != nil {
		t.Fatalf("NewClient with a valid CA bundle returned error: %v", err)
	}
	// Sanity: the cert really is a parseable x509 certificate.
	if _, err := x509.ParseCertificate(srv.Certificate().Raw); err != nil {
		t.Fatalf("server certificate did not parse: %v", err)
	}
}

func TestTarArchiveRoundTrips(t *testing.T) {
	files := []CredentialFileContent{
		{Name: "runner", Bytes: []byte("aaa")},
		{Name: "credentials", Bytes: []byte("bbb")},
	}
	buf, err := TarArchive(files)
	if err != nil {
		t.Fatalf("TarArchive returned unexpected error: %v", err)
	}
	got := readTar(t, buf.Bytes())
	if len(got) != 2 {
		t.Fatalf("archive has %d entries, want 2", len(got))
	}
	if got["runner"] != "aaa" || got["credentials"] != "bbb" {
		t.Errorf("archive contents = %v, want runner=aaa credentials=bbb", got)
	}
}
