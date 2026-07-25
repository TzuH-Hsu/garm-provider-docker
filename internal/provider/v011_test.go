package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	executionv011 "github.com/cloudbase/garm-provider-common/execution/v0.1.1"
	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
)

func TestGetSupportedInterfaceVersions(t *testing.T) {
	p, _ := newTestProvider(t)
	got := p.GetSupportedInterfaceVersions(context.Background())
	if !slices.Equal(got, []string{execcommon.Version010, execcommon.Version011}) {
		t.Errorf("GetSupportedInterfaceVersions() = %v, want [v0.1.0 v0.1.1]", got)
	}
}

func TestGetExtraSpecsJSONSchemaIsParseable(t *testing.T) {
	p, _ := newTestProvider(t)
	schema, err := p.GetExtraSpecsJSONSchema(context.Background())
	if err != nil {
		t.Fatalf("GetExtraSpecsJSONSchema: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(schema), &m); err != nil {
		t.Fatalf("GetExtraSpecsJSONSchema returned unparseable JSON: %v", err)
	}
	if m["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("extra_specs schema is not draft-07: %v", m["$schema"])
	}
	if _, ok := m["properties"].(map[string]any)["dind_mode"]; !ok {
		t.Errorf("extra_specs schema missing dind_mode property")
	}
}

func TestGetConfigJSONSchemaIsParseable(t *testing.T) {
	p, _ := newTestProvider(t)
	schema, err := p.GetConfigJSONSchema(context.Background())
	if err != nil {
		t.Fatalf("GetConfigJSONSchema: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(schema), &m); err != nil {
		t.Fatalf("GetConfigJSONSchema returned unparseable JSON: %v", err)
	}
	if m["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("config schema is not draft-07: %v", m["$schema"])
	}
	// The schema GARM receives over this command must carry the loader's real
	// constraints, not just its key names — otherwise a caller validating a
	// config against the provider's own published schema gets a false PASS on
	// a config the provider would refuse to load. The full schema-vs-loader
	// agreement is pinned in internal/config's
	// TestJSONSchemaMatchesLoaderOnRequiredKeys; this asserts the wiring
	// delivers that same schema rather than a stripped one.
	req, ok := m["required"].([]any)
	if !ok || len(req) == 0 || req[0] != "runner_image" {
		t.Errorf("config schema from GetConfigJSONSchema has required=%v, want [runner_image]", m["required"])
	}
	if _, ok := m["if"]; !ok {
		t.Error("config schema from GetConfigJSONSchema lost its dind_image conditional")
	}
}

func TestValidatePoolInfoAcceptsGood(t *testing.T) {
	// H2: extra_env is fail-closed, so a "good" payload's env NAME must be on the
	// operator's allowed_env allowlist. FOO is benign and opted in here.
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		StorageDriver:    "overlay2",
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		Network:          config.Network{EnableJobNetwork: true},
		ExtraSpecs:       config.ExtraSpecsPolicy{AllowedEnv: []string{"FOO"}},
	}
	p, _ := providerWithConfig(t, cfg)
	err := p.ValidatePoolInfo(context.Background(), "", "", "", `{"dind_mode": "privileged-sidecar", "extra_env": {"FOO": "bar"}}`)
	if err != nil {
		t.Errorf("ValidatePoolInfo of a good extra_specs = %v, want nil", err)
	}
}

func TestValidatePoolInfoAcceptsEmpty(t *testing.T) {
	p, _ := newTestProvider(t)
	if err := p.ValidatePoolInfo(context.Background(), "", "", "", ""); err != nil {
		t.Errorf("ValidatePoolInfo of empty extra_specs = %v, want nil", err)
	}
}

func TestValidatePoolInfoRejectsCeilingViolation(t *testing.T) {
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		StorageDriver:    "overlay2",
		AllowedDindModes: []string{config.DindModeNone}, // ceiling forbids privileged
		Network:          config.Network{EnableJobNetwork: true},
	}
	p, _ := providerWithConfig(t, cfg)
	err := p.ValidatePoolInfo(context.Background(), "", "", "", `{"dind_mode": "privileged-sidecar"}`)
	if err == nil {
		t.Fatal("ValidatePoolInfo of a ceiling-violating extra_specs = nil, want error")
	}
	if !strings.Contains(err.Error(), config.DindModePrivilegedSidecar) {
		t.Errorf("error %q should name the rejected mode", err.Error())
	}
}

func TestValidatePoolInfoRejectsReservedOverride(t *testing.T) {
	p, _ := newTestProvider(t)
	err := p.ValidatePoolInfo(context.Background(), "", "", "", `{"extra_env": {"DOCKER_HOST": "tcp://evil:2375"}}`)
	if err == nil {
		t.Fatal("ValidatePoolInfo of a reserved extra_env override = nil, want error")
	}
	if !strings.Contains(err.Error(), "DOCKER_HOST") {
		t.Errorf("error %q should name the rejected reserved variable", err.Error())
	}
}

// TestValidatePoolInfoWithConfigPath exercises the reload-from-path branch: the
// providerConfig arg names a real config file whose ceiling forbids privileged,
// so a pool requesting it is rejected against THAT file (not p.cfg).
func TestValidatePoolInfoWithConfigPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`runner_image = "ghcr.io/example/runner@sha256:`+strings.Repeat("d", 64)+`"
dind_mode = "none"
allowed_dind_modes = ["none"]
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	p, _ := newTestProvider(t) // p.cfg would allow privileged, but the file does not

	if err := p.ValidatePoolInfo(context.Background(), "", "", cfgPath, `{"dind_mode": "none"}`); err != nil {
		t.Errorf("good extra_specs against the config file = %v, want nil", err)
	}
	if err := p.ValidatePoolInfo(context.Background(), "", "", cfgPath, `{"dind_mode": "privileged-sidecar"}`); err == nil {
		t.Error("privileged-sidecar against a none-only config file = nil, want reject")
	}
}

func TestValidatePoolInfoAcceptsBase64(t *testing.T) {
	p, _ := newTestProvider(t)
	b64 := base64.StdEncoding.EncodeToString([]byte(`{"dind_mode": "none"}`))
	if err := p.ValidatePoolInfo(context.Background(), "", "", "", b64); err != nil {
		t.Errorf("ValidatePoolInfo of base64 extra_specs = %v, want nil", err)
	}
}

// TestV011RunDispatch drives the four v0.1.1 commands through
// garm-provider-common's own EnvironmentV011.Run, proving *Provider satisfies
// the versioned interface at RUNTIME (the same dispatch the binary uses under
// GARM_INTERFACE_VERSION=v0.1.1) and that each command returns the expected shape.
func TestV011RunDispatch(t *testing.T) {
	p, _ := newTestProvider(t)
	ctx := context.Background()

	// GetSupportedInterfaceVersions -> JSON array of versions.
	out, err := (executionv011.EnvironmentV011{Command: execcommon.GetSupportedInterfaceVersionsCommand}).Run(ctx, p)
	if err != nil {
		t.Fatalf("Run GetSupportedInterfaceVersions: %v", err)
	}
	var versions []string
	if err := json.Unmarshal([]byte(out), &versions); err != nil {
		t.Fatalf("versions not JSON: %v", err)
	}
	if !slices.Contains(versions, "v0.1.1") {
		t.Errorf("versions %v should contain v0.1.1", versions)
	}

	// GetExtraSpecsJSONSchema -> the schema string.
	out, err = (executionv011.EnvironmentV011{Command: execcommon.GetExtraSpecsJSONSchemaCommand}).Run(ctx, p)
	if err != nil {
		t.Fatalf("Run GetExtraSpecsJSONSchema: %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("GetExtraSpecsJSONSchema via Run did not return valid JSON")
	}

	// GetConfigJSONSchema -> the schema string.
	if _, err := (executionv011.EnvironmentV011{Command: execcommon.GetConfigJSONSchemaCommand}).Run(ctx, p); err != nil {
		t.Fatalf("Run GetConfigJSONSchema: %v", err)
	}

	// ValidatePoolInfo -> nil (empty result) for a good extra_specs; the
	// dispatch passes BootstrapParams.Image/Flavor + ProviderConfigFile + ExtraSpecs.
	good := executionv011.EnvironmentV011{
		Command:         execcommon.ValidatePoolInfoCommand,
		ExtraSpecs:      `{"dind_mode": "none"}`,
		BootstrapParams: params.BootstrapInstance{},
	}
	if _, err := good.Run(ctx, p); err != nil {
		t.Errorf("Run ValidatePoolInfo (good) = %v, want nil", err)
	}

	// ValidatePoolInfo -> error for a reserved override.
	bad := executionv011.EnvironmentV011{
		Command:    execcommon.ValidatePoolInfoCommand,
		ExtraSpecs: `{"extra_env": {"RUNNER_EPHEMERAL": "false"}}`,
	}
	if _, err := bad.Run(ctx, p); err == nil {
		t.Error("Run ValidatePoolInfo (reserved override) = nil, want error")
	}
}
