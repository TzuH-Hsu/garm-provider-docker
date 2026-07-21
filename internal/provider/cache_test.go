package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// newCacheProvider builds a Provider with the persistent cache feature enabled.
func newCacheProvider(t *testing.T, allowOrgShared bool) (*Provider, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		Network:          config.Network{EnableJobNetwork: true, Internal: false},
		Cache: config.Cache{
			Enabled:        true,
			Generation:     "1",
			PnpmMajor:      "9",
			ToolcachePath:  "/opt/hostedtoolcache",
			PnpmStorePath:  "/opt/pnpm-store",
			AllowOrgShared: allowOrgShared,
		},
	}
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}
	return p, fake
}

func cacheBootstrap(name, repoURL, metadataURL string) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             name,
		RepoURL:          repoURL,
		MetadataURL:      metadataURL,
		InstanceToken:    testInstanceToken,
		OSType:           params.Linux,
		OSArch:           params.Amd64,
		PoolID:           "pool-xyz",
		JitConfigEnabled: true,
	}
}

func mountAt(c types.ContainerJSON, dest string) (types.MountPoint, bool) {
	for _, m := range c.Mounts {
		if m.Destination == dest {
			return m, true
		}
	}
	return types.MountPoint{}, false
}

func envHas(c types.ContainerJSON, kv string) bool {
	return c.Config != nil && slices.Contains(c.Config.Env, kv)
}

func envHasPrefix(c types.ContainerJSON, prefix string) bool {
	if c.Config == nil {
		return false
	}
	for _, e := range c.Config.Env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func countCacheVols(t *testing.T, fake *docker.FakeClient) int {
	t.Helper()
	out, err := fake.VolumeList(context.Background(), volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", spec.LabelCache+"=true")),
	})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	return len(out.Volumes)
}

const cacheRepoURL = "https://github.com/example-org/example-repo"

// TestCreateInstanceMountsRepoCaches: a repo-scoped pool gets the toolcache and
// pnpm store volumes created, mounted at their configured paths, and the cache
// env set. The cache volumes carry cache=true and NO instance-name.
func TestCreateInstanceMountsRepoCaches(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)

	if _, err := p.CreateInstance(context.Background(), cacheBootstrap("job-1", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	repoKey := spec.RepoKey(cacheRepoURL)
	toolName := spec.ToolcacheVolumeName(repoKey, "1")
	pnpmName := spec.PnpmVolumeName(repoKey, "9")

	tv, ok := volByName(t, fake, toolName)
	if !ok {
		t.Fatalf("toolcache volume %q was not created", toolName)
	}
	if tv.Labels[spec.LabelCache] != "true" || tv.Labels[spec.LabelRepo] != repoKey || tv.Labels[spec.LabelGeneration] != "1" {
		t.Errorf("toolcache labels missing/incorrect: %v", tv.Labels)
	}
	if _, has := tv.Labels[spec.LabelInstanceName]; has {
		t.Error("toolcache volume must NOT carry an instance-name label")
	}
	if _, ok := volByName(t, fake, pnpmName); !ok {
		t.Fatalf("pnpm store volume %q was not created", pnpmName)
	}

	c := inspectRunner(t, fake, "job-1")
	if m, ok := mountAt(c, "/opt/hostedtoolcache"); !ok || m.Name != toolName {
		t.Errorf("toolcache not mounted at /opt/hostedtoolcache (got %+v)", m)
	}
	if m, ok := mountAt(c, "/opt/pnpm-store"); !ok || m.Name != pnpmName {
		t.Errorf("pnpm store not mounted at /opt/pnpm-store (got %+v)", m)
	}
	if !envHas(c, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
		t.Errorf("RUNNER_TOOL_CACHE not set: %v", c.Config.Env)
	}
	if !envHas(c, "npm_config_store_dir=/opt/pnpm-store") {
		t.Errorf("npm_config_store_dir not set: %v", c.Config.Env)
	}
}

// TestCreateInstanceCacheHitAcrossAllocationsSurvivesDelete: two allocations for
// the SAME repo_url mount the SAME single cache volumes (the cache hit), and
// deleting one allocation does NOT tear the caches down.
func TestCreateInstanceCacheHitAcrossAllocationsSurvivesDelete(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)
	ctx := context.Background()

	repoKey := spec.RepoKey(cacheRepoURL)
	toolName := spec.ToolcacheVolumeName(repoKey, "1")
	pnpmName := spec.PnpmVolumeName(repoKey, "9")

	if _, err := p.CreateInstance(ctx, cacheBootstrap("job-1", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance job-1: %v", err)
	}
	if _, err := p.CreateInstance(ctx, cacheBootstrap("job-2", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance job-2: %v", err)
	}

	// Exactly two cache volumes total (one toolcache + one pnpm) across BOTH
	// jobs — the second job reused the first's, it did not create its own.
	if n := countCacheVols(t, fake); n != 2 {
		t.Errorf("cache volume count = %d after two allocations for the same repo, want 2 (toolcache + pnpm, reused)", n)
	}
	// Both runners mount the SAME cache volume names.
	c1 := inspectRunner(t, fake, "job-1")
	c2 := inspectRunner(t, fake, "job-2")
	for _, c := range []types.ContainerJSON{c1, c2} {
		if m, ok := mountAt(c, "/opt/hostedtoolcache"); !ok || m.Name != toolName {
			t.Errorf("job did not mount the shared toolcache %q (got %+v)", toolName, m)
		}
		if m, ok := mountAt(c, "/opt/pnpm-store"); !ok || m.Name != pnpmName {
			t.Errorf("job did not mount the shared pnpm store %q (got %+v)", pnpmName, m)
		}
	}

	// Delete job-1: its allocation is torn down, but the caches SURVIVE (they
	// carry no instance-name, so the teardown predicate never matches them).
	if err := p.DeleteInstance(ctx, "job-1"); err != nil {
		t.Fatalf("DeleteInstance job-1: %v", err)
	}
	if _, ok := volByName(t, fake, toolName); !ok {
		t.Error("toolcache volume was removed by DeleteInstance — caches must outlive allocations")
	}
	if _, ok := volByName(t, fake, pnpmName); !ok {
		t.Error("pnpm store volume was removed by DeleteInstance — caches must outlive allocations")
	}
	if n := countCacheVols(t, fake); n != 2 {
		t.Errorf("cache volume count = %d after deleting one allocation, want 2 (both survive)", n)
	}
}

// TestCreateInstanceOrgScopeWithholdsCaches: an org-scoped pool WITHOUT
// allow_org_shared gets no persistent cache volumes and no persistent cache
// mounts, but RUNNER_TOOL_CACHE is still set (ephemeral, in-container) and
// npm_config_store_dir is absent.
func TestCreateInstanceOrgScopeWithholdsCaches(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false) // allow_org_shared = false

	if _, err := p.CreateInstance(context.Background(), cacheBootstrap("job-org", "https://github.com/example-org", srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	if n := countCacheVols(t, fake); n != 0 {
		t.Errorf("org-scoped pool created %d cache volumes without allow_org_shared, want 0", n)
	}
	c := inspectRunner(t, fake, "job-org")
	if _, ok := mountAt(c, "/opt/hostedtoolcache"); ok {
		t.Error("org-scoped pool mounted a persistent toolcache volume without opt-in")
	}
	if _, ok := mountAt(c, "/opt/pnpm-store"); ok {
		t.Error("org-scoped pool mounted a persistent pnpm store without opt-in")
	}
	// RUNNER_TOOL_CACHE is still set (ephemeral); npm_config_store_dir is not.
	if !envHas(c, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
		t.Error("RUNNER_TOOL_CACHE should still be set (ephemeral toolcache) for a cache-ineligible pool")
	}
	if envHasPrefix(c, "npm_config_store_dir=") {
		t.Error("npm_config_store_dir must be absent when no persistent pnpm store is mounted")
	}
}

// TestCreateInstanceOrgScopeOptInGetsCaches: with allow_org_shared, an
// org-scoped pool DOES get persistent caches, keyed on the org identity.
func TestCreateInstanceOrgScopeOptInGetsCaches(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, true) // allow_org_shared = true

	orgURL := "https://github.com/example-org"
	if _, err := p.CreateInstance(context.Background(), cacheBootstrap("job-org", orgURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	orgKey := spec.RepoKey(orgURL)
	if _, ok := volByName(t, fake, spec.ToolcacheVolumeName(orgKey, "1")); !ok {
		t.Error("org-scoped pool with allow_org_shared did not get a toolcache volume")
	}
	if _, ok := volByName(t, fake, spec.PnpmVolumeName(orgKey, "9")); !ok {
		t.Error("org-scoped pool with allow_org_shared did not get a pnpm store volume")
	}
}

// TestCreateInstanceCacheDisabledNoCacheEnvOrVolumes: with the cache feature
// off (the default in newTestProvider), no cache volumes and no cache env — the
// M1 behavior is preserved.
func TestCreateInstanceCacheDisabledNoCacheEnvOrVolumes(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newTestProvider(t) // Cache zero-value: disabled

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if n := countCacheVols(t, fake); n != 0 {
		t.Errorf("cache-disabled config created %d cache volumes, want 0", n)
	}
	c := inspectRunner(t, fake, "Test-Instance-01")
	if envHasPrefix(c, "RUNNER_TOOL_CACHE=") {
		t.Error("RUNNER_TOOL_CACHE must be absent when the cache feature is disabled")
	}
	if envHasPrefix(c, "npm_config_store_dir=") {
		t.Error("npm_config_store_dir must be absent when the cache feature is disabled")
	}
}
