package config

import "testing"

// TestExamplesLoadAndValidate is the anti-drift pin for the two
// user-facing example configs under examples/ (docs/plan.md M4 item 4;
// referenced from the root README's Quick start section and
// docs/config-reference.md). It loads each one through the REAL Load
// entry point - the same one main.go calls - so if either example ever
// drifts out of sync with internal/config (a renamed key, a changed
// default, a newly-required field), this test fails rather than the
// example silently going stale.
func TestExamplesLoadAndValidate(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "config.minimal.toml", path: "../../examples/config.minimal.toml"},
		{name: "config.full.toml", path: "../../examples/config.full.toml"},
	}

	for _, tt := range tests {
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

// TestExampleMinimalUsesDocumentedDefaults asserts config.minimal.toml
// actually exercises the "smallest working config" claim made in its own
// header comment and the root README: every field left unset resolves to
// its documented default, not to some other value that merely happens to
// validate.
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

// TestExampleFullMatchesDocumentedDefaults asserts every default-valued
// field config.full.toml claims to set "to its default" in its own
// comments actually IS that default in code, per field group. A value the
// example deliberately overrides away from the default (runner_memory,
// dind_image, flavors, extra_specs.allowed_env) is intentionally NOT
// asserted here as a default - only the fields the file's own comments
// claim equal the default are checked, so this test fails the moment
// either side (the example's comment or the code) drifts from the other.
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
