package config

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Cache defaults (ADR-003/ADR-005). Enabled defaults to true, so an operator
// who never touches [cache] still gets persistent repo-scoped caches; the
// generation/pnpm-major salts and mount paths take the ADR-005 illustrative
// values, and allow_org_shared stays off (org/enterprise pools get no persistent
// cache without a deliberate opt-in).
const (
	defaultCacheGeneration            = "1"
	defaultCachePnpmMajor             = "9"
	defaultToolcachePath              = "/opt/hostedtoolcache"
	defaultPnpmStorePath              = "/opt/pnpm-store"
	defaultStaleCacheEvictionDays     = 30
	defaultDiagnosticLogRetentionDays = 7
)

// cacheSegmentPattern is the grammar a generation/pnpm-major salt must match:
// they are appended verbatim into a Docker volume name
// (spec.ToolcacheVolumeName/PnpmVolumeName), so each must be a legal
// Docker-resource-name fragment on its own.
var cacheSegmentPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// reservedMountPaths mirrors internal/spec's fixed in-runner mount targets that
// a cache mount must never shadow: CredentialDir (/run/garm), DindSocketDir
// (/run), the runner INSTALL dir (/actions-runner — H2: it transiently holds
// .runner/.credentials* during a non-JIT registration and is the seed source for
// externals, so a cache mounted onto it could persist credentials into a repo
// cache), RunnerWorkDir (/actions-runner/_work), DindStateDir (/var/lib/docker),
// and the fixed externals (RunnerExternalsDir) and diagnostic-logs (RunnerDiagDir)
// mount targets. It is duplicated here rather than imported to keep package config
// decoupled from package spec — the same layering choice spec/mounts.go documents
// for its own dind-mode-string duplication. A configurable
// toolcache_path/pnpm_store_path mounted at (or straddling) one of these would
// break credential delivery, the DinD socket, the runner install dir, the
// workspace, the daemon's data root, the read-only externals mount, or the
// diag-logs volume. Every entry is already canonical (path.Clean is a no-op on
// each); validateCacheMountPath canonicalizes the operator's input — and the
// /var/run→/run symlink alias — before comparing, so a non-canonical spelling
// cannot slip a mount onto any of these.
var reservedMountPaths = []string{
	"/run/garm",
	"/run",
	"/actions-runner",
	"/actions-runner/_work",
	"/actions-runner/externals",
	"/actions-runner/_diag",
	"/var/lib/docker",
}

// Cache is the [cache] table (ADR-003/ADR-005): the persistent repo-scoped
// toolcache and pnpm-store caches. Externals, diagnostic logs, and opportunistic
// GC (which consume stale_cache_eviction_days/diagnostic_log_retention_days) are
// M2-W2 — those two retention fields are parsed and range-validated here for
// forward-compatibility with ADR-005's canonical config block, but their
// behavior is not wired in this work package.
type Cache struct {
	// Enabled toggles the whole persistent-cache feature. Default true. When
	// false, CreateInstance provisions no cache volumes and sets no cache env
	// (the M1 behavior).
	Enabled bool `toml:"enabled"`

	// Generation is the toolcache generation salt (spec.ToolcacheVolumeName's
	// <gen>): bumping it invalidates every repo's toolcache by moving to a
	// fresh, empty volume name, without an explicit GC pass. Default "1". Must
	// be a legal Docker-name fragment since it is embedded in the volume name.
	Generation string `toml:"generation"`

	// PnpmMajor is the pnpm store salt (spec.PnpmVolumeName's <pnpmMajor>):
	// bump it on a pnpm major-version change, since the store's on-disk layout
	// is tied to the pnpm major. Default "9". Same name-fragment constraint as
	// Generation.
	PnpmMajor string `toml:"pnpm_major"`

	// ToolcachePath is where the toolcache volume mounts inside the runner and
	// the value of RUNNER_TOOL_CACHE (research.md §3: the hosted-runner
	// convention is /opt/hostedtoolcache). Absolute path. Default
	// /opt/hostedtoolcache.
	ToolcachePath string `toml:"toolcache_path"`

	// PnpmStorePath is where the pnpm store volume mounts inside the runner and
	// the value of npm_config_store_dir, which pnpm honors as its store-dir.
	// RESOLVED for M2 (ADR-003 open question): a named volume mounted at a fixed
	// path plus npm_config_store_dir pointing pnpm at it — verified on Docker
	// Engine 29.6.1 (`pnpm config get store-dir` and `pnpm store path` resolve
	// to the volume, and package content is reused across containers sharing it,
	// "downloaded 0"). Absolute path. Default /opt/pnpm-store.
	PnpmStorePath string `toml:"pnpm_store_path"`

	// AllowOrgShared is the org/enterprise opt-in (ADR-003). Default false: an
	// org- or enterprise-scoped pool gets NO persistent cache, because repo_url
	// is shared across every member repository and keying on it would silently
	// pool them into one cross-repo cache. Setting this true opts into
	// org/enterprise-keyed shared caches, explicitly treating the whole
	// org/enterprise as ONE trust domain for cache purposes.
	AllowOrgShared bool `toml:"allow_org_shared"`

	// StaleCacheEvictionDays and DiagnosticLogRetentionDays are ADR-003's GC/
	// log-retention windows. M2-W2 consumes them; W1 only parses and
	// range-validates (non-negative) so an operator's ADR-005-shaped config
	// loads and a typo (a negative window) is caught rather than silently
	// ignored. Defaults 30 / 7.
	StaleCacheEvictionDays     int `toml:"stale_cache_eviction_days"`
	DiagnosticLogRetentionDays int `toml:"diagnostic_log_retention_days"`
}

// defaultCache returns the [cache] defaults applied by Load before decoding the
// TOML file, so an omitted key keeps its default while an explicitly-set key
// (including enabled = false) overrides it.
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

// validate checks the [cache] table. The retention windows are always
// range-checked; the generation/pnpm-major salts and mount paths are enforced
// only when the cache is Enabled (a disabled cache is inert, so its salts/paths
// do not have to be well-formed — matching how a "none"-mode dind_image is only
// required when a sidecar would actually be created).
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

// validateCacheMountPath requires an absolute, CANONICAL path that does not
// collide (at, under, or over) with any reserved in-runner mount target.
//
// H2: raw-string validation was bypassable via canonical aliases — Docker mounts
// at the path the kernel canonicalizes to, not the literal string, so
// "/actions-runner/." , "/opt/../actions-runner", "//run//garm", or the
// /var/run→/run symlink would let a cache land on the runner install dir, the
// credential tmpfs, or the DinD socket while slipping past a raw-string ancestor
// check. This closes it by: (1) rejecting any NON-canonical spelling
// (path.Clean(raw) != raw catches dot-segments, repeated slashes, and trailing
// slashes); (2) rejecting the filesystem root; (3) canonicalizing the /var/run
// alias to /run BEFORE comparing; and (4) comparing CLEANED ancestor
// relationships against the (canonical) reserved list.
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
	// /var/run is a symlink to /run (verified on the runner base image:
	// /var/run -> /run), so a cache configured at /var/run[/...] actually lands
	// on /run[/...] — the DinD socket dir or the credential tmpfs (/run/garm).
	// Canonicalize the alias before the ancestor check so it cannot smuggle a
	// mount onto either.
	canon := canonicalizeVarRun(cleaned)
	for _, r := range reservedMountPaths {
		if pathIsAtOrUnder(canon, r) || pathIsAtOrUnder(r, canon) {
			return fmt.Errorf("%s %q resolves to %q, which collides with the reserved in-runner mount %q (runner install dir / credential tmpfs / DinD socket / workspace / read-only externals / diag-logs / daemon data root)", field, raw, canon, r)
		}
	}
	return nil
}

// canonicalizeVarRun rewrites the /var/run→/run symlink alias so a cache path
// spelled under /var/run is checked against the reserved list as its real /run
// target. It operates on an already-path.Clean'd absolute path.
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

// pathIsAtOrUnder reports whether canonical path a IS b or is nested UNDER b
// (b is an ancestor directory of a). Both must already be path.Clean'd.
func pathIsAtOrUnder(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/")
}
