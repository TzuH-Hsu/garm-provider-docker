package provider

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/version"
)

// inspectRunner inspects the runner container CreateInstance built for
// instanceName, resolved by its (lowercased) Docker name. As of F6 a created
// instance's provider_id is the instance NAME, not the container ID, so tests
// that want the actual runner container inspect it by its derived Docker name
// rather than by provider_id.
func inspectRunner(t *testing.T, fake *docker.FakeClient, instanceName string) types.ContainerJSON {
	t.Helper()
	c, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName(instanceName))
	if err != nil {
		t.Fatalf("inspect runner container for %q: %v", instanceName, err)
	}
	return c
}

// runnerContainerID returns the actual Docker container ID of the runner
// CreateInstance built for instanceName (distinct from its provider_id, which
// is the instance name as of F6).
func runnerContainerID(t *testing.T, fake *docker.FakeClient, instanceName string) string {
	t.Helper()
	return inspectRunner(t, fake, instanceName).ID
}

// newTestProvider builds a Provider backed by a FakeClient for unit tests.
func newTestProvider(t *testing.T) (*Provider, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:  "unix:///var/run/docker.sock",
		RunnerImage: "ghcr.io/example/runner@sha256:deadbeef",
		// none mode (the default). DindMode is set explicitly because a
		// hand-built Config does not go through config.Load's defaulting, and
		// EffectiveDindMode now rejects an empty mode against the ceiling (F10).
		DindMode: config.DindModeNone,
		// A well-formed dind-mode ceiling is now required at provider
		// construction (F10); hand-built test configs set it explicitly.
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		// Match config.Load's [network] defaults (ADR-001, amended
		// 2026-07-21): job network on, internal off (egress allowed).
		// Exercising the real defaults keeps the warning path
		// (enable_job_network=false) out of the common test config.
		Network: config.Network{EnableJobNetwork: true, Internal: false},
	}
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}
	return p, fake
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
