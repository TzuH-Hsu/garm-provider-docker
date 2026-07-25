package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// cachePlan is the resolved per-allocation cache decision (ADR-003), produced
// by planCaches and consumed by CreateInstance to shape the runner container's
// cache mounts and cache env. A zero cachePlan (the cache-disabled case) mounts
// nothing and sets no cache env — the M1 behavior.
type cachePlan struct {
	// toolcachePath is the RUNNER_TOOL_CACHE value AND the toolcache mount
	// target. It is set whenever the cache feature is enabled — even for a
	// cache-INELIGIBLE allocation, giving the runner a consistent toolcache
	// location (merely an ephemeral in-container one, since toolcacheVolume is
	// then empty and no persistent volume is mounted). Empty when the cache
	// feature is disabled entirely.
	toolcachePath string

	// toolcacheVolume is the persistent toolcache volume's name, set ONLY when
	// this allocation is cache-eligible. Empty means no persistent toolcache
	// mount is added (BuildRunnerContainer mounts only when BOTH the name and
	// the path are non-empty).
	toolcacheVolume string

	// pnpmPath is the npm_config_store_dir value AND the pnpm mount target, set
	// ONLY when a persistent pnpm store volume is mounted (cache-eligible).
	// Empty means pnpm uses its own default store and no npm_config_store_dir is
	// injected.
	pnpmPath string

	// pnpmVolume is the persistent pnpm store volume's name, set alongside
	// pnpmPath when cache-eligible.
	pnpmVolume string

	// diagVolume is the per-repo diagnostic-logs volume's name (ADR-003 W2), set
	// ONLY when this allocation is cache-eligible (same repo-scope rule as the
	// toolcache/pnpm volumes). Empty means no persistent diag mount is added.
	diagVolume string

	// diagDir is the runner's _diag mount target AND the GARM_DIAG_DIR value the
	// entrypoint uses to own the mounted dir, set alongside diagVolume.
	diagDir string

	// externalsVolume is the shared, image-digest-keyed externals volume's name
	// (ADR-003 W2), set whenever the cache feature is enabled — for ANY entity
	// scope, since externals carry no repo data. It is resolved and SEEDED
	// separately from planCaches (planExternals), after the runner image is
	// present, and mounted READ-ONLY.
	externalsVolume string

	// cacheRefs pairs every persistent cache volume this plan mounts into the
	// runner with the FULL identity labels the provider ensured it with (NEW-H1 /
	// H3b). revalidateReferencedCaches re-inspects each AFTER ContainerCreate and
	// validates the LIVE volume's labels still match this expected identity via
	// spec.ValidateAdoptedCacheVolume — not merely cache=true — so an UNLABELED
	// auto-created replacement (a manual-purge/create race — the provider's own
	// GC never removes a cache volume itself), a foreign same-name squatter, or
	// a wrong-digest externals volume is caught and the allocation fails CLOSED.
	// Carrying the want labels here (rather than re-deriving them at revalidation)
	// keeps the expected identity single-sourced from the ensure call.
	cacheRefs []cacheVolumeRef
}

// cacheVolumeRef pairs a referenced cache volume's name with the identity labels
// the provider ensured it with, so revalidateReferencedCaches can re-validate the
// LIVE volume's full identity at the destructive boundary (NEW-H1 / H3b).
type cacheVolumeRef struct {
	name string
	want map[string]string
}

// addCacheRef records a referenced cache volume and the identity labels it was
// ensured with, for the post-create revalidation (NEW-H1 / H3b). A cloned copy of
// the labels is stored so a later mutation of the caller's map cannot alter the
// expected identity.
func (c *cachePlan) addCacheRef(name string, want map[string]string) {
	if name == "" {
		return
	}
	cp := make(map[string]string, len(want))
	for k, v := range want {
		cp[k] = v
	}
	c.cacheRefs = append(c.cacheRefs, cacheVolumeRef{name: name, want: cp})
}

// planCaches resolves ADR-003's per-allocation cache decision and, when the
// allocation is cache-eligible, create-or-reuses the toolcache and pnpm store
// volumes for the repo. It returns the plan CreateInstance threads into the
// runner container's cache mounts (spec.RunnerContainerSpec) and cache env
// (buildRunnerEnv).
//
// Eligibility (ADR-003, via spec.DetectCacheEntityScope + CacheScopeAllowed):
//   - repo-scoped pool                      -> persistent toolcache + pnpm store;
//   - org/enterprise pool, allow_org_shared -> persistent caches keyed on the
//     shared org/enterprise identity (the operator's explicit single-trust-domain
//     opt-in);
//   - org/enterprise pool, default          -> NO persistent cache (repo_url is
//     shared across the whole org, so keying on it would silently pool every
//     member repo into one cross-repo cache);
//   - unparseable repo_url (unknown scope)  -> NO persistent cache (fail-safe).
//
// In the no-persistent-cache cases RUNNER_TOOL_CACHE is still set to the
// configured toolcache_path (an ephemeral in-container toolcache), so the runner
// behaves consistently; only the persistent VOLUME is withheld.
//
// The cache volumes it creates carry no instance-name and no create-nonce, so
// the creation-guard rollback (nonce+instance-name-scoped) never removes them:
// a create that fails AFTER planCaches leaves the warm cache intact for the next
// job, which is exactly what a cache is for. planCaches is therefore called
// inside CreateInstance's guarded region for its own error handling (a
// cache-volume create failure fails the allocation and rolls back the
// allocation's OWN resources), but the caches themselves are never rolled back.
func (p *Provider) planCaches(ctx context.Context, bootstrap params.BootstrapInstance) (cachePlan, error) {
	if !p.cfg.Cache.Enabled {
		return cachePlan{}, nil
	}

	// RUNNER_TOOL_CACHE / the toolcache mount target is the configured path in
	// every enabled case; only the persistent volume backing it is conditional.
	plan := cachePlan{toolcachePath: p.cfg.Cache.ToolcachePath}

	scope := spec.DetectCacheEntityScope(bootstrap.RepoURL)
	if !spec.CacheScopeAllowed(scope, p.cfg.Cache.AllowOrgShared) {
		slog.InfoContext(ctx, "CreateInstance: entity scope withholds persistent caches; using an ephemeral in-container toolcache",
			"instance", bootstrap.Name, "scope", scope, "allow_org_shared", p.cfg.Cache.AllowOrgShared, "toolcache_path", plan.toolcachePath)
		return plan, nil
	}

	repoKey := spec.RepoKey(bootstrap.RepoURL)
	id := spec.CacheVolumeIdentity{
		ControllerID:  p.controllerID,
		RepoKey:       repoKey,
		RepoURLDigest: spec.RepoURLDigest(bootstrap.RepoURL),
	}
	// One creation timestamp for both volumes' last-used labels. time.Now() is
	// here (the impure provider layer), not in the pure spec builders.
	lastUsed := time.Now()

	toolName := spec.ToolcacheVolumeName(repoKey, p.cfg.Cache.Generation)
	if err := spec.ValidateDerivedName("toolcache cache volume", toolName); err != nil {
		return cachePlan{}, err
	}
	toolLabels := id.ToolcacheLabels(p.cfg.Cache.Generation, lastUsed)
	toolRes, err := p.topo.EnsureCacheVolume(ctx, toolName, toolLabels)
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to ensure toolcache volume for %q: %w", bootstrap.Name, err)
	}
	plan.toolcacheVolume = toolRes.Name
	plan.addCacheRef(toolRes.Name, toolLabels)

	pnpmName := spec.PnpmVolumeName(repoKey, p.cfg.Cache.PnpmMajor)
	if err := spec.ValidateDerivedName("pnpm store cache volume", pnpmName); err != nil {
		return cachePlan{}, err
	}
	pnpmLabels := id.PnpmLabels(p.cfg.Cache.PnpmMajor, lastUsed)
	pnpmRes, err := p.topo.EnsureCacheVolume(ctx, pnpmName, pnpmLabels)
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to ensure pnpm store volume for %q: %w", bootstrap.Name, err)
	}
	plan.pnpmVolume = pnpmRes.Name
	plan.pnpmPath = p.cfg.Cache.PnpmStorePath
	plan.addCacheRef(pnpmRes.Name, pnpmLabels)

	// Per-repo diagnostic-logs volume (ADR-003 W2): same repo-scope eligibility as
	// the toolcache/pnpm volumes. Its file-level retention is pruned provider-side
	// during the opportunistic GC, never by the untrusted runner.
	diagName := spec.DiagVolumeName(repoKey)
	if err := spec.ValidateDerivedName("diag cache volume", diagName); err != nil {
		return cachePlan{}, err
	}
	diagLabels := id.DiagLabels(lastUsed)
	diagRes, err := p.topo.EnsureCacheVolume(ctx, diagName, diagLabels)
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to ensure diag volume for %q: %w", bootstrap.Name, err)
	}
	plan.diagVolume = diagRes.Name
	plan.diagDir = spec.RunnerDiagDir
	plan.addCacheRef(diagRes.Name, diagLabels)

	slog.InfoContext(ctx, "CreateInstance: repo-scoped caches resolved",
		"instance", bootstrap.Name, "repo_key", repoKey,
		"toolcache_volume", toolName, "toolcache_hit", toolRes.Hit,
		"pnpm_volume", pnpmName, "pnpm_hit", pnpmRes.Hit,
		"diag_volume", diagName, "diag_hit", diagRes.Hit)
	return plan, nil
}
