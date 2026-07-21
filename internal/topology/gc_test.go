package topology

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// gcTime is an arbitrary fixed creation instant for seeded cache volumes; the
// GC-mechanics tests below decide by NAME, so the exact value is immaterial (the
// age/supersession decision itself is unit-tested in package spec).
func gcTime() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }

// seedRawCacheVolume creates a cache volume with an explicit label set, so a GC
// test can place volumes of any kind/generation/controller directly.
func seedRawCacheVolume(t *testing.T, fake *docker.FakeClient, name string, labels map[string]string) {
	t.Helper()
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: name, Labels: labels}); err != nil {
		t.Fatalf("seed cache volume %q: %v", name, err)
	}
}

// evictByName returns a decide func that evicts volumes whose repo label is in
// the given set (each seeded volume gets a unique repo label used as its handle).
func evictByName(names ...string) func(map[string]string) (bool, string) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(labels map[string]string) (bool, string) {
		return set[labels[spec.LabelRepo]], "test-evict"
	}
}

// TestEvictCachesRemovesDecidedKeepsOthers: EvictCaches removes exactly the
// volumes decide selects and leaves the rest.
func TestEvictCachesRemovesDecidedKeepsOthers(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	id := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: "keep"}
	victim := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: "evict"}
	keepName := spec.ToolcacheVolumeName("keep", "1")
	evictName := spec.ToolcacheVolumeName("evict", "1")
	seedRawCacheVolume(t, fake, keepName, id.ToolcacheLabels("1", gcTime()))
	seedRawCacheVolume(t, fake, evictName, victim.ToolcacheLabels("1", gcTime()))

	evicted, err := m.EvictCaches(ctx, evictByName("evict"), 0)
	if err != nil {
		t.Fatalf("EvictCaches: %v", err)
	}
	if len(evicted) != 1 || evicted[0].Name != evictName {
		t.Fatalf("evicted = %+v, want just %q", evicted, evictName)
	}
	if _, ok := cacheVolumeByName(t, fake, evictName); ok {
		t.Error("the decided-evict volume was not removed")
	}
	if _, ok := cacheVolumeByName(t, fake, keepName); !ok {
		t.Error("a kept volume was removed")
	}
}

// TestEvictCachesSkipsInUse: a cache volume mounted into a container returns a
// Conflict on removal, which EvictCaches SKIPS (never yanks a warm cache from a
// live job) without erroring.
func TestEvictCachesSkipsInUse(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	inUse := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: "evict"}
	name := spec.ToolcacheVolumeName("evict", "1")
	seedRawCacheVolume(t, fake, name, inUse.ToolcacheLabels("1", gcTime()))

	// A container references the cache volume → the fake rejects its removal with
	// a Conflict ("volume is in use"), modeling a live job holding the cache.
	if _, err := fake.ContainerCreate(ctx, &container.Config{Image: "runner"},
		&container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/opt/hostedtoolcache"}}},
		nil, nil, "live-runner"); err != nil {
		t.Fatalf("seed in-use container: %v", err)
	}

	evicted, err := m.EvictCaches(ctx, evictByName("evict"), 0)
	if err != nil {
		t.Fatalf("EvictCaches returned error for an in-use volume, want a best-effort skip: %v", err)
	}
	if len(evicted) != 0 {
		t.Errorf("evicted = %+v, want none (the volume is in use)", evicted)
	}
	if _, ok := cacheVolumeByName(t, fake, name); !ok {
		t.Error("an in-use cache volume was removed — GC must never yank a warm cache from a live job")
	}
}

// TestEvictCachesNeverTouchesForeignOrNonCache: a foreign-controller cache
// volume and a non-cache (job-scoped) volume are never even listed, so decide is
// never consulted for them and they are never removed — even when decide would
// say "evict".
func TestEvictCachesNeverTouchesForeignOrNonCache(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	// Foreign controller's cache volume (different controller-id label).
	foreign := spec.CacheVolumeIdentity{ControllerID: "other-controller", RepoKey: "evict"}
	foreignName := spec.ToolcacheVolumeName("evict", "1") + "-foreign"
	seedRawCacheVolume(t, fake, foreignName, foreign.ToolcacheLabels("1", gcTime()))

	// A job-scoped (non-cache) volume for THIS controller.
	alloc := spec.AllocationIdentity{ControllerID: testControllerID, PoolID: "p", InstanceName: "job-1"}
	jobVolName := "job-1-abc-workspace"
	seedRawCacheVolume(t, fake, jobVolName, alloc.WorkspaceVolumeLabels(gcTime()))

	// decide says "evict everything" — the point is these two are never even
	// passed to it.
	evictAll := func(map[string]string) (bool, string) { return true, "evict-all" }
	evicted, err := m.EvictCaches(ctx, evictAll, 0)
	if err != nil {
		t.Fatalf("EvictCaches: %v", err)
	}
	for _, e := range evicted {
		if e.Name == foreignName || e.Name == jobVolName {
			t.Errorf("EvictCaches removed a foreign/non-cache volume %q — it must only touch this controller's cache=true volumes", e.Name)
		}
	}
	for _, n := range []string{foreignName, jobVolName} {
		if !rawVolumePresent(t, fake, n) {
			t.Errorf("volume %q was removed by GC, must never be", n)
		}
	}
}

// TestEvictCachesRespectsCap: maxEvictions bounds a single pass.
func TestEvictCachesRespectsCap(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c", "d"} {
		id := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: key}
		seedRawCacheVolume(t, fake, spec.ToolcacheVolumeName(key, "1"), id.ToolcacheLabels("1", gcTime()))
	}
	evictAll := func(map[string]string) (bool, string) { return true, "all" }
	evicted, err := m.EvictCaches(ctx, evictAll, 2)
	if err != nil {
		t.Fatalf("EvictCaches: %v", err)
	}
	if len(evicted) != 2 {
		t.Errorf("evicted %d, want the cap of 2", len(evicted))
	}
}

// TestListDiagVolumes returns only the diag-logs cache volumes for this
// controller.
func TestListDiagVolumes(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	id := spec.CacheVolumeIdentity{ControllerID: testControllerID, RepoKey: "repo-a"}
	diagName := spec.DiagVolumeName("repo-a")
	seedRawCacheVolume(t, fake, diagName, id.DiagLabels(gcTime()))
	// A toolcache volume (different kind) and a foreign diag volume must be excluded.
	seedRawCacheVolume(t, fake, spec.ToolcacheVolumeName("repo-a", "1"), id.ToolcacheLabels("1", gcTime()))
	foreign := spec.CacheVolumeIdentity{ControllerID: "other", RepoKey: "repo-b"}
	seedRawCacheVolume(t, fake, spec.DiagVolumeName("repo-b"), foreign.DiagLabels(gcTime()))

	names, err := m.ListDiagVolumes(ctx)
	if err != nil {
		t.Fatalf("ListDiagVolumes: %v", err)
	}
	if len(names) != 1 || names[0] != diagName {
		t.Errorf("ListDiagVolumes = %v, want just %q", names, diagName)
	}
}

func rawVolumePresent(t *testing.T, fake *docker.FakeClient, name string) bool {
	t.Helper()
	list, err := fake.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList: %v", err)
	}
	for _, v := range list.Volumes {
		if v != nil && v.Name == name {
			return true
		}
	}
	return false
}
