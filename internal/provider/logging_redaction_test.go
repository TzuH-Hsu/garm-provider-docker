package provider

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateInstanceCredentialPathFailureLogsNoToken is the CRITICAL
// redaction proof (M3-W2): a CreateInstance failure on a credential-bearing
// path — a metadata fetch rejected for a bad instance token, immediately
// followed by a creation-guard rollback failure so at least one slog line is
// actually emitted while the real instance token is in scope on the call
// stack — must never let that token reach a log line. It swaps slog's
// process-wide default logger to a buffer-backed handler for the duration of
// the call (mirroring internal/logging's package doc: main.go installs the
// same process-wide default in production), runs CreateInstance to a
// guaranteed failure, and asserts the captured text contains neither the raw
// instance token this test supplies nor the correct one the metadata server
// expects.
func TestCreateInstanceCredentialPathFailureLogsNoToken(t *testing.T) {
	const (
		correctToken = "correct-super-secret-instance-token"
		wrongToken   = "wrong-super-secret-instance-token"
	)

	// A metadata server that 401s any request not bearing correctToken —
	// exactly the JIT credential-fetch failure shape a bad/expired instance
	// token produces in production.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+correctToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte("should not be reached"))
	}))
	defer srv.Close()

	p, fake := newTestProvider(t)
	// Force the creation-guard rollback itself to fail too, so a slog line
	// (CreateInstance: creation-guard rollback failed) is GUARANTEED to be
	// emitted while wrongToken is still a live value on this call's stack —
	// a vacuous "no log line was emitted at all" pass would not actually
	// prove the redaction property.
	fake.VolumeRemoveErr = errors.New("simulated volume-remove failure to force a rollback-failure log line")

	// Capture every slog record emitted during the call.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	b := jitBootstrap(srv.URL)
	b.InstanceToken = wrongToken // the "credential-bearing" value under test

	if _, err := p.CreateInstance(context.Background(), b); err == nil {
		t.Fatal("expected CreateInstance to fail (bad instance token + forced rollback failure), got nil error")
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("expected at least one log line from the forced rollback failure, got none — test setup did not exercise the intended path")
	}
	t.Logf("captured log output:\n%s", logged)

	if strings.Contains(logged, wrongToken) {
		t.Errorf("REDACTION FAILURE: log output contains the instance token %q:\n%s", wrongToken, logged)
	}
	if strings.Contains(logged, correctToken) {
		t.Errorf("REDACTION FAILURE: log output contains the metadata server's expected token %q:\n%s", correctToken, logged)
	}
}

// TestCreateInstanceHappyPathLogsNoToken is the positive-path companion: a
// SUCCESSFUL CreateInstance also emits slog lines (e.g. the repo-scoped-cache
// resolution line in cache.go), and none of them may carry the instance
// token either.
func TestCreateInstanceHappyPathLogsNoToken(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, _ := newTestProvider(t)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}

	logged := buf.String()
	if strings.Contains(logged, testInstanceToken) {
		t.Errorf("REDACTION FAILURE: happy-path log output contains the instance token:\n%s", logged)
	}
	if strings.Contains(logged, srv.URL) {
		t.Errorf("log output leaks the metadata URL:\n%s", logged)
	}
}
