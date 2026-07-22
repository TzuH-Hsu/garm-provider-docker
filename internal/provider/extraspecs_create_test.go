package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
)

// providerWithConfig builds a Provider backed by a FakeClient using cfg, for
// extra_specs create-path tests that need flavors or a tighter ceiling than
// newTestProvider's default config provides.
func providerWithConfig(t *testing.T, cfg config.Config) (*Provider, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}
	return p, fake
}

// assertNoResources asserts a create attempt left zero containers, networks,
// volumes, and pulled images — the "failed closed before any Docker op" contract.
func assertNoResources(t *testing.T, p *Provider, fake *docker.FakeClient) {
	t.Helper()
	if n := listAll(t, p); n != 0 {
		t.Errorf("extra_specs rejection left %d containers behind, want 0", n)
	}
	if len(fake.PulledImages) != 0 {
		t.Errorf("extra_specs rejection must not pull an image, got %v", fake.PulledImages)
	}
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 0 {
		t.Errorf("extra_specs rejection left %d networks behind, want 0", len(nets))
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("extra_specs rejection left %d volumes behind, want 0", len(vols.Volumes))
	}
}

// TestCreateInstanceRejectsReservedExtraEnvBeforeAnyDockerOp: a pool that tries
// to set the reserved RUNNER_EPHEMERAL via extra_specs.extra_env (which would
// break the single-job ephemerality the whole teardown model depends on) is
// rejected FAIL-CLOSED before any Docker operation — the metadata service is
// never even contacted (its URL is unreachable here).
func TestCreateInstanceRejectsReservedExtraEnvBeforeAnyDockerOp(t *testing.T) {
	p, fake := newTestProvider(t)

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"RUNNER_EPHEMERAL": "false"}}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with a reserved extra_env override = nil error, want fail-closed reject")
	}
	if !strings.Contains(err.Error(), "RUNNER_EPHEMERAL") {
		t.Errorf("error %q should name the rejected reserved variable", err.Error())
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceRejectsRunAsRootExtraEnv: RUN_AS_ROOT is hard-reserved (H2)
// — setting it via extra_env would run the runner AND its job as ROOT, defeating
// the non-root isolation M1 established. It is rejected fail-closed before any
// Docker op, even though the operator set no allowlist (hard-reserved always wins).
func TestCreateInstanceRejectsRunAsRootExtraEnv(t *testing.T) {
	p, fake := newTestProvider(t)

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"RUN_AS_ROOT": "true"}}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with RUN_AS_ROOT extra_env = nil error, want fail-closed reject (H2)")
	}
	if !strings.Contains(err.Error(), "RUN_AS_ROOT") {
		t.Errorf("error %q should name the rejected hard-reserved variable", err.Error())
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceRejectsGarmTimeoutExtraEnv: GARM_CRED_WAIT_SECONDS is
// hard-reserved (GARM_* prefix, H2). The runner-image entrypoint reads it into
// Bash arithmetic, where a value like "a[$(cmd)]" is a command-substitution RCE
// as root — so it must never be settable from a pool. Rejected before any Docker op.
func TestCreateInstanceRejectsGarmTimeoutExtraEnv(t *testing.T) {
	p, fake := newTestProvider(t)

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"GARM_CRED_WAIT_SECONDS": "a[$(touch /tmp/pwned)]"}}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with GARM_CRED_WAIT_SECONDS extra_env = nil error, want fail-closed reject (H2)")
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceRejectsNonAllowlistedEnvByDefault: with no operator
// allowed_env (the default), a pool cannot inject ANY extra env — even a benign
// name is rejected fail-closed before any Docker op (H2).
func TestCreateInstanceRejectsNonAllowlistedEnvByDefault(t *testing.T) {
	p, fake := newTestProvider(t) // no ExtraSpecs.AllowedEnv => empty => fail-closed

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"MY_BENIGN_VAR": "x"}}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with a non-allowlisted extra_env = nil error, want fail-closed reject (H2)")
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceRejectsEqualsInEnvKey: an env key containing '=' (H3) is
// rejected before any Docker op — it must not smuggle a reserved variable (e.g.
// "JIT_CONFIG_ENABLED=false") past the exact-name reserved/allowlist checks.
func TestCreateInstanceRejectsEqualsInEnvKey(t *testing.T) {
	p, fake := newTestProvider(t)

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"JIT_CONFIG_ENABLED=false": "x"}}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with an '='-in-key extra_env = nil error, want charset reject (H3)")
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceRejectsRawImageInExtraSpecs: a raw image reference has no
// field in the schema (additionalProperties:false), so a pool that tries to
// smuggle one via extra_specs is rejected before any Docker op — image
// selection is named-flavor-only (ADR-002).
func TestCreateInstanceRejectsRawImageInExtraSpecs(t *testing.T) {
	p, fake := newTestProvider(t)

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"image": "attacker/evil@sha256:` + strings.Repeat("a", 64) + `"}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with a raw image in extra_specs = nil error, want reject")
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceRejectsDindModeOutsideCeilingViaExtraSpecs: the ceiling is
// enforced on the pool's REQUESTED mode (threaded through EffectiveDindMode) —
// a host whose allowed_dind_modes is ["none"] rejects a pool's
// extra_specs.dind_mode=privileged-sidecar before any Docker op, no matter what
// the admin set at the pool level (ADR-001 F7 / ADR-005).
func TestCreateInstanceRejectsDindModeOutsideCeilingViaExtraSpecs(t *testing.T) {
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		DindImage:        "docker:dind@sha256:beefdead",
		StorageDriver:    "overlay2",
		AllowedDindModes: []string{config.DindModeNone}, // ceiling forbids privileged
		Network:          config.Network{EnableJobNetwork: true},
	}
	p, fake := providerWithConfig(t, cfg)

	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = json.RawMessage(`{"dind_mode": "privileged-sidecar"}`)

	_, err := p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("CreateInstance with an out-of-ceiling extra_specs.dind_mode = nil error, want reject")
	}
	if !strings.Contains(err.Error(), config.DindModePrivilegedSidecar) {
		t.Errorf("error %q should name the rejected mode", err.Error())
	}
	assertNoResources(t, p, fake)
}

// TestCreateInstanceFlavorSelectsImage: a pool selecting a named flavor via
// extra_specs.flavor gets THAT flavor's runner image pulled/run — the only
// image-varying channel (ADR-002).
func TestCreateInstanceFlavorSelectsImage(t *testing.T) {
	const flavorImage = "ghcr.io/example/runner-big@sha256:beefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeefbeef"
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		StorageDriver:    "overlay2",
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		Network:          config.Network{EnableJobNetwork: true},
		Flavors: map[string]config.Flavor{
			"big": {RunnerImage: flavorImage, RunnerMemory: "16GiB"},
		},
	}
	p, fake := providerWithConfig(t, cfg)

	srv := newJITMetadataServer(t)
	defer srv.Close()

	b := jitBootstrap(srv.URL)
	b.ExtraSpecs = json.RawMessage(`{"flavor": "big"}`)

	inst, err := p.CreateInstance(context.Background(), b)
	if err != nil {
		t.Fatalf("CreateInstance with flavor selection: %v", err)
	}

	// The flavor's image was pulled, not the top-level default.
	if len(fake.PulledImages) != 1 || fake.PulledImages[0] != flavorImage {
		t.Errorf("PulledImages = %v, want the flavor image %q", fake.PulledImages, flavorImage)
	}
	// The runner container runs the flavor's image.
	got := inspectRunner(t, fake, inst.Name)
	if got.Config.Image != flavorImage {
		t.Errorf("runner Image = %q, want the flavor image %q", got.Config.Image, flavorImage)
	}
}

// TestCreateInstanceExtraEnvMergedProviderWins: an operator-ALLOWLISTED extra_env
// value reaches the runner container; a non-reserved but provider-injected name
// (DISABLE_RUNNER_UPDATE) that collides is DROPPED in favor of the provider's
// value — provider-injected environment always wins the merge (ADR-005 H2).
func TestCreateInstanceExtraEnvMergedProviderWins(t *testing.T) {
	// H2: both names must be on the operator's allowed_env allowlist to pass the
	// fail-closed extra_env gate; neither is hard-reserved.
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		Network:          config.Network{EnableJobNetwork: true, Internal: false},
		ExtraSpecs:       config.ExtraSpecsPolicy{AllowedEnv: []string{"MY_CI_FLAG", "DISABLE_RUNNER_UPDATE"}},
	}
	p, fake := providerWithConfig(t, cfg)

	srv := newJITMetadataServer(t)
	defer srv.Close()

	b := jitBootstrap(srv.URL)
	// MY_CI_FLAG is allowlisted and provider-injects nothing under that name, so
	// it merges; DISABLE_RUNNER_UPDATE is allowlisted too, but the provider injects
	// it as =true, so the pool's =false must be dropped (provider wins).
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"MY_CI_FLAG": "on", "DISABLE_RUNNER_UPDATE": "false"}}`)

	inst, err := p.CreateInstance(context.Background(), b)
	if err != nil {
		t.Fatalf("CreateInstance with extra_env: %v", err)
	}

	env := inspectRunner(t, fake, inst.Name).Config.Env
	if !hasEnv(env, "MY_CI_FLAG=on") {
		t.Errorf("runner env missing allowlisted MY_CI_FLAG=on: %v", env)
	}
	if !hasEnv(env, "DISABLE_RUNNER_UPDATE=true") {
		t.Errorf("provider-injected DISABLE_RUNNER_UPDATE=true should win: %v", env)
	}
	if hasEnv(env, "DISABLE_RUNNER_UPDATE=false") {
		t.Errorf("pool's DISABLE_RUNNER_UPDATE=false must not win over the provider's value: %v", env)
	}
}

// TestCreateInstanceExtraRunnerLabelsAppendedNonJIT: extra_specs.runner_labels
// are appended to RUNNER_LABELS in non-JIT mode.
func TestCreateInstanceExtraRunnerLabelsAppendedNonJIT(t *testing.T) {
	p, fake := newTestProvider(t)

	srv := newRegistrationTokenServer(t)
	defer srv.Close()

	b := jitBootstrap(srv.URL)
	b.JitConfigEnabled = false
	b.Labels = []string{"self-hosted"}
	b.ExtraSpecs = json.RawMessage(`{"runner_labels": ["gpu", "cuda"]}`)

	inst, err := p.CreateInstance(context.Background(), b)
	if err != nil {
		t.Fatalf("CreateInstance non-JIT with runner_labels: %v", err)
	}

	env := inspectRunner(t, fake, inst.Name).Config.Env
	var labelsVal string
	for _, e := range env {
		if strings.HasPrefix(e, "RUNNER_LABELS=") {
			labelsVal = strings.TrimPrefix(e, "RUNNER_LABELS=")
		}
	}
	for _, want := range []string{"self-hosted", "gpu", "cuda"} {
		if !strings.Contains(labelsVal, want) {
			t.Errorf("RUNNER_LABELS %q should contain %q (GARM labels + extra_specs.runner_labels)", labelsVal, want)
		}
	}
}
