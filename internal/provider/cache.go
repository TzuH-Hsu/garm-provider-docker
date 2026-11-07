package provider

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// cachePlan is one allocation's cache decision, from planCaches (ADR-003). Its zero
// value mounts nothing and sets no cache env.
type cachePlan struct {
	// toolcachePath is RUNNER_TOOL_CACHE and the toolcache mount target. It is set
	// whenever the cache feature is on; with an empty toolcacheVolume it is just a
	// container path.
	toolcachePath string

	// toolcacheVolume is set only for a cache-eligible allocation.
	toolcacheVolume string

	// pnpmPath is the pnpm store env value and mount target, set only with pnpmVolume.
	// When empty, pnpm uses its own default store.
	pnpmPath string

	pnpmVolume string

	// diagVolume is the per-repository diagnostic-log volume, set only when eligible.
	diagVolume string

	// diagDir is the _diag mount target and GARM_DIAG_DIR, set with diagVolume.
	diagDir string

	// externalsVolume is the shared read-only externals volume, set whenever the cache
	// feature is on, for any entity scope. planExternals resolves and seeds it.
	externalsVolume string

	// cacheRefs holds each mounted cache volume with the labels it was ensured with,
	// for revalidateReferencedCaches.
	cacheRefs []cacheVolumeRef
}

// cacheVolumeRef is a cache volume's name and its expected identity labels.
type cacheVolumeRef struct {
	name string
	want map[string]string
}

// addCacheRef records a cache volume and its expected labels, keeping a copy of want.
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

// planCaches resolves one allocation's cache decision and, when it is eligible, creates
// or reuses its toolcache, pnpm store and diagnostic-log volumes (ADR-003, Keys and
// scope). An organization or enterprise pool without allow_org_shared, or a URL that
// cannot be classified, gets no persistent volume: RUNNER_TOOL_CACHE then points inside
// the container. runnerImage must be present locally.
func (p *Provider) planCaches(ctx context.Context, bootstrap params.BootstrapInstance, runnerImage string) (cachePlan, error) {
	if !p.cfg.Cache.Enabled {
		return cachePlan{}, nil
	}

	plan := cachePlan{toolcachePath: p.cfg.Cache.ToolcachePath}

	scope := spec.DetectCacheEntityScope(bootstrap.RepoURL)
	if !spec.CacheScopeAllowed(scope, p.cfg.Cache.AllowOrgShared) {
		slog.InfoContext(ctx, "CreateInstance: entity scope withholds persistent caches; using an ephemeral in-container toolcache",
			"instance", bootstrap.Name, "scope", scope, "allow_org_shared", p.cfg.Cache.AllowOrgShared, "toolcache_path", plan.toolcachePath)
		return plan, nil
	}

	osKey, err := p.runnerOSKey(ctx, runnerImage)
	if err != nil {
		return cachePlan{}, err
	}

	repoKey := spec.RepoKey(bootstrap.RepoURL)
	id := spec.CacheVolumeIdentity{
		ControllerID:  p.controllerID,
		RepoKey:       repoKey,
		RepoURLDigest: spec.RepoURLDigest(bootstrap.RepoURL),
		OSKey:         osKey,
	}
	// One timestamp for the last-used labels of all three volumes.
	lastUsed := time.Now()

	toolName := spec.ToolcacheVolumeName(repoKey, osKey, p.cfg.Cache.Generation)
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

	pnpmName := spec.PnpmVolumeName(repoKey, osKey, p.cfg.Cache.PnpmMajor)
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

	// The provider prunes these logs during GC; the runner, which a job can reach,
	// never does.
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
		"instance", bootstrap.Name, "repo_key", repoKey, "os_key", osKey,
		"toolcache_volume", toolName, "toolcache_hit", toolRes.Hit,
		"pnpm_volume", pnpmName, "pnpm_hit", pnpmRes.Hit,
		"diag_volume", diagName, "diag_hit", diagRes.Hit)
	return plan, nil
}

// runnerOSKey reads the OS key of the locally present runner image: its
// garm.docker/os-release label, or a key derived from its content ID when the
// label is absent (spec.RunnerOSKey). A malformed label fails the allocation
// rather than guessing which caches the image may share.
func (p *Provider) runnerOSKey(ctx context.Context, runnerImage string) (string, error) {
	insp, _, err := p.cli.ImageInspectWithRaw(ctx, runnerImage)
	if err != nil {
		return "", fmt.Errorf("failed to inspect runner image %q for its OS release: %w", runnerImage, err)
	}
	var labels map[string]string
	if insp.Config != nil {
		labels = insp.Config.Labels
	}
	key, err := spec.RunnerOSKey(labels, insp.ID)
	if err != nil {
		return "", fmt.Errorf("runner image %q: %w", runnerImage, err)
	}
	if _, labelled := labels[spec.LabelOSRelease]; !labelled {
		// Correct but costly: every rebuild of this image starts its toolcache
		// and pnpm store cold. Say so, so an operator can add the label.
		slog.InfoContext(ctx, "CreateInstance: runner image has no OS release label; keying its toolcache and pnpm store on the image digest (set the label to keep them warm across rebuilds)",
			"image", runnerImage, "label", spec.LabelOSRelease, "os_key", key)
	}
	return key, nil
}
