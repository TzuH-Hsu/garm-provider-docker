package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xeipuuv/gojsonschema"
)

// TestJSONSchemaIsValidDraft07 asserts the embedded provider-config schema is
// parseable JSON, declares draft-07 and describes the top-level config keys.
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

	// The schema must state the constraints the loader enforces, not just list
	// key names: a schema that accepts `{}` while Load rejects it gives a
	// consumer a false pass.
	req, ok := m["required"].([]any)
	if !ok || len(req) == 0 {
		t.Fatalf("config schema declares no top-level required keys, but Load requires runner_image: required=%v", m["required"])
	}
	if req[0] != "runner_image" {
		t.Errorf("config schema required = %v, want runner_image", req)
	}
	if _, ok := m["if"]; !ok {
		t.Error("config schema has no if/then conditional for the dind_image rule")
	}
	if _, ok := m["then"]; !ok {
		t.Error("config schema has an if with no matching then")
	}
}

// TestJSONSchemaMatchesLoaderOnRequiredKeys runs real instance documents
// through the published schema and checks that it agrees with the loader on
// which configs are acceptable. Checking only that an "if" key exists would miss
// a conditional that is present but wrong. Compiling the schema with
// gojsonschema also proves it is well-formed draft-07.
func TestJSONSchemaMatchesLoaderOnRequiredKeys(t *testing.T) {
	schema, err := gojsonschema.NewSchema(gojsonschema.NewStringLoader(JSONSchema()))
	if err != nil {
		t.Fatalf("config schema does not compile as draft-07: %v", err)
	}

	const runnerRef = "ghcr.io/example/runner@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const dindRef = "docker:dind@sha256:beefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdead"
	const runnerTOML = `runner_image = "` + runnerRef + `"` + "\n"

	tests := []struct {
		name     string
		instance string
		// toml, when non-empty, is the TOML form of instance. It also goes through
		// Load, whose result must agree with wantOK, so the table is a truth table
		// that both the schema and the loader must satisfy.
		toml   string
		wantOK bool
	}{
		{
			name:     "empty object rejected (runner_image is required)",
			instance: `{}`,
			wantOK:   false,
		},
		{
			name:     "empty runner_image rejected",
			instance: `{"runner_image": ""}`,
			wantOK:   false,
		},
		{
			// dind_mode and allowed_dind_modes both omitted: they default to "none"
			// and ["none"], and "none" is a member of its own default ceiling.
			name:     "runner_image alone is a complete minimal config",
			instance: `{"runner_image": "` + runnerRef + `"}`,
			toml:     runnerTOML,
			wantOK:   true,
		},
		{
			name:     "explicit dind_mode none needs no dind_image",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "none"}`,
			toml:     runnerTOML + `dind_mode = "none"` + "\n",
			wantOK:   true,
		},
		{
			name:     "privileged-sidecar without dind_image rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "privileged-sidecar"}`,
			wantOK:   false,
		},
		{
			name:     "privileged-sidecar with an EMPTY dind_image rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "privileged-sidecar", "dind_image": ""}`,
			wantOK:   false,
		},
		{
			// A non-"none" dind_mode with allowed_dind_modes omitted must fail the
			// schema too, matching the loader's fail-closed ["none"] default.
			name:     "privileged-sidecar with dind_image but no allowed_dind_modes ceiling rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "privileged-sidecar", "dind_image": "` + dindRef + `"}`,
			wantOK:   false,
		},
		{
			name:     "privileged-sidecar with dind_image and a ceiling that EXCLUDES it rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "privileged-sidecar", "dind_image": "` + dindRef + `", "allowed_dind_modes": ["none"]}`,
			wantOK:   false,
		},
		{
			name:     "privileged-sidecar with dind_image and a ceiling that CONTAINS it accepted",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "privileged-sidecar", "dind_image": "` + dindRef + `", "allowed_dind_modes": ["privileged-sidecar"]}`,
			wantOK:   true,
		},
		{
			name:     "sysbox-runc without dind_image rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "sysbox-runc"}`,
			wantOK:   false,
		},
		{
			name:     "sysbox-runc with dind_image but no allowed_dind_modes ceiling rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "sysbox-runc", "dind_image": "` + dindRef + `"}`,
			wantOK:   false,
		},
		{
			name:     "sysbox-runc with dind_image and a ceiling that CONTAINS it accepted",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "sysbox-runc", "dind_image": "` + dindRef + `", "allowed_dind_modes": ["sysbox-runc"]}`,
			wantOK:   true,
		},
		{
			name:     "unknown dind_mode rejected by the enum",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "bogus"}`,
			wantOK:   false,
		},
		// The loader's membership check (dind_mode must be in allowed_dind_modes)
		// is unconditional: it applies to the "none" or omitted default as much as
		// to the other modes. The schema must say so too, and these cases check
		// it against Load where they can.
		{
			name:     "none mode with an EMPTY allowed_dind_modes ceiling rejected (empty ceiling denies every mode)",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "none", "allowed_dind_modes": []}`,
			toml:     runnerTOML + "dind_mode = \"none\"\nallowed_dind_modes = []\n",
			wantOK:   false,
		},
		{
			name:     "none mode with a ceiling that EXCLUDES none rejected",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "none", "allowed_dind_modes": ["privileged-sidecar"]}`,
			toml:     runnerTOML + "dind_mode = \"none\"\nallowed_dind_modes = [\"privileged-sidecar\"]\n",
			wantOK:   false,
		},
		{
			name:     "omitted dind_mode (defaults to none) with a ceiling that EXCLUDES none rejected",
			instance: `{"runner_image": "` + runnerRef + `", "allowed_dind_modes": ["privileged-sidecar"]}`,
			toml:     runnerTOML + "allowed_dind_modes = [\"privileged-sidecar\"]\n",
			wantOK:   false,
		},
		{
			name:     "none mode with a ceiling that CONTAINS none accepted",
			instance: `{"runner_image": "` + runnerRef + `", "dind_mode": "none", "allowed_dind_modes": ["none"]}`,
			toml:     runnerTOML + "dind_mode = \"none\"\nallowed_dind_modes = [\"none\"]\n",
			wantOK:   true,
		},
		{
			// dind_mode omitted (defaults to "none") and the ceiling widened: "none"
			// is still a member, so this passes. Widening only adds permission.
			name:     "omitted dind_mode with a ceiling containing none plus another mode accepted",
			instance: `{"runner_image": "` + runnerRef + `", "allowed_dind_modes": ["none", "privileged-sidecar"]}`,
			toml:     runnerTOML + "allowed_dind_modes = [\"none\", \"privileged-sidecar\"]\n",
			wantOK:   true,
		},
		{
			// additionalProperties stays true at the top level because the loader
			// ignores unknown TOML keys, and the schema must not claim otherwise.
			name:     "unknown top-level key tolerated, matching the loader",
			instance: `{"runner_image": "` + runnerRef + `", "some_future_key": 1}`,
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := schema.Validate(gojsonschema.NewStringLoader(tt.instance))
			if err != nil {
				t.Fatalf("validating %s: %v", tt.instance, err)
			}
			if got := result.Valid(); got != tt.wantOK {
				t.Errorf("schema.Validate(%s) valid = %v, want %v (errors: %v)",
					tt.instance, got, tt.wantOK, result.Errors())
			}

			if tt.toml == "" {
				return
			}
			// Cross-check: the same instance as TOML through the real loader.
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0o600); err != nil {
				t.Fatalf("writing temp config %q: %v", path, err)
			}
			_, loadErr := Load(path)
			if gotLoadOK := loadErr == nil; gotLoadOK != tt.wantOK {
				t.Errorf("Load(%s) ok = %v, want %v (err: %v) — schema and loader DISAGREE",
					tt.toml, gotLoadOK, tt.wantOK, loadErr)
			}
		})
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

// validBaseConfig returns a Config that passes Validate. dind_mode and
// storage_driver match Load's defaults, but allowed_dind_modes is widened to all
// three modes instead of Load's ["none"], so a test can combine any dind_mode
// without overriding the ceiling. Tests that build a Config by hand should start
// from this and not from a bare Config{}.
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
