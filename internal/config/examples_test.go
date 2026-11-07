package config

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// examplePaths are the two user-facing example configs under examples/,
// referenced from the root README's Quick start section and
// docs/config-reference.md.
var examplePaths = []struct {
	name string
	path string
}{
	{name: "config.minimal.toml", path: "../../examples/config.minimal.toml"},
	{name: "config.full.toml", path: "../../examples/config.full.toml"},
}

// TestExamplesLoadAndValidate loads each example through Load, the entry point
// main.go uses, so an example fails the build when it drifts from this package.
func TestExamplesLoadAndValidate(t *testing.T) {
	for _, tt := range examplePaths {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.path)
			if err != nil {
				t.Fatalf("Load(%q) returned unexpected error: %v", tt.path, err)
			}
			if cfg.RunnerImage == "" {
				t.Errorf("Load(%q): RunnerImage is empty after a successful load", tt.path)
			}
		})
	}
}

// TestExamplesHaveNoUnknownKeys covers what Load cannot: Load ignores unknown
// keys, so a misspelled key in an example (`dind_mod`) or one renamed in code
// would still load. This test requires every key to map onto a Config field.
// The strictness is in the test, not the loader, because an operator's real
// config must keep tolerating unknown keys.
func TestExamplesHaveNoUnknownKeys(t *testing.T) {
	for _, tt := range examplePaths {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			md, err := toml.DecodeFile(tt.path, &cfg)
			if err != nil {
				t.Fatalf("DecodeFile(%q) returned unexpected error: %v", tt.path, err)
			}
			if undecoded := md.Undecoded(); len(undecoded) > 0 {
				t.Errorf("%s contains %d key(s) that no Config field maps to: %v\n"+
					"Either the example has a typo/stale key, or a field was renamed in internal/config "+
					"without updating the example.", tt.path, len(undecoded), undecoded)
			}
		})
	}
}

// TestExampleMinimalUsesDocumentedDefaults asserts that every field left unset
// in config.minimal.toml resolves to its documented default.
func TestExampleMinimalUsesDocumentedDefaults(t *testing.T) {
	cfg, err := Load("../../examples/config.minimal.toml")
	if err != nil {
		t.Fatalf("Load(minimal) returned unexpected error: %v", err)
	}

	if cfg.DockerHost != defaultDockerHost {
		t.Errorf("DockerHost = %q, want default %q", cfg.DockerHost, defaultDockerHost)
	}
	if cfg.DindMode != DindModeNone {
		t.Errorf("DindMode = %q, want default %q", cfg.DindMode, DindModeNone)
	}
	if cfg.StorageDriver != defaultStorageDriver {
		t.Errorf("StorageDriver = %q, want default %q", cfg.StorageDriver, defaultStorageDriver)
	}
	if !cfg.Cache.Enabled {
		t.Error("Cache.Enabled = false, want true (the default)")
	}
	if cfg.Network.Internal {
		t.Error("Network.Internal = true, want false (the default)")
	}
	if len(cfg.ExtraSpecs.AllowedEnv) != 0 {
		t.Errorf("ExtraSpecs.AllowedEnv = %v, want empty (fail-closed default)", cfg.ExtraSpecs.AllowedEnv)
	}
}

// TestExampleFullMatchesDocumentedDefaults asserts that each field
// config.full.toml marks as its default equals the default in code. Values the
// example overrides on purpose (runner_memory, dind_image, flavors,
// extra_specs.allowed_env) are not checked.
func TestExampleFullMatchesDocumentedDefaults(t *testing.T) {
	cfg, err := Load("../../examples/config.full.toml")
	if err != nil {
		t.Fatalf("Load(full) returned unexpected error: %v", err)
	}

	if cfg.DockerHost != defaultDockerHost {
		t.Errorf("DockerHost = %q, want default %q", cfg.DockerHost, defaultDockerHost)
	}
	if cfg.AllowUnpinnedRunnerImage {
		t.Error("AllowUnpinnedRunnerImage = true, want false (the default)")
	}
	if cfg.DindMode != DindModeNone {
		t.Errorf("DindMode = %q, want default %q", cfg.DindMode, DindModeNone)
	}
	wantAllowedDindModes := []string{DindModeNone}
	if len(cfg.AllowedDindModes) != len(wantAllowedDindModes) {
		t.Fatalf("AllowedDindModes = %v, want %v", cfg.AllowedDindModes, wantAllowedDindModes)
	}
	for i, m := range wantAllowedDindModes {
		if cfg.AllowedDindModes[i] != m {
			t.Errorf("AllowedDindModes[%d] = %q, want %q", i, cfg.AllowedDindModes[i], m)
		}
	}
	if cfg.AllowUnpinnedDindImage {
		t.Error("AllowUnpinnedDindImage = true, want false (the default)")
	}
	if cfg.StorageDriver != defaultStorageDriver {
		t.Errorf("StorageDriver = %q, want default %q", cfg.StorageDriver, defaultStorageDriver)
	}
	if !cfg.Network.EnableJobNetwork {
		t.Error("Network.EnableJobNetwork = false, want true (the default)")
	}
	if cfg.Network.Internal {
		t.Error("Network.Internal = true, want false (the default)")
	}

	wantCache := defaultCache()
	if cfg.Cache != wantCache {
		t.Errorf("Cache = %+v, want the documented defaults %+v", cfg.Cache, wantCache)
	}
}

// TestExampleFullShowsRegistrySettings checks that config.full.toml keeps an
// illustrative value for both registry keys that the loader accepts. Their
// defaults are empty, so the defaults test above does not cover them.
func TestExampleFullShowsRegistrySettings(t *testing.T) {
	cfg, err := Load("../../examples/config.full.toml")
	if err != nil {
		t.Fatalf("Load(full) returned unexpected error: %v", err)
	}
	if len(cfg.RegistryMirrors) == 0 {
		t.Error("config.full.toml sets no registry_mirrors, want an illustrative value")
	}
	if len(cfg.InsecureRegistries) == 0 {
		t.Error("config.full.toml sets no insecure_registries, want an illustrative value")
	}

	minimal, err := Load("../../examples/config.minimal.toml")
	if err != nil {
		t.Fatalf("Load(minimal) returned unexpected error: %v", err)
	}
	if len(minimal.RegistryMirrors) != 0 || len(minimal.InsecureRegistries) != 0 {
		t.Errorf("config.minimal.toml sets registry settings %v / %v, want none (the defaults)",
			minimal.RegistryMirrors, minimal.InsecureRegistries)
	}
}
