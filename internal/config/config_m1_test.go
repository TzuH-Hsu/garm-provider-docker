package config

import "testing"

// TestLoadM1 covers Load's new M1 validation branches (dind_mode,
// allowed_dind_modes, dind_image, storage_driver, [resources], [flavors.*])
// via testdata fixtures, mirroring TestLoad's structure in config_test.go.
func TestLoadM1(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "full valid M1 config", path: "testdata/m1_valid.toml"},
		{name: "unknown dind_mode is rejected", path: "testdata/m1_dind_mode_invalid.toml", wantErr: true},
		{name: "dind_mode outside allowed_dind_modes is rejected", path: "testdata/m1_dind_mode_not_allowed.toml", wantErr: true},
		{name: "unknown entry in allowed_dind_modes is rejected", path: "testdata/m1_allowed_dind_modes_invalid.toml", wantErr: true},
		{name: "dind_image required when dind_mode != none", path: "testdata/m1_dind_image_missing.toml", wantErr: true},
		{name: "tag-only dind_image rejected by default", path: "testdata/m1_dind_image_unpinned.toml", wantErr: true},
		{name: "tag-only dind_image accepted with its own escape hatch", path: "testdata/m1_dind_image_unpinned_allowed.toml"},
		{name: "unsupported storage_driver is rejected", path: "testdata/m1_storage_driver_invalid.toml", wantErr: true},
		{name: "malformed runner_memory is rejected", path: "testdata/m1_resources_invalid.toml", wantErr: true},
		{name: "tag-only flavor runner_image is rejected", path: "testdata/m1_flavor_image_unpinned.toml", wantErr: true},
		{name: "malformed flavor runner_memory is rejected", path: "testdata/m1_flavor_memory_invalid.toml", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load(%q) succeeded, want error", tt.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q) returned unexpected error: %v", tt.path, err)
			}
		})
	}
}

// TestLoadM1Defaults confirms an M0-shaped config (no M1 keys at all, e.g.
// testdata/valid.toml) still loads with ADR-001's documented M1 defaults
// applied: dind_mode=none, every mode allowed, storage_driver=overlay2,
// enable_job_network=true, and internal=false (the 2026-07-21 owner ruling,
// ADR-001 Amendment — job networks default to open egress; per-allocation
// network separation, not this flag, provides job-to-job isolation).
func TestLoadM1Defaults(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.DindMode != DindModeNone {
		t.Errorf("DindMode = %q, want %q", cfg.DindMode, DindModeNone)
	}
	wantModes := []string{DindModeNone, DindModePrivilegedSidecar, DindModeSysboxRunc}
	if len(cfg.AllowedDindModes) != len(wantModes) {
		t.Fatalf("AllowedDindModes = %v, want %v", cfg.AllowedDindModes, wantModes)
	}
	for i, m := range wantModes {
		if cfg.AllowedDindModes[i] != m {
			t.Errorf("AllowedDindModes[%d] = %q, want %q", i, cfg.AllowedDindModes[i], m)
		}
	}
	if cfg.StorageDriver != defaultStorageDriver {
		t.Errorf("StorageDriver = %q, want %q", cfg.StorageDriver, defaultStorageDriver)
	}
	if !cfg.Network.EnableJobNetwork {
		t.Error("Network.EnableJobNetwork = false, want true (default)")
	}
	if cfg.Network.Internal {
		t.Error("Network.Internal = true, want false (default, 2026-07-21 owner ruling)")
	}
	if cfg.DindImage != "" {
		t.Errorf("DindImage = %q, want empty (dind_mode=none, never set)", cfg.DindImage)
	}
}

// TestConfigValidateDindMode is a focused table test on validateDindMode's
// branches, isolated from every other Validate check via validBaseConfig.
func TestConfigValidateDindMode(t *testing.T) {
	const digestRef = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const dindDigestRef = "docker:dind@sha256:beefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdead"

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "default none is valid", mutate: func(c *Config) {}},
		{
			name: "privileged-sidecar with a digest-pinned dind_image is valid",
			mutate: func(c *Config) {
				c.DindMode = DindModePrivilegedSidecar
				c.DindImage = dindDigestRef
			},
		},
		{
			name: "sysbox-runc with a digest-pinned dind_image is valid",
			mutate: func(c *Config) {
				c.DindMode = DindModeSysboxRunc
				c.DindImage = dindDigestRef
			},
		},
		{
			name:    "unknown dind_mode rejected",
			mutate:  func(c *Config) { c.DindMode = "bogus" },
			wantErr: true,
		},
		{
			name: "dind_mode not in allowed_dind_modes rejected",
			mutate: func(c *Config) {
				c.DindMode = DindModePrivilegedSidecar
				c.DindImage = dindDigestRef
				c.AllowedDindModes = []string{DindModeNone}
			},
			wantErr: true,
		},
		{
			name:    "unknown entry in allowed_dind_modes rejected",
			mutate:  func(c *Config) { c.AllowedDindModes = []string{DindModeNone, "bogus"} },
			wantErr: true,
		},
		{
			name:    "dind_image required when dind_mode is privileged-sidecar",
			mutate:  func(c *Config) { c.DindMode = DindModePrivilegedSidecar },
			wantErr: true,
		},
		{
			name:    "dind_image required when dind_mode is sysbox-runc",
			mutate:  func(c *Config) { c.DindMode = DindModeSysboxRunc },
			wantErr: true,
		},
		{
			name: "unpinned dind_image rejected by default",
			mutate: func(c *Config) {
				c.DindMode = DindModePrivilegedSidecar
				c.DindImage = "docker:dind"
			},
			wantErr: true,
		},
		{
			name: "unpinned dind_image accepted with its own escape hatch",
			mutate: func(c *Config) {
				c.DindMode = DindModePrivilegedSidecar
				c.DindImage = "docker:dind"
				c.AllowUnpinnedDindImage = true
			},
		},
		{
			name: "unpinned dind_image rejected even when AllowUnpinnedRunnerImage is set (separate flags)",
			mutate: func(c *Config) {
				c.DindMode = DindModePrivilegedSidecar
				c.DindImage = "docker:dind"
				c.AllowUnpinnedRunnerImage = true
			},
			wantErr: true,
		},
		{
			name:    "malformed dind_image validated even in none mode",
			mutate:  func(c *Config) { c.DindImage = "docker:dind@sha256:00aa" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig(digestRef, false)
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}

func TestConfigValidateStorageDriver(t *testing.T) {
	const digestRef = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	tests := []struct {
		name          string
		storageDriver string
		wantErr       bool
	}{
		{name: "overlay2 accepted", storageDriver: "overlay2"},
		{name: "vfs accepted", storageDriver: "vfs"},
		{name: "zfs rejected", storageDriver: "zfs", wantErr: true},
		{name: "empty rejected", storageDriver: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig(digestRef, false)
			cfg.StorageDriver = tt.storageDriver
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}

func TestConfigValidateResources(t *testing.T) {
	const digestRef = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	tests := []struct {
		name      string
		resources Resources
		wantErr   bool
	}{
		{name: "empty resources (unlimited) is valid", resources: Resources{}},
		{name: "valid runner_memory and dind_memory", resources: Resources{RunnerMemory: "8GiB", DindMemory: "4GiB"}},
		{name: "malformed runner_memory rejected", resources: Resources{RunnerMemory: "bogus"}, wantErr: true},
		{name: "malformed dind_memory rejected", resources: Resources{DindMemory: "bogus"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig(digestRef, false)
			cfg.Resources = tt.resources
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}

func TestConfigValidateFlavors(t *testing.T) {
	const digestRef = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const flavorDigestRef = "ghcr.io/example/garm-runner-noble-small@sha256:cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"

	tests := []struct {
		name    string
		flavors map[string]Flavor
		wantErr bool
	}{
		{name: "no flavors is valid", flavors: nil},
		{
			name: "flavor with valid overrides is valid",
			flavors: map[string]Flavor{
				"default": {RunnerMemory: "8GiB", DindMemory: "4GiB", RunnerImage: flavorDigestRef},
			},
		},
		{
			name:    "flavor with malformed runner_memory rejected",
			flavors: map[string]Flavor{"default": {RunnerMemory: "bogus"}},
			wantErr: true,
		},
		{
			name:    "flavor with malformed dind_memory rejected",
			flavors: map[string]Flavor{"default": {DindMemory: "bogus"}},
			wantErr: true,
		},
		{
			name:    "flavor with unpinned runner_image rejected",
			flavors: map[string]Flavor{"default": {RunnerImage: "ghcr.io/example/runner:dev"}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig(digestRef, false)
			cfg.Flavors = tt.flavors
			err := cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}

// TestConfigEffectiveDindMode covers EffectiveDindMode's ceiling enforcement
// (ADR-001 F7): a mode within allowed_dind_modes resolves cleanly, and a
// mode outside it — whether the config-wide default or a (future M3)
// poolMode override — is rejected with a clear error, exactly as
// validateDindMode already enforces at Load time (config_test.go /
// TestConfigValidateDindMode above), but re-checked here since this is the
// method WP4's provider create path calls defensively.
func TestConfigEffectiveDindMode(t *testing.T) {
	tests := []struct {
		name             string
		dindMode         string
		allowedDindModes []string
		poolMode         string
		want             string
		wantErr          bool
	}{
		{
			name:             "config default within the ceiling resolves",
			dindMode:         DindModePrivilegedSidecar,
			allowedDindModes: []string{DindModeNone, DindModePrivilegedSidecar, DindModeSysboxRunc},
			want:             DindModePrivilegedSidecar,
		},
		{
			name:             "config default outside the ceiling is rejected",
			dindMode:         DindModePrivilegedSidecar,
			allowedDindModes: []string{DindModeNone},
			wantErr:          true,
		},
		{
			name:             "sysbox-runc within the ceiling resolves",
			dindMode:         DindModeSysboxRunc,
			allowedDindModes: []string{DindModeNone, DindModeSysboxRunc},
			want:             DindModeSysboxRunc,
		},
		{
			name:             "sysbox-runc outside the ceiling is rejected",
			dindMode:         DindModeSysboxRunc,
			allowedDindModes: []string{DindModeNone, DindModePrivilegedSidecar},
			wantErr:          true,
		},
		{
			name:             "poolMode override within the ceiling wins over the config default",
			dindMode:         DindModeNone,
			allowedDindModes: []string{DindModeNone, DindModePrivilegedSidecar},
			poolMode:         DindModePrivilegedSidecar,
			want:             DindModePrivilegedSidecar,
		},
		{
			name:             "poolMode override outside the ceiling is rejected even though the config default is allowed",
			dindMode:         DindModeNone,
			allowedDindModes: []string{DindModeNone},
			poolMode:         DindModeSysboxRunc,
			wantErr:          true,
		},
		{
			name:             "an empty AllowedDindModes (a hand-built Config that skipped Load) is unrestricted",
			dindMode:         DindModePrivilegedSidecar,
			allowedDindModes: nil,
			want:             DindModePrivilegedSidecar,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{DindMode: tt.dindMode, AllowedDindModes: tt.allowedDindModes}
			got, err := cfg.EffectiveDindMode(tt.poolMode)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("EffectiveDindMode(%q) succeeded with %q, want an error", tt.poolMode, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("EffectiveDindMode(%q) returned unexpected error: %v", tt.poolMode, err)
			}
			if got != tt.want {
				t.Errorf("EffectiveDindMode(%q) = %q, want %q", tt.poolMode, got, tt.want)
			}
		})
	}
}

func TestConfigEffectiveRunnerMemoryBytes(t *testing.T) {
	cfg := validBaseConfig("ghcr.io/example/runner@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false)
	cfg.Resources = Resources{RunnerMemory: "8GiB", DindMemory: "4GiB"}
	cfg.Flavors = map[string]Flavor{
		"small": {RunnerMemory: "2GiB"},
		"empty": {},
	}

	tests := []struct {
		name       string
		flavorName string
		want       int64
	}{
		{name: "no flavor falls back to config default", flavorName: "", want: 8 << 30},
		{name: "unknown flavor falls back to config default", flavorName: "does-not-exist", want: 8 << 30},
		{name: "flavor override wins", flavorName: "small", want: 2 << 30},
		{name: "flavor with no override falls back to config default", flavorName: "empty", want: 8 << 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cfg.EffectiveRunnerMemoryBytes(tt.flavorName)
			if err != nil {
				t.Fatalf("EffectiveRunnerMemoryBytes(%q) returned unexpected error: %v", tt.flavorName, err)
			}
			if got != tt.want {
				t.Errorf("EffectiveRunnerMemoryBytes(%q) = %d, want %d", tt.flavorName, got, tt.want)
			}
		})
	}
}

func TestConfigEffectiveDindMemoryBytes(t *testing.T) {
	cfg := validBaseConfig("ghcr.io/example/runner@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", false)
	cfg.Resources = Resources{DindMemory: "4GiB"}
	cfg.Flavors = map[string]Flavor{"small": {DindMemory: "1GiB"}}

	got, err := cfg.EffectiveDindMemoryBytes("small")
	if err != nil {
		t.Fatalf("EffectiveDindMemoryBytes returned unexpected error: %v", err)
	}
	if got != 1<<30 {
		t.Errorf("EffectiveDindMemoryBytes(small) = %d, want %d", got, 1<<30)
	}

	got, err = cfg.EffectiveDindMemoryBytes("")
	if err != nil {
		t.Fatalf("EffectiveDindMemoryBytes returned unexpected error: %v", err)
	}
	if got != 4<<30 {
		t.Errorf("EffectiveDindMemoryBytes(\"\") = %d, want %d", got, 4<<30)
	}
}

func TestConfigEffectiveRunnerImage(t *testing.T) {
	const topLevelImage = "ghcr.io/example/runner@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const flavorImage = "ghcr.io/example/runner-small@sha256:cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"

	cfg := validBaseConfig(topLevelImage, false)
	cfg.Flavors = map[string]Flavor{
		"small": {RunnerImage: flavorImage},
		"empty": {},
	}

	tests := []struct {
		name       string
		flavorName string
		want       string
	}{
		{name: "no flavor uses top-level image", flavorName: "", want: topLevelImage},
		{name: "unknown flavor uses top-level image", flavorName: "does-not-exist", want: topLevelImage},
		{name: "flavor override wins", flavorName: "small", want: flavorImage},
		{name: "flavor with no image override uses top-level image", flavorName: "empty", want: topLevelImage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.EffectiveRunnerImage(tt.flavorName); got != tt.want {
				t.Errorf("EffectiveRunnerImage(%q) = %q, want %q", tt.flavorName, got, tt.want)
			}
		})
	}
}
