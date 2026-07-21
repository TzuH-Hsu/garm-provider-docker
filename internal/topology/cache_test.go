package topology

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const testRepoKey = "octo-org-octo-repo-0123456789ab"

func cacheIdentity() spec.CacheVolumeIdentity {
	return spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: testRepoKey}
}

// cacheVolumeByName returns a cache volume's labels and whether it is present,
// discovered by the cache=true label filter (as W2's GC/purge will discover it).
func cacheVolumeByName(t *testing.T, fake *docker.FakeClient, name string) (map[string]string, bool) {
	t.Helper()
	for _, v := range listCacheVolumes(t, fake) {
		if v != nil && v.Name == name {
			return v.Labels, true
		}
	}
	return nil, false
}

func listCacheVolumes(t *testing.T, fake *docker.FakeClient) []*volume.Volume {
	t.Helper()
	list, err := fake.VolumeList(context.Background(), volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", spec.LabelCache+"=true")),
	})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	return list.Volumes
}

func countCacheVolumes(t *testing.T, fake *docker.FakeClient) int {
	t.Helper()
	return len(listCacheVolumes(t, fake))
}

// TestEnsureCacheVolumeMissThenHit is the core create-or-reuse contract: the
// first call MISSES (fresh, empty) and the second call for the SAME name HITS
// (reuse), with exactly ONE volume and its ORIGINAL contents/labels preserved —
// never removed-and-recreated (that would discard the cache).
func TestEnsureCacheVolumeMissThenHit(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	t1 := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)

	// First allocation: MISS.
	res1, err := m.EnsureCacheVolume(ctx, name, cacheIdentity().ToolcacheLabels("1", t1))
	if err != nil {
		t.Fatalf("EnsureCacheVolume (first) returned error: %v", err)
	}
	if res1.Hit {
		t.Error("first EnsureCacheVolume reported a HIT, want a MISS (fresh create)")
	}

	labels1, ok := cacheVolumeByName(t, fake, name)
	if !ok {
		t.Fatal("toolcache volume was not created")
	}
	if labels1[spec.LabelLastUsed] != t1.UTC().Format(time.RFC3339) {
		t.Errorf("created last-used = %q, want %q", labels1[spec.LabelLastUsed], t1.UTC().Format(time.RFC3339))
	}

	// Second allocation for the SAME repo/generation: HIT.
	res2, err := m.EnsureCacheVolume(ctx, name, cacheIdentity().ToolcacheLabels("1", t2))
	if err != nil {
		t.Fatalf("EnsureCacheVolume (second) returned error: %v", err)
	}
	if !res2.Hit {
		t.Error("second EnsureCacheVolume for the same name reported a MISS, want a HIT (reuse)")
	}

	// Still exactly one cache volume of that name, and its ORIGINAL last-used is
	// preserved (the daemon does not mutate a local volume's labels; the reuse
	// did NOT remove-and-recreate, which is what preserves the cache contents).
	labels2, ok := cacheVolumeByName(t, fake, name)
	if !ok {
		t.Fatal("toolcache volume disappeared after reuse")
	}
	if labels2[spec.LabelLastUsed] != t1.UTC().Format(time.RFC3339) {
		t.Errorf("after reuse last-used = %q, want the ORIGINAL %q preserved (label immutability; data-preserving reuse)",
			labels2[spec.LabelLastUsed], t1.UTC().Format(time.RFC3339))
	}
	if n := countCacheVolumes(t, fake); n != 1 {
		t.Errorf("cache volume count after reuse = %d, want exactly 1 (reuse must not duplicate)", n)
	}
}

// TestEnsureCacheVolumeRejectsForeignSquatter is the M6 fail-closed guard: a
// FOREIGN volume that already holds the deterministic cache name (no cache
// labels) must NOT be adopted and mounted as this repo's cache. VolumeCreate is
// idempotent on a duplicate name and returns the existing (foreign) volume with
// its ORIGINAL labels, so without validation the provider would silently mount
// someone else's data (or, for externals, run preexisting content).
func TestEnsureCacheVolumeRejectsForeignSquatter(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	// A foreign volume squats the name with no managed/cache labels.
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{"someone": "else"}}); err != nil {
		t.Fatalf("seed foreign volume: %v", err)
	}

	_, err := m.EnsureCacheVolume(ctx, name, cacheIdentity().ToolcacheLabels("1", time.Now()))
	if err == nil {
		t.Fatal("EnsureCacheVolume adopted a foreign volume squatting the cache name, want a fail-closed error")
	}
}

// TestEnsureCacheVolumeRejectsRepokeyCollision is the L7 guard: a DIFFERENT
// repository whose cache volume collided on the (truncated) repokey — same name,
// same managed+cache markers, but a DIFFERENT full repo-url-digest — must be
// rejected, so one repo never adopts another's cache.
func TestEnsureCacheVolumeRejectsRepokeyCollision(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	other := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: testRepoKey, RepoURLDigest: "digest-of-a-DIFFERENT-repo"}
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: other.ToolcacheLabels("1", time.Now())}); err != nil {
		t.Fatalf("seed colliding cache volume: %v", err)
	}

	ours := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: testRepoKey, RepoURLDigest: "digest-of-OUR-repo"}
	if _, err := m.EnsureCacheVolume(ctx, name, ours.ToolcacheLabels("1", time.Now())); err == nil {
		t.Fatal("EnsureCacheVolume adopted a repokey-colliding foreign repo's cache, want a fail-closed error")
	}
}

// TestEnsureCacheVolumeCrossControllerSharedReuseOK is the flip side: a
// legitimately shared cache first LABELED by ANOTHER controller (different
// controller-id, different creation timestamp, but the SAME repo identity) IS
// adopted — controller-id/last-used are not identity keys (ADR-003 shared caches).
func TestEnsureCacheVolumeCrossControllerSharedReuseOK(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	peer := spec.CacheVolumeIdentity{ControllerID: "some-OTHER-controller", RepoKey: testRepoKey, RepoURLDigest: "shared-digest"}
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: peer.ToolcacheLabels("1", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}); err != nil {
		t.Fatalf("seed peer-controller cache volume: %v", err)
	}

	ours := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: testRepoKey, RepoURLDigest: "shared-digest"}
	if _, err := m.EnsureCacheVolume(ctx, name, ours.ToolcacheLabels("1", time.Now())); err != nil {
		t.Fatalf("EnsureCacheVolume rejected a legitimately shared cross-controller cache: %v", err)
	}
}

// TestEnsureCacheVolumeDistinctKinds: toolcache and pnpm are distinct volumes.
func TestEnsureCacheVolumeDistinctKinds(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()
	now := time.Now()

	tool := spec.ToolcacheVolumeName(testRepoKey, "1")
	pnpm := spec.PnpmVolumeName(testRepoKey, "9")
	if _, err := m.EnsureCacheVolume(ctx, tool, cacheIdentity().ToolcacheLabels("1", now)); err != nil {
		t.Fatalf("toolcache ensure: %v", err)
	}
	if _, err := m.EnsureCacheVolume(ctx, pnpm, cacheIdentity().PnpmLabels("9", now)); err != nil {
		t.Fatalf("pnpm ensure: %v", err)
	}
	if n := countCacheVolumes(t, fake); n != 2 {
		t.Errorf("cache volume count = %d, want 2 (toolcache + pnpm)", n)
	}
	if _, ok := cacheVolumeByName(t, fake, tool); !ok {
		t.Error("toolcache volume missing")
	}
	if _, ok := cacheVolumeByName(t, fake, pnpm); !ok {
		t.Error("pnpm volume missing")
	}
}

// TestCacheVolumesSurviveTeardownAndSweep is the isolation guarantee: cache
// volumes must survive DeleteInstance (TeardownAllocation), the orphan sweep,
// AND the RemoveAllInstances rescue (TeardownAll), because they carry no
// instance-name and cache=true and so never match ADR-004's predicate.
func TestCacheVolumesSurviveTeardownAndSweep(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	// A persistent cache volume for this repo.
	cacheName := spec.ToolcacheVolumeName(testRepoKey, "1")
	if _, err := m.EnsureCacheVolume(ctx, cacheName, cacheIdentity().ToolcacheLabels("1", time.Now())); err != nil {
		t.Fatalf("EnsureCacheVolume: %v", err)
	}

	// A full allocation for the same "repo" (network + workspace volume + an
	// exited runner aged well past the sweep grace) that teardown/sweep SHOULD
	// reap — while leaving the cache volume alone.
	const inst = "job-1"
	const nonce = "cafe1234"
	old := time.Now().Add(-2 * time.Hour)

	reseed := func() string {
		seedNetworkFor(t, fake, testControllerID, inst, old, nonce)
		seedWorkspaceVolumeFor(t, fake, testControllerID, inst, old, nonce)
		id := seedRunnerFor(t, fake, testControllerID, inst, old, nonce, "exited")
		fake.SetFinishedAt(id, old)
		return id
	}
	reseed()

	assertCachePresent := func(stage string) {
		if _, ok := cacheVolumeByName(t, fake, cacheName); !ok {
			t.Fatalf("[%s] cache volume %q was removed — teardown/sweep must never touch a cache volume", stage, cacheName)
		}
	}

	// DeleteInstance path.
	if _, err := m.TeardownAllocation(ctx, inst); err != nil {
		t.Fatalf("TeardownAllocation: %v", err)
	}
	assertCachePresent("after TeardownAllocation")

	// Host-wide orphan sweep.
	reseed()
	if err := m.SweepOrphans(ctx); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	assertCachePresent("after SweepOrphans")

	// RemoveAllInstances rescue path (generation-agnostic wipe of managed
	// job-scoped resources) — still must not touch caches.
	seedNetworkFor(t, fake, testControllerID, inst, old, nonce)
	seedWorkspaceVolumeFor(t, fake, testControllerID, inst, old, nonce)
	if err := m.TeardownAll(ctx); err != nil {
		t.Fatalf("TeardownAll: %v", err)
	}
	assertCachePresent("after TeardownAll")

	// And it is still the only cache volume (nothing duplicated it either).
	if n := countCacheVolumes(t, fake); n != 1 {
		t.Errorf("cache volume count = %d after all teardown paths, want 1", n)
	}
}
