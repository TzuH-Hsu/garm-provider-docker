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

// TestNewRejectsEmptyOrMalformedCeiling is the F10 construction guard: a
// hand-built or Load-bypassing Config whose allowed_dind_modes is empty or
// malformed is rejected at provider construction, so it can never silently
// permit every mode. A well-formed non-empty ceiling constructs fine, even
// without dind_mode being a member (that is deferred to the create path).
func TestNewRejectsEmptyOrMalformedCeiling(t *testing.T) {
	fake := docker.NewFakeClient()

	if _, err := New(fake, config.Config{AllowedDindModes: nil}, "controller-abc"); err == nil {
		t.Error("New with an empty allowed_dind_modes must fail closed (F10), got nil")
	}
	if _, err := New(fake, config.Config{AllowedDindModes: []string{"bogus"}}, "controller-abc"); err == nil {
		t.Error("New with an invalid allowed_dind_modes entry must fail, got nil")
	}
	if _, err := New(fake, config.Config{AllowedDindModes: []string{config.DindModeNone}}, "controller-abc"); err != nil {
		t.Errorf("New with a well-formed ceiling returned unexpected error: %v", err)
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
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}

	// Metadata URL is deliberately unreachable: the ceiling check must trip
	// before credentials are ever fetched, exactly like platform validation.
	_, err = p.CreateInstance(context.Background(), jitBootstrap("https://metadata.invalid/"))
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
