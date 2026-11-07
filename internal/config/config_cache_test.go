package config

import "testing"

// TestLoadCache covers Load's [cache] validation through testdata fixtures.
func TestLoadCache(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "full valid [cache] block", path: "testdata/cache_valid.toml"},
		{name: "cache disabled is valid", path: "testdata/cache_disabled.toml"},
		{name: "empty generation is rejected", path: "testdata/cache_generation_empty.toml", wantErr: true},
		{name: "generation with a slash is rejected", path: "testdata/cache_generation_invalid.toml", wantErr: true},
		{name: "relative toolcache_path is rejected", path: "testdata/cache_path_relative.toml", wantErr: true},
		{name: "colliding cache paths are rejected", path: "testdata/cache_paths_collide.toml", wantErr: true},
		{name: "cache path shadowing a reserved mount is rejected", path: "testdata/cache_path_reserved.toml", wantErr: true},
		{name: "negative retention window is rejected", path: "testdata/cache_days_negative.toml", wantErr: true},
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

// TestLoadCacheDefaults confirms a config with no [cache] table loads with the
// documented cache defaults.
func TestLoadCacheDefaults(t *testing.T) {
	cfg, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	c := cfg.Cache
	if !c.Enabled {
		t.Error("Cache.Enabled = false, want true (default)")
	}
	if c.Generation != defaultCacheGeneration {
		t.Errorf("Cache.Generation = %q, want %q", c.Generation, defaultCacheGeneration)
	}
	if c.PnpmMajor != defaultCachePnpmMajor {
		t.Errorf("Cache.PnpmMajor = %q, want %q", c.PnpmMajor, defaultCachePnpmMajor)
	}
	if c.ToolcachePath != defaultToolcachePath {
		t.Errorf("Cache.ToolcachePath = %q, want %q", c.ToolcachePath, defaultToolcachePath)
	}
	if c.PnpmStorePath != defaultPnpmStorePath {
		t.Errorf("Cache.PnpmStorePath = %q, want %q", c.PnpmStorePath, defaultPnpmStorePath)
	}
	if c.AllowOrgShared {
		t.Error("Cache.AllowOrgShared = true, want false (default: org/enterprise pools get no persistent cache)")
	}
	if c.StaleCacheEvictionDays != defaultStaleCacheEvictionDays {
		t.Errorf("Cache.StaleCacheEvictionDays = %d, want %d", c.StaleCacheEvictionDays, defaultStaleCacheEvictionDays)
	}
	if c.DiagnosticLogRetentionDays != defaultDiagnosticLogRetentionDays {
		t.Errorf("Cache.DiagnosticLogRetentionDays = %d, want %d", c.DiagnosticLogRetentionDays, defaultDiagnosticLogRetentionDays)
	}
}

// TestLoadCacheEnabledFalseOverridesDefault confirms an explicit
// enabled = false overrides the true default.
func TestLoadCacheEnabledFalseOverridesDefault(t *testing.T) {
	cfg, err := Load("testdata/cache_disabled.toml")
	if err != nil {
		t.Fatalf("Load returned unexpected error: %v", err)
	}
	if cfg.Cache.Enabled {
		t.Error("Cache.Enabled = true, want false (explicit enabled = false in the file)")
	}
}

// TestCacheValidate is a focused table test on Cache.validate, independent of
// TOML parsing.
func TestCacheValidate(t *testing.T) {
	base := defaultCache()
	tests := []struct {
		name    string
		mutate  func(*Cache)
		wantErr bool
	}{
		{name: "defaults are valid", mutate: func(*Cache) {}},
		{name: "disabled with junk salts is valid (inert)", mutate: func(c *Cache) {
			c.Enabled = false
			c.Generation = ""
			c.ToolcachePath = "not-absolute"
		}},
		{name: "empty generation rejected when enabled", mutate: func(c *Cache) { c.Generation = "" }, wantErr: true},
		{name: "empty pnpm_major rejected when enabled", mutate: func(c *Cache) { c.PnpmMajor = "" }, wantErr: true},
		{name: "relative pnpm_store_path rejected", mutate: func(c *Cache) { c.PnpmStorePath = "opt/pnpm" }, wantErr: true},
		{name: "identical paths rejected", mutate: func(c *Cache) { c.PnpmStorePath = c.ToolcachePath }, wantErr: true},
		{name: "toolcache under /run rejected", mutate: func(c *Cache) { c.ToolcachePath = "/run/x" }, wantErr: true},
		{name: "toolcache shadowing the externals mount rejected", mutate: func(c *Cache) { c.ToolcachePath = "/actions-runner/externals" }, wantErr: true},
		{name: "pnpm store shadowing the diag mount rejected", mutate: func(c *Cache) { c.PnpmStorePath = "/actions-runner/_diag" }, wantErr: true},
		{name: "toolcache nested under the externals mount rejected", mutate: func(c *Cache) { c.ToolcachePath = "/actions-runner/externals/node20" }, wantErr: true},
		{name: "negative stale days rejected", mutate: func(c *Cache) { c.StaleCacheEvictionDays = -5 }, wantErr: true},
		{name: "negative log retention rejected", mutate: func(c *Cache) { c.DiagnosticLogRetentionDays = -1 }, wantErr: true},

		// Canonical-alias bypasses: each of these resolves onto a reserved in-runner
		// mount, the runner install dir or the /run socket dir, and would slip past
		// a check on the raw string.
		{name: "dot-segment onto the runner install dir rejected", mutate: func(c *Cache) { c.ToolcachePath = "/actions-runner/." }, wantErr: true},
		{name: "/opt/../ traversal onto the runner install dir rejected", mutate: func(c *Cache) { c.ToolcachePath = "/opt/../actions-runner" }, wantErr: true},
		{name: "repeated slashes are non-canonical, rejected", mutate: func(c *Cache) { c.ToolcachePath = "//opt//hostedtoolcache" }, wantErr: true},
		{name: "trailing slash is non-canonical, rejected", mutate: func(c *Cache) { c.PnpmStorePath = "/opt/pnpm-store/" }, wantErr: true},
		{name: "filesystem root rejected", mutate: func(c *Cache) { c.ToolcachePath = "/" }, wantErr: true},
		{name: "/var/run aliases onto the /run socket dir, rejected", mutate: func(c *Cache) { c.ToolcachePath = "/var/run" }, wantErr: true},
		{name: "/var/run/garm aliases onto the credential tmpfs, rejected", mutate: func(c *Cache) { c.PnpmStorePath = "/var/run/garm" }, wantErr: true},
		{name: "the bare runner install dir is reserved", mutate: func(c *Cache) { c.ToolcachePath = "/actions-runner" }, wantErr: true},
		{name: "traversal that canonicalizes onto the runner dir rejected", mutate: func(c *Cache) { c.PnpmStorePath = "/actions-runner/x/.." }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			err := c.validate()
			if tt.wantErr && err == nil {
				t.Fatalf("validate() = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validate() = %v, want nil", err)
			}
		})
	}
}
