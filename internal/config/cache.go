package config

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Cache defaults. Caching is on out of the box; allow_org_shared stays off.
const (
	defaultCacheGeneration            = "1"
	defaultCachePnpmMajor             = "9"
	defaultToolcachePath              = "/opt/hostedtoolcache"
	defaultPnpmStorePath              = "/opt/pnpm-store"
	defaultStaleCacheEvictionDays     = 30
	defaultDiagnosticLogRetentionDays = 7
)

// cacheSegmentPattern limits generation and pnpm_major, which are embedded in
// Docker volume names.
var cacheSegmentPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// reservedMountPaths are the fixed in-runner mounts that a cache path must not
// collide with. The runner install dir is on the list because it holds
// registration credentials during a non-JIT setup. The list mirrors
// internal/spec, which this package does not import. Every entry is canonical.
var reservedMountPaths = []string{
	"/run/garm",
	"/run",
	"/actions-runner",
	"/actions-runner/_work",
	"/actions-runner/externals",
	"/actions-runner/_diag",
	"/var/lib/docker",
}

// Cache is the [cache] table (ADR-003). The stale-cache GC only logs a stale
// volume and never deletes one; the diagnostic-log prune deletes aged files
// inside the diagnostics volume, not the volume.
type Cache struct {
	// Enabled false provisions no cache volumes and sets no cache environment.
	Enabled bool `toml:"enabled"`

	// Generation and PnpmMajor are salts in the volume names: changing one starts
	// a fresh, empty volume. Change PnpmMajor with pnpm's major version, because
	// the store layout depends on it. Both must be legal Docker name fragments.
	Generation string `toml:"generation"`
	PnpmMajor  string `toml:"pnpm_major"`

	// ToolcachePath and PnpmStorePath are the absolute in-runner mount points.
	// ToolcachePath is also the value of RUNNER_TOOL_CACHE; PnpmStorePath reaches
	// pnpm through environment variables (ADR-003, "pnpm").
	ToolcachePath string `toml:"toolcache_path"`
	PnpmStorePath string `toml:"pnpm_store_path"`

	// AllowOrgShared gives organization and enterprise pools a persistent cache.
	// Those pools share one repo_url across every member repository, so enabling
	// this treats the whole organization as one trust domain.
	AllowOrgShared bool `toml:"allow_org_shared"`

	// Ages in days, both >= 0: when the GC logs a volume as stale, and when
	// diagnostic files are deleted.
	StaleCacheEvictionDays     int `toml:"stale_cache_eviction_days"`
	DiagnosticLogRetentionDays int `toml:"diagnostic_log_retention_days"`
}

// defaultCache is applied before decoding, so an explicit key, including
// enabled = false, overrides it.
func defaultCache() Cache {
	return Cache{
		Enabled:                    true,
		Generation:                 defaultCacheGeneration,
		PnpmMajor:                  defaultCachePnpmMajor,
		ToolcachePath:              defaultToolcachePath,
		PnpmStorePath:              defaultPnpmStorePath,
		AllowOrgShared:             false,
		StaleCacheEvictionDays:     defaultStaleCacheEvictionDays,
		DiagnosticLogRetentionDays: defaultDiagnosticLogRetentionDays,
	}
}

// validate always checks the retention windows. The salts and paths are checked
// only when the cache is enabled, because a disabled cache never uses them.
func (c Cache) validate() error {
	if c.StaleCacheEvictionDays < 0 {
		return fmt.Errorf("stale_cache_eviction_days must be >= 0, got %d", c.StaleCacheEvictionDays)
	}
	if c.DiagnosticLogRetentionDays < 0 {
		return fmt.Errorf("diagnostic_log_retention_days must be >= 0, got %d", c.DiagnosticLogRetentionDays)
	}
	if !c.Enabled {
		return nil
	}

	if !cacheSegmentPattern.MatchString(c.Generation) {
		return fmt.Errorf("generation %q is invalid: it is embedded in a Docker volume name and must match %s", c.Generation, cacheSegmentPattern.String())
	}
	if !cacheSegmentPattern.MatchString(c.PnpmMajor) {
		return fmt.Errorf("pnpm_major %q is invalid: it is embedded in a Docker volume name and must match %s", c.PnpmMajor, cacheSegmentPattern.String())
	}
	if err := validateCacheMountPath("toolcache_path", c.ToolcachePath); err != nil {
		return err
	}
	if err := validateCacheMountPath("pnpm_store_path", c.PnpmStorePath); err != nil {
		return err
	}
	if c.ToolcachePath == c.PnpmStorePath {
		return fmt.Errorf("toolcache_path and pnpm_store_path must differ (both are %q)", c.ToolcachePath)
	}
	return nil
}

// validateCacheMountPath requires an absolute, canonical path that does not
// collide with a reserved mount. Docker mounts at the path the kernel resolves,
// so a spelling such as "/actions-runner/." would slip past a prefix check and
// is rejected.
func validateCacheMountPath(field, raw string) error {
	if raw == "" {
		return fmt.Errorf("%s is required when the cache is enabled", field)
	}
	if !strings.HasPrefix(raw, "/") {
		return fmt.Errorf("%s %q must be an absolute path", field, raw)
	}
	cleaned := path.Clean(raw)
	if cleaned != raw {
		return fmt.Errorf("%s %q is not canonical (it resolves to %q); configure the canonical path so it can be checked against the reserved in-runner mounts", field, raw, cleaned)
	}
	if cleaned == "/" {
		return fmt.Errorf("%s must not be the filesystem root", field)
	}
	// /var/run is a symlink to /run on the runner base image, so a path under it
	// lands on the DinD socket dir or the credential tmpfs.
	canon := canonicalizeVarRun(cleaned)
	for _, r := range reservedMountPaths {
		if pathIsAtOrUnder(canon, r) || pathIsAtOrUnder(r, canon) {
			return fmt.Errorf("%s %q resolves to %q, which collides with the reserved in-runner mount %q (runner install dir / credential tmpfs / DinD socket / workspace / read-only externals / diag-logs / daemon data root)", field, raw, canon, r)
		}
	}
	return nil
}

// canonicalizeVarRun rewrites a cleaned path under /var/run to its /run target.
func canonicalizeVarRun(p string) string {
	switch {
	case p == "/var/run":
		return "/run"
	case strings.HasPrefix(p, "/var/run/"):
		return "/run" + strings.TrimPrefix(p, "/var/run")
	default:
		return p
	}
}

// pathIsAtOrUnder reports whether the cleaned path a is b or nested under it.
func pathIsAtOrUnder(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/")
}
