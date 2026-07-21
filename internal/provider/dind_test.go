package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
)

// TestResolveDindModeWithinCeiling confirms resolveDindMode passes through
// the config's dind_mode unchanged when it is within allowed_dind_modes —
// the ordinary case (config.Load's own Validate already guarantees this for
// every Config that went through Load).
func TestResolveDindModeWithinCeiling(t *testing.T) {
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DindMode:         config.DindModePrivilegedSidecar,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
	}
	p := New(fake, cfg, "controller-abc")

	got, err := p.resolveDindMode()
	if err != nil {
		t.Fatalf("resolveDindMode() returned unexpected error: %v", err)
	}
	if got != config.DindModePrivilegedSidecar {
		t.Errorf("resolveDindMode() = %q, want %q", got, config.DindModePrivilegedSidecar)
	}
}

// TestResolveDindModeOutsideCeilingErrors is the provider-level counterpart
// to config.TestConfigEffectiveDindMode: a Config whose dind_mode falls
// outside its own allowed_dind_modes (ADR-001 F7) — the kind of
// misconfiguration Validate would already reject at Load time, but which
// this method re-checks defensively for any Config that reaches the
// provider without going through Load/Validate — must fail closed with a
// clear, non-nil error naming the mode.
func TestResolveDindModeOutsideCeilingErrors(t *testing.T) {
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DindMode:         config.DindModePrivilegedSidecar,
		AllowedDindModes: []string{config.DindModeNone}, // ceiling excludes DindMode
	}
	p := New(fake, cfg, "controller-abc")

	_, err := p.resolveDindMode()
	if err == nil {
		t.Fatal("resolveDindMode() succeeded, want an error (dind_mode outside allowed_dind_modes)")
	}
	if !strings.Contains(err.Error(), config.DindModePrivilegedSidecar) {
		t.Errorf("resolveDindMode() error = %q, want it to name the rejected mode %q", err.Error(), config.DindModePrivilegedSidecar)
	}
}

// TestCreateInstanceFailsClosedWhenDindModeOutsideCeiling drives the ceiling
// violation through the real CreateInstance path (not just resolveDindMode
// in isolation): a constructed Config whose dind_mode falls outside its own
// allowed_dind_modes must be rejected before ANY Docker operation — the
// same "before any Docker op" contract validatePlatform already gets
// (TestCreateInstanceRejectsUnsupportedPlatform, create_test.go) — so a
// misconfigured host never leaves a partial allocation (network/volumes)
// behind, and the metadata service is never even contacted.
func TestCreateInstanceFailsClosedWhenDindModeOutsideCeiling(t *testing.T) {
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModePrivilegedSidecar,
		DindImage:        "docker:dind@sha256:beefdead",
		StorageDriver:    "overlay2",
		AllowedDindModes: []string{config.DindModeNone}, // ceiling excludes DindMode
		Network:          config.Network{EnableJobNetwork: true, Internal: false},
	}
	p := New(fake, cfg, "controller-abc")

	// Metadata URL is deliberately unreachable: the ceiling check must trip
	// before credentials are ever fetched, exactly like platform validation.
	_, err := p.CreateInstance(context.Background(), jitBootstrap("https://metadata.invalid/"))
	if err == nil {
		t.Fatal("expected CreateInstance to fail closed on a dind_mode outside allowed_dind_modes, got nil")
	}
	if !strings.Contains(err.Error(), config.DindModePrivilegedSidecar) {
		t.Errorf("error = %q, want it to name the rejected dind_mode %q", err.Error(), config.DindModePrivilegedSidecar)
	}

	if n := listAll(t, p); n != 0 {
		t.Errorf("ceiling violation left %d containers behind, want 0", n)
	}
	if len(fake.PulledImages) != 0 {
		t.Errorf("ceiling violation must not pull an image, got %v", fake.PulledImages)
	}
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 0 {
		t.Errorf("ceiling violation left %d networks behind, want 0", len(nets))
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("ceiling violation left %d volumes behind, want 0", len(vols.Volumes))
	}
}
