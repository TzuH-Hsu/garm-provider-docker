package config

import (
	"encoding/json"
	"testing"
)

// TestJSONSchemaIsValidDraft07 asserts the embedded provider-config schema
// (returned by the v0.1.1 GetConfigJSONSchema command) is valid, parseable JSON
// declaring draft-07 and describing the top-level config keys.
func TestJSONSchemaIsValidDraft07(t *testing.T) {
	var m map[string]any
	if err := json.Unmarshal([]byte(JSONSchema()), &m); err != nil {
		t.Fatalf("JSONSchema() is not valid JSON: %v", err)
	}
	if m["$schema"] != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("config schema is not draft-07: $schema=%v", m["$schema"])
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("config schema has no properties object")
	}
	for _, want := range []string{"docker_host", "runner_image", "dind_mode", "allowed_dind_modes", "storage_driver", "resources", "network", "flavors", "cache"} {
		if _, ok := props[want]; !ok {
			t.Errorf("config schema is missing property %q", want)
		}
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantErr    bool
		wantHost   string
		wantRunner string
	}{
		{
			name:       "valid config with explicit docker_host",
			path:       "testdata/valid.toml",
			wantHost:   "unix:///var/run/docker-custom.sock",
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:       "docker_host defaults when omitted",
			path:       "testdata/valid_defaults.toml",
			wantHost:   defaultDockerHost,
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:    "missing runner_image is rejected",
			path:    "testdata/missing_runner_image.toml",
			wantErr: true,
		},
		{
			name:    "malformed TOML is rejected",
			path:    "testdata/bad.toml",
			wantErr: true,
		},
		{
			name:       "full ADR-005-shaped config is tolerated (forward-compat)",
			path:       "testdata/forward_compat.toml",
			wantHost:   "unix:///var/run/docker.sock",
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:    "nonexistent file is rejected",
			path:    "testdata/does-not-exist.toml",
			wantErr: true,
		},
		{
			name:    "tag-only runner_image is rejected by default",
			path:    "testdata/unpinned_rejected.toml",
			wantErr: true,
		},
		{
			name:       "tag-only runner_image accepted with the dev escape hatch",
			path:       "testdata/unpinned_allowed.toml",
			wantHost:   defaultDockerHost,
			wantRunner: "ghcr.io/example/garm-runner-noble:dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load(%q) succeeded, want error", tt.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q) returned unexpected error: %v", tt.path, err)
			}
			if cfg.DockerHost != tt.wantHost {
				t.Errorf("DockerHost = %q, want %q", cfg.DockerHost, tt.wantHost)
			}
			if cfg.RunnerImage != tt.wantRunner {
				t.Errorf("RunnerImage = %q, want %q", cfg.RunnerImage, tt.wantRunner)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	// A valid, digest-pinned reference (name@sha256:<64-hex>).
	const digestRef = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "digest-pinned runner_image accepted",
			cfg:  validBaseConfig(digestRef, false),
		},
		{
			name:    "tag-only runner_image rejected by default",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner:latest"},
			wantErr: true,
		},
		{
			name: "tag-only runner_image accepted with the escape hatch",
			cfg:  validBaseConfig("ghcr.io/example/runner:latest", true),
		},
		{
			name:    "malformed digest rejected",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner@sha256:00aa"},
			wantErr: true,
		},
		{
			name:    "malformed digest rejected even with the escape hatch",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner@sha256:00aa", AllowUnpinnedRunnerImage: true},
			wantErr: true,
		},
		{
			name:    "runner_image empty",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: ""},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}

// validBaseConfig returns a Config that passes every M0+M1 Validate check
// (dind_mode/allowed_dind_modes/storage_driver default to the same values
// Load() applies), so a test focused on one field (e.g. RunnerImage here,
// or a single M1 field in config_m1_test.go) does not have to separately
// satisfy every other field's validation just to reach the check it cares
// about. Tests that hand-build a Config directly (bypassing Load(), which
// applies these same defaults from a bare TOML file) must start from this
// baseline rather than a bare Config{} literal, or they fail on fields
// unrelated to what they're testing.
func validBaseConfig(runnerImage string, allowUnpinnedRunnerImage bool) Config {
	return Config{
		DockerHost:               defaultDockerHost,
		RunnerImage:              runnerImage,
		AllowUnpinnedRunnerImage: allowUnpinnedRunnerImage,
		DindMode:                 DindModeNone,
		AllowedDindModes:         []string{DindModeNone, DindModePrivilegedSidecar, DindModeSysboxRunc},
		StorageDriver:            defaultStorageDriver,
	}
}
