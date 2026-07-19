package provider

import (
	"context"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/version"
)

// newTestProvider builds a Provider backed by a FakeClient for unit tests.
func newTestProvider(t *testing.T) (*Provider, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:  "unix:///var/run/docker.sock",
		RunnerImage: "ghcr.io/example/runner@sha256:deadbeef",
	}
	return New(fake, cfg, "controller-abc"), fake
}

func TestGetVersion(t *testing.T) {
	p, _ := newTestProvider(t)

	version.Version = "v9.9.9-test"
	t.Cleanup(func() { version.Version = "v0.0.0-unknown" })

	got := p.GetVersion(context.Background())
	if got != "v9.9.9-test" {
		t.Errorf("GetVersion() = %q, want %q", got, "v9.9.9-test")
	}
}
