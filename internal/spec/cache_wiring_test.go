package spec

import (
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

// mountForTarget returns the mount whose Target is target, or a zero Mount.
func mountForTarget(mounts []mount.Mount, target string) (mount.Mount, bool) {
	for _, m := range mounts {
		if m.Target == target {
			return m, true
		}
	}
	return mount.Mount{}, false
}

// TestBuildRunnerContainerCacheMounts: toolcache and pnpm cache volumes are
// mounted read-write at their configured paths when set, and not otherwise.
func TestBuildRunnerContainerCacheMounts(t *testing.T) {
	_, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:               "x",
		WorkspaceVolumeName: "job-1-nonce-workspace",
		ToolcacheVolumeName: "garm-cache-toolcache-repo-abc-1",
		ToolcacheMountPath:  "/opt/hostedtoolcache",
		PnpmVolumeName:      "garm-cache-pnpm-repo-abc-9",
		PnpmMountPath:       "/opt/pnpm-store",
	})

	tool, ok := mountForTarget(host.Mounts, "/opt/hostedtoolcache")
	if !ok {
		t.Fatal("toolcache volume not mounted at /opt/hostedtoolcache")
	}
	if tool.Type != mount.TypeVolume || tool.Source != "garm-cache-toolcache-repo-abc-1" {
		t.Errorf("toolcache mount = %+v, want the named cache volume", tool)
	}
	if tool.ReadOnly {
		t.Error("toolcache mount is read-only, want read-write (setup-* actions populate it)")
	}

	pnpm, ok := mountForTarget(host.Mounts, "/opt/pnpm-store")
	if !ok {
		t.Fatal("pnpm store volume not mounted at /opt/pnpm-store")
	}
	if pnpm.Type != mount.TypeVolume || pnpm.Source != "garm-cache-pnpm-repo-abc-9" {
		t.Errorf("pnpm mount = %+v, want the named cache volume", pnpm)
	}

	// Workspace + toolcache + pnpm = 3 mounts (no DinD socket in none mode).
	if len(host.Mounts) != 3 {
		t.Errorf("Mounts = %d, want 3 (workspace + toolcache + pnpm)", len(host.Mounts))
	}
}

// TestBuildRunnerContainerNoCacheMountsWhenUnset: a cache-ineligible allocation
// (no volume names) mounts no cache volumes, even if a path is present.
func TestBuildRunnerContainerNoCacheMountsWhenUnset(t *testing.T) {
	_, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:               "x",
		WorkspaceVolumeName: "job-1-nonce-workspace",
		// A path without a volume name (the cache-ineligible case: RUNNER_TOOL_CACHE
		// is still set in env, but no persistent volume is mounted).
		ToolcacheMountPath: "/opt/hostedtoolcache",
	})
	if _, ok := mountForTarget(host.Mounts, "/opt/hostedtoolcache"); ok {
		t.Error("a toolcache path without a volume name must NOT produce a mount")
	}
	if len(host.Mounts) != 1 {
		t.Errorf("Mounts = %d, want 1 (workspace only)", len(host.Mounts))
	}
}

// TestBuildRunnerEnvCacheEnv: RUNNER_TOOL_CACHE and the pnpm store envs are
// emitted when their fields are set, in BOTH JIT and non-JIT modes. The store
// path is emitted under both npm_config_store_dir (pnpm <= 10) and
// pnpm_config_store_dir (pnpm >= 11): each pnpm major ignores the other's name.
func TestBuildRunnerEnvCacheEnv(t *testing.T) {
	for _, jit := range []bool{true, false} {
		env := BuildRunnerEnv(RunnerEnvOptions{
			JITConfigEnabled: jit,
			GitHubURL:        "https://github.com",
			RunnerWorkDir:    RunnerWorkDir,
			RunnerName:       "r",
			Entity:           Entity{Scope: EntityRepo, Org: "o", Repo: "r"},
			ToolCacheDir:     "/opt/hostedtoolcache",
			PnpmStoreDir:     "/opt/pnpm-store",
		})
		if !slices.Contains(env, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
			t.Errorf("jit=%v: env missing RUNNER_TOOL_CACHE: %v", jit, env)
		}
		if !slices.Contains(env, "npm_config_store_dir=/opt/pnpm-store") {
			t.Errorf("jit=%v: env missing npm_config_store_dir (pnpm <= 10): %v", jit, env)
		}
		if !slices.Contains(env, "pnpm_config_store_dir=/opt/pnpm-store") {
			t.Errorf("jit=%v: env missing pnpm_config_store_dir (pnpm >= 11): %v", jit, env)
		}
	}
}

// TestBuildRunnerEnvNoCacheEnvWhenUnset: the cache env is absent when the fields
// are empty (a cache-disabled allocation), and the pnpm env is absent when only
// the toolcache is set (a cache-ineligible allocation with an ephemeral
// toolcache path but no persistent pnpm store).
func TestBuildRunnerEnvNoCacheEnvWhenUnset(t *testing.T) {
	disabled := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: true,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    RunnerWorkDir,
	})
	for _, e := range disabled {
		if strings.HasPrefix(e, "RUNNER_TOOL_CACHE=") {
			t.Errorf("cache-disabled env unexpectedly set RUNNER_TOOL_CACHE: %q", e)
		}
		if strings.HasPrefix(e, "npm_config_store_dir=") || strings.HasPrefix(e, "pnpm_config_store_dir=") {
			t.Errorf("cache-disabled env unexpectedly set a pnpm store env: %q", e)
		}
	}

	toolOnly := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: true,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    RunnerWorkDir,
		ToolCacheDir:     "/opt/hostedtoolcache", // ephemeral toolcache, no persistent pnpm
	})
	if !slices.Contains(toolOnly, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
		t.Error("RUNNER_TOOL_CACHE should be set even for an ephemeral (cache-ineligible) toolcache")
	}
	for _, e := range toolOnly {
		if strings.HasPrefix(e, "npm_config_store_dir=") || strings.HasPrefix(e, "pnpm_config_store_dir=") {
			t.Errorf("pnpm store envs must be absent when no pnpm store volume is mounted: %q", e)
		}
	}
}

// TestBuildRunnerEnvPnpmCacheDir: the metadata cache envs ride on the pnpm
// store volume. They are emitted iff a persistent pnpm store is mounted, under
// both spellings (pnpm <= 10 reads npm_config_cache_dir, pnpm >= 11 reads
// pnpm_config_cache_dir), in JIT and non-JIT modes, and point at a
// sub-directory of the store mount.
func TestBuildRunnerEnvPnpmCacheDir(t *testing.T) {
	const store = "/opt/pnpm-store"
	const want = store + "/metadata-cache"
	for _, jit := range []bool{true, false} {
		env := BuildRunnerEnv(RunnerEnvOptions{
			JITConfigEnabled: jit,
			GitHubURL:        "https://github.com",
			RunnerWorkDir:    RunnerWorkDir,
			RunnerName:       "r",
			Entity:           Entity{Scope: EntityRepo, Org: "o", Repo: "r"},
			PnpmStoreDir:     store,
		})
		if !slices.Contains(env, "npm_config_cache_dir="+want) {
			t.Errorf("jit=%v: env missing npm_config_cache_dir=%s (pnpm <= 10): %v", jit, want, env)
		}
		if !slices.Contains(env, "pnpm_config_cache_dir="+want) {
			t.Errorf("jit=%v: env missing pnpm_config_cache_dir=%s (pnpm >= 11): %v", jit, want, env)
		}
	}
}

// TestBuildRunnerEnvNoPnpmCacheDirWithoutStore: no pnpm store volume, no cache
// dir env. A cache-ineligible allocation (toolcache only) and a cache-disabled
// one leave pnpm on its own default cache location.
func TestBuildRunnerEnvNoPnpmCacheDirWithoutStore(t *testing.T) {
	cases := map[string]RunnerEnvOptions{
		"disabled":  {JITConfigEnabled: true, GitHubURL: "https://github.com", RunnerWorkDir: RunnerWorkDir},
		"toolcache": {JITConfigEnabled: true, GitHubURL: "https://github.com", RunnerWorkDir: RunnerWorkDir, ToolCacheDir: "/opt/hostedtoolcache"},
	}
	for name, opts := range cases {
		for _, e := range BuildRunnerEnv(opts) {
			if strings.HasPrefix(e, "npm_config_cache_dir=") || strings.HasPrefix(e, "pnpm_config_cache_dir=") {
				t.Errorf("%s: pnpm cache-dir env must be absent without a pnpm store volume: %q", name, e)
			}
		}
	}
}

// TestPnpmMetadataCacheDirIsStrictlyBelowStoreRoot: the cache dir must never be
// the store mount itself. pnpm 11's `pnpm store prune` removes what it finds
// under cache-dir; with cache-dir at the store root it deleted the whole store
// in a lab run. A sub-directory limits prune to the metadata.
func TestPnpmMetadataCacheDirIsStrictlyBelowStoreRoot(t *testing.T) {
	for _, store := range []string{"/opt/pnpm-store", "/data/pnpm", "/a"} {
		got := PnpmMetadataCacheDir(store)
		if got == store {
			t.Errorf("PnpmMetadataCacheDir(%q) == store mount root", store)
		}
		if path.Dir(got) != store {
			t.Errorf("PnpmMetadataCacheDir(%q) = %q, want a direct child of the store mount", store, got)
		}
		if !strings.HasPrefix(got, store+"/") {
			t.Errorf("PnpmMetadataCacheDir(%q) = %q, not beneath the store mount", store, got)
		}
	}
	if got := PnpmMetadataCacheDir(""); got != "" {
		t.Errorf("PnpmMetadataCacheDir(empty) = %q, want empty (no store, no cache dir)", got)
	}
}
