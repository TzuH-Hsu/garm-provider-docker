package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xeipuuv/gojsonschema"
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

	// The schema must state the constraints the loader actually enforces, not
	// merely list the key names: a self-describing schema that accepts `{}`
	// while Load rejects it is worse than none, because a consumer validating
	// against it gets a false PASS.
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

// TestJSONSchemaMatchesLoaderOnRequiredKeys compiles the published schema and
// runs real instance documents through it, asserting the schema AGREES with
// the loader on which configs are acceptable. Structural assertions alone
// (does an "if" key exist?) would not catch a conditional that is present but
// wrong, which is exactly how the schema drifted into accepting `{}`.
//
// gojsonschema is the same validator internal/extraspecs uses for the
// extra_specs contract, so compiling successfully here also proves the schema
// is well-formed draft-07 rather than merely well-formed JSON.
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
		// toml, when non-empty, is a TOML-equivalent of instance: it is also run
		// through the real Load(), and the resulting success/failure is asserted
		// to AGREE with wantOK — so this table isn't just testing the schema in
		// isolation, it is a truth table both the schema and the loader must
		// satisfy identically (the whole point of a self-describing schema).
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
			// omitted dind_mode + omitted allowed_dind_modes: both default
			// ("none" / ["none"]), and "none" is trivially a member of its own
			// default ceiling.
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
			// R4(b): a non-"none" dind_mode with an OMITTED allowed_dind_modes
			// must now FAIL schema validation, matching the loader's fail-closed
			// ceiling (allowed_dind_modes defaults to ["none"], M4-W1) — this is
			// the case that used to pass the schema while the loader rejected it.
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
		// H2/M4-W1: the loader's membership check (dind_mode must be a member of
		// allowed_dind_modes) is UNCONDITIONAL — it applies just as much to the
		// "none"/omitted default as to privileged-sidecar/sysbox-runc above. The
		// schema used to omit this for the "none" case (no allOf conditional
		// covered it), a false PASS the loader itself rejects. These cases pin
		// the full truth table down, with a real Load() cross-check where
		// practical.
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
			// dind_mode omitted (defaults to "none") but the ceiling is
			// explicitly widened to include privileged-sidecar too: "none" is
			// still a member, so this is a PASS despite the wider ceiling —
			// widening the ceiling only ever adds permission, never removes it.
			name:     "omitted dind_mode with a ceiling containing none plus another mode accepted",
			instance: `{"runner_image": "` + runnerRef + `", "allowed_dind_modes": ["none", "privileged-sidecar"]}`,
			toml:     runnerTOML + "allowed_dind_modes = [\"none\", \"privileged-sidecar\"]\n",
			wantOK:   true,
		},
		{
			// additionalProperties stays true at the top level on purpose:
			// the loader tolerates unknown TOML keys for forward/backward
			// compatibility, and the schema must not claim otherwise.
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
			// Cross-check: the same instance, as TOML, run through the REAL
			// loader. The schema and the loader must agree — that is the whole
			// point of a self-describing schema (see the package doc comment on
			// JSONSchema()).
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

// validBaseConfig returns a Config that passes every M0+M1 Validate check.
// dind_mode and storage_driver match the same defaults Load() applies, but
// allowed_dind_modes is DELIBERATELY WIDENED to all three modes here rather
// than matching Load()'s real fail-closed ["none"] default (M4-W1), so a test
// focused on one field (e.g. RunnerImage here, or a single M1 field in
// config_m1_test.go) can freely combine any dind_mode without also having to
// override the ceiling just to reach the check it actually cares about. Tests
// that hand-build a Config directly (bypassing Load(), which applies the real
// ["none"] ceiling default from a bare TOML file) must start from this
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
