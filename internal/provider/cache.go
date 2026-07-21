package provider

import (
	"context"
	"fmt"
	"log"
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
}

// referencedCacheVolumeNames returns the non-empty persistent cache volume names
// this plan mounts into the runner. The create path revalidates each of these
// AFTER ContainerCreate (H3): a concurrent GC could have evicted a still-current
// cache in the ensure→mount gap, and real Moby then auto-creates the missing named
// volume UNLABELED during ContainerCreate — an empty externals tree plus an orphan.
func (c cachePlan) referencedCacheVolumeNames() []string {
	var names []string
	for _, n := range []string{c.toolcacheVolume, c.pnpmVolume, c.diagVolume, c.externalsVolume} {
		if n != "" {
			names = append(names, n)
		}
	}
	return names
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
		log.Printf("garm-provider-docker: CreateInstance: %q is %s-scoped (allow_org_shared=%v); withholding persistent caches. RUNNER_TOOL_CACHE=%s is an ephemeral in-container toolcache.",
			bootstrap.Name, scope, p.cfg.Cache.AllowOrgShared, plan.toolcachePath)
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
	toolRes, err := p.topo.EnsureCacheVolume(ctx, toolName, id.ToolcacheLabels(p.cfg.Cache.Generation, lastUsed))
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to ensure toolcache volume for %q: %w", bootstrap.Name, err)
	}
	plan.toolcacheVolume = toolRes.Name

	pnpmName := spec.PnpmVolumeName(repoKey, p.cfg.Cache.PnpmMajor)
	if err := spec.ValidateDerivedName("pnpm store cache volume", pnpmName); err != nil {
		return cachePlan{}, err
	}
	pnpmRes, err := p.topo.EnsureCacheVolume(ctx, pnpmName, id.PnpmLabels(p.cfg.Cache.PnpmMajor, lastUsed))
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to ensure pnpm store volume for %q: %w", bootstrap.Name, err)
	}
	plan.pnpmVolume = pnpmRes.Name
	plan.pnpmPath = p.cfg.Cache.PnpmStorePath

	// Per-repo diagnostic-logs volume (ADR-003 W2): same repo-scope eligibility as
	// the toolcache/pnpm volumes. Its file-level retention is pruned provider-side
	// during the opportunistic GC, never by the untrusted runner.
	diagName := spec.DiagVolumeName(repoKey)
	if err := spec.ValidateDerivedName("diag cache volume", diagName); err != nil {
		return cachePlan{}, err
	}
	diagRes, err := p.topo.EnsureCacheVolume(ctx, diagName, id.DiagLabels(lastUsed))
	if err != nil {
		return cachePlan{}, fmt.Errorf("failed to ensure diag volume for %q: %w", bootstrap.Name, err)
	}
	plan.diagVolume = diagRes.Name
	plan.diagDir = spec.RunnerDiagDir

	log.Printf("garm-provider-docker: CreateInstance: %q repo-scoped caches (repokey=%s): toolcache=%s hit=%v, pnpm=%s hit=%v, diag=%s hit=%v",
		bootstrap.Name, repoKey, toolName, toolRes.Hit, pnpmName, pnpmRes.Hit, diagName, diagRes.Hit)
	return plan, nil
}
