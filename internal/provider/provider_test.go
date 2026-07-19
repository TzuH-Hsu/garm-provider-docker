package provider

import (
	"context"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/version"
)

func TestGetVersion(t *testing.T) {
	p := New()

	version.Version = "v9.9.9-test"
	t.Cleanup(func() { version.Version = "v0.0.0-unknown" })

	got := p.GetVersion(context.Background())
	if got != "v9.9.9-test" {
		t.Errorf("GetVersion() = %q, want %q", got, "v9.9.9-test")
	}
}

// TestStubMethodsReturnNotImplemented exercises every lifecycle method that
// M0 WP1 has not yet implemented (WP5-WP8 fill these in). It exists so the
// interface conformance of Provider is covered by go test, not just by the
// compile-time assertion in provider.go.
func TestStubMethodsReturnNotImplemented(t *testing.T) {
	p := New()
	ctx := context.Background()

	t.Run("CreateInstance", func(t *testing.T) {
		if _, err := p.CreateInstance(ctx, params.BootstrapInstance{}); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
	t.Run("DeleteInstance", func(t *testing.T) {
		if err := p.DeleteInstance(ctx, "instance"); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
	t.Run("GetInstance", func(t *testing.T) {
		if _, err := p.GetInstance(ctx, "instance"); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
	t.Run("ListInstances", func(t *testing.T) {
		if _, err := p.ListInstances(ctx, "pool"); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
	t.Run("RemoveAllInstances", func(t *testing.T) {
		if err := p.RemoveAllInstances(ctx); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
	t.Run("Start", func(t *testing.T) {
		if err := p.Start(ctx, "instance"); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
	t.Run("Stop", func(t *testing.T) {
		if err := p.Stop(ctx, "instance", true); err == nil {
			t.Error("expected a not-implemented error, got nil")
		}
	})
}
