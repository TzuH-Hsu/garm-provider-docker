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

// rawVolumeLabels inspects a volume by name directly (bypassing the cache=true
// label filter), so a test can assert a FOREIGN/UNLABELED volume the reconcile
// must PRESERVE is still present with its ORIGINAL labels.
func rawVolumeLabels(t *testing.T, fake *docker.FakeClient, name string) (map[string]string, bool) {
	t.Helper()
	v, err := fake.VolumeInspect(context.Background(), name)
	if err != nil {
		return nil, false
	}
	return v.Labels, true
}

// TestEnsureCacheVolumeReconcilesAroundForeignSquatter is the B2 structural guard:
// a FOREIGN volume squatting the deterministic cache name is NEVER deleted (a
// Moby auto-created unlabeled volume and a genuinely foreign one are
// indistinguishable, so name-based deletion can never be foreign-safe) and NEVER
// wedges creation. EnsureCacheVolume reconciles AROUND it to a deterministic
// alternate name; the foreign volume is PRESERVED verbatim, our cache is created
// under the alternate, and — because discovery is by LABEL — the alternate is
// found and reused on the next allocation.
func TestEnsureCacheVolumeReconcilesAroundForeignSquatter(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	foreignLabels := map[string]string{"someone": "else"}
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: foreignLabels}); err != nil {
		t.Fatalf("seed foreign volume: %v", err)
	}

	want := cacheIdentity().ToolcacheLabels("1", time.Now())
	res, err := m.EnsureCacheVolume(ctx, name, want)
	if err != nil {
		t.Fatalf("EnsureCacheVolume must reconcile around a foreign squatter, not error: %v", err)
	}
	if res.Name == name {
		t.Fatalf("EnsureCacheVolume returned the squatted deterministic name %q; want a reconciled alternate", name)
	}
	if res.Hit {
		t.Error("EnsureCacheVolume reported a HIT for a freshly reconciled cache; want a MISS")
	}

	// The foreign volume is PRESERVED verbatim — never deleted, never adopted.
	lbls, ok := rawVolumeLabels(t, fake, name)
	if !ok {
		t.Fatalf("the foreign volume %q was DELETED — the provider must never delete a volume it cannot prove is ours", name)
	}
	if lbls["someone"] != "else" || lbls[spec.LabelCache] == "true" {
		t.Errorf("the foreign volume %q was mutated/adopted (labels=%v)", name, lbls)
	}

	// Our cache exists under the ALTERNATE name with our identity.
	altLabels, ok := cacheVolumeByName(t, fake, res.Name)
	if !ok {
		t.Fatalf("our cache was not created under the reconciled name %q", res.Name)
	}
	if altLabels[spec.LabelRepo] != testRepoKey {
		t.Errorf("reconciled cache lost its identity: %v", altLabels)
	}

	// Discovery-by-label finds the alternate on the next allocation → HIT, same name.
	res2, err := m.EnsureCacheVolume(ctx, name, cacheIdentity().ToolcacheLabels("1", time.Now()))
	if err != nil {
		t.Fatalf("second EnsureCacheVolume errored: %v", err)
	}
	if !res2.Hit {
		t.Error("second EnsureCacheVolume did not report a HIT; the reconciled cache was not discovered by label")
	}
	if res2.Name != res.Name {
		t.Errorf("second EnsureCacheVolume returned %q, want the reconciled %q (label discovery must be name-stable)", res2.Name, res.Name)
	}
}

// TestEnsureCacheVolumeReconcilesAroundRepokeyCollision is the L7-as-reconcile
// case: a DIFFERENT repository whose cache collided on the truncated repokey (same
// name and managed+cache markers, but a DIFFERENT full repo-url-digest) is NEVER
// adopted (one repo must not mount another's cache) and NEVER deleted — the
// provider reconciles around it to an alternate name, and the colliding cache is
// preserved.
func TestEnsureCacheVolumeReconcilesAroundRepokeyCollision(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	other := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: testRepoKey, RepoURLDigest: "digest-of-a-DIFFERENT-repo"}
	otherLabels := other.ToolcacheLabels("1", time.Now())
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: otherLabels}); err != nil {
		t.Fatalf("seed colliding cache volume: %v", err)
	}

	ours := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: testRepoKey, RepoURLDigest: "digest-of-OUR-repo"}
	res, err := m.EnsureCacheVolume(ctx, name, ours.ToolcacheLabels("1", time.Now()))
	if err != nil {
		t.Fatalf("EnsureCacheVolume must reconcile around a repokey collision, not error: %v", err)
	}
	if res.Name == name {
		t.Fatalf("EnsureCacheVolume adopted the colliding name %q; want a reconciled alternate", name)
	}

	// The colliding repo's cache is PRESERVED with its own digest.
	lbls, ok := rawVolumeLabels(t, fake, name)
	if !ok {
		t.Fatalf("the colliding cache %q was deleted — never delete another repo's cache", name)
	}
	if lbls[spec.LabelRepoURLDigest] != "digest-of-a-DIFFERENT-repo" {
		t.Errorf("the colliding cache %q was mutated (labels=%v)", name, lbls)
	}

	// Our cache under the alternate carries OUR digest.
	altLabels, ok := cacheVolumeByName(t, fake, res.Name)
	if !ok {
		t.Fatalf("our cache was not created under the reconciled name %q", res.Name)
	}
	if altLabels[spec.LabelRepoURLDigest] != "digest-of-OUR-repo" {
		t.Errorf("reconciled cache carries the wrong digest: %v", altLabels)
	}
}

// TestEnsureCacheVolumeUnlabeledSquatterDoesNotWedge is the B3 no-wedge guard: an
// UNLABELED volume holding the deterministic name (e.g. a Moby auto-created
// replacement left behind by an evict/create race) must NOT permanently block
// creation the way the old M6 adoption guard did (which rejected the name
// forever). EnsureCacheVolume reconciles around it, so a subsequent create for the
// same identity succeeds — no wedge — while the unlabeled volume is left as
// (logged) cruft, never deleted.
func TestEnsureCacheVolumeUnlabeledSquatterDoesNotWedge(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	name := spec.ToolcacheVolumeName(testRepoKey, "1")
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{}}); err != nil {
		t.Fatalf("seed unlabeled volume: %v", err)
	}

	res, err := m.EnsureCacheVolume(ctx, name, cacheIdentity().ToolcacheLabels("1", time.Now()))
	if err != nil {
		t.Fatalf("[B3] an unlabeled deterministic-named volume WEDGED creation: %v", err)
	}
	if res.Name == name {
		t.Fatalf("[B3] EnsureCacheVolume adopted the unlabeled squatter %q instead of reconciling", name)
	}
	// The unlabeled volume is left as cruft (not deleted).
	if _, ok := rawVolumeLabels(t, fake, name); !ok {
		t.Errorf("[B3] the unlabeled volume %q was deleted — cruft must be left, not deleted", name)
	}
	// A cache for our identity now exists (creation is not wedged).
	if _, ok := cacheVolumeByName(t, fake, res.Name); !ok {
		t.Errorf("[B3] no cache was created after reconciling around the unlabeled squatter")
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
