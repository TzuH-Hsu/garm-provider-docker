package topology

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// gcTime is an arbitrary fixed creation instant for seeded cache volumes; the
// log-only GC tests below decide by the repo label, so the exact value is
// immaterial (the age/supersession decision itself is unit-tested in package spec).
func gcTime() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) }

// fullCacheID returns a COMPLETE cache identity (controller + repokey + full
// repo-url-digest) so the strict kind-aware validation the log-only enumeration
// applies accepts the seeded volume as provably ours. The repo-url-digest is
// derived from a synthetic URL so it is valid 64-hex.
func fullCacheID(repoKey string) spec.CacheVolumeIdentity {
	return spec.CacheVolumeIdentity{
		ControllerID:  testControllerID,
		RepoKey:       repoKey,
		RepoURLDigest: spec.RepoURLDigest("https://github.com/garm-topology-test/" + repoKey),
	}
}

// seedRawCacheVolume creates a cache volume with an explicit label set, so a GC
// test can place volumes of any kind/generation/controller directly.
func seedRawCacheVolume(t *testing.T, fake *docker.FakeClient, name string, labels map[string]string) {
	t.Helper()
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: name, Labels: labels}); err != nil {
		t.Fatalf("seed cache volume %q: %v", name, err)
	}
}

// evictByRepo returns a decide func that flags volumes whose repo label is in the
// given set (each seeded volume gets a unique repo label used as its handle).
func evictByRepo(repoKeys ...string) func(map[string]string) (bool, string) {
	set := map[string]bool{}
	for _, n := range repoKeys {
		set[n] = true
	}
	return func(labels map[string]string) (bool, string) {
		return set[labels[spec.LabelRepo]], "test-stale"
	}
}

// failIfVolumeRemoved installs a hook that fails the test if ANY VolumeRemove is
// issued — the load-bearing NEW-H1 assertion that the log-only eviction path never
// removes a cache volume.
func failIfVolumeRemoved(t *testing.T, fake *docker.FakeClient) {
	t.Helper()
	fake.VolumeRemoveHook = func(name string) {
		t.Errorf("VolumeRemove(%q) was called on the log-only eviction path — cache GC must be NON-DESTRUCTIVE (NEW-H1)", name)
	}
}

// TestListStaleCachesReturnsDecidedKeepsOthers: ListStaleCaches RETURNS exactly
// the volumes decide flags stale, LEAVES every volume in place (log-only), and
// never issues a VolumeRemove.
func TestListStaleCachesReturnsDecidedKeepsOthers(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()
	failIfVolumeRemoved(t, fake)

	keepName := spec.ToolcacheVolumeName("keep", "1")
	staleName := spec.ToolcacheVolumeName("stale", "1")
	seedRawCacheVolume(t, fake, keepName, fullCacheID("keep").ToolcacheLabels("1", gcTime()))
	seedRawCacheVolume(t, fake, staleName, fullCacheID("stale").ToolcacheLabels("1", gcTime()))

	stale, err := m.ListStaleCaches(ctx, evictByRepo("stale"))
	if err != nil {
		t.Fatalf("ListStaleCaches: %v", err)
	}
	if len(stale) != 1 || stale[0].Name != staleName {
		t.Fatalf("stale = %+v, want just %q", stale, staleName)
	}
	// BOTH volumes must survive — the GC only reports, never removes.
	for _, n := range []string{keepName, staleName} {
		if !rawVolumePresent(t, fake, n) {
			t.Errorf("volume %q was removed by the log-only GC; it must survive", n)
		}
	}
}

// TestListStaleCachesNeverRemovesEvenWhenAllStale: with decide flagging every
// cache stale, ListStaleCaches STILL removes nothing (no VolumeRemove) and every
// volume survives — the operator, not the opportunistic GC, does the purge.
func TestListStaleCachesNeverRemovesEvenWhenAllStale(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()
	failIfVolumeRemoved(t, fake)

	names := []string{
		spec.ToolcacheVolumeName("a", "1"),
		spec.ToolcacheVolumeName("b", "1"),
		spec.ToolcacheVolumeName("c", "1"),
	}
	for i, key := range []string{"a", "b", "c"} {
		seedRawCacheVolume(t, fake, names[i], fullCacheID(key).ToolcacheLabels("1", gcTime()))
	}

	evictAll := func(map[string]string) (bool, string) { return true, "all-stale" }
	stale, err := m.ListStaleCaches(ctx, evictAll)
	if err != nil {
		t.Fatalf("ListStaleCaches: %v", err)
	}
	if len(stale) != len(names) {
		t.Errorf("stale count = %d, want %d (all flagged)", len(stale), len(names))
	}
	for _, n := range names {
		if !rawVolumePresent(t, fake, n) {
			t.Errorf("volume %q was removed; the log-only GC must never delete a cache volume", n)
		}
	}
}

// TestListStaleCachesRequiresFullIdentity: a cache-name volume for this controller
// that decide WOULD flag stale but is missing a required identity key (no
// repo-url-digest) is NOT reported — strict kind-aware validation keeps the
// operator log accurate (never naming a volume that is not provably ours). A
// complete sibling IS reported.
func TestListStaleCachesRequiresFullIdentity(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()
	failIfVolumeRemoved(t, fake)

	// Complete identity → reported.
	completeName := spec.ToolcacheVolumeName("complete", "1")
	seedRawCacheVolume(t, fake, completeName, fullCacheID("complete").ToolcacheLabels("1", gcTime()))

	// Incomplete identity (no repo-url-digest) → skipped even though decide matches.
	incompleteName := spec.ToolcacheVolumeName("incomplete", "1")
	seedRawCacheVolume(t, fake, incompleteName, map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: testControllerID,
		spec.LabelCache:        "true",
		spec.LabelCacheKind:    string(spec.CacheKindToolcache),
		spec.LabelRepo:         "incomplete",
		spec.LabelGeneration:   "1",
		spec.LabelLastUsed:     gcTime().Format(time.RFC3339),
		// deliberately NO repo-url-digest
	})

	stale, err := m.ListStaleCaches(ctx, evictByRepo("complete", "incomplete"))
	if err != nil {
		t.Fatalf("ListStaleCaches: %v", err)
	}
	if len(stale) != 1 || stale[0].Name != completeName {
		t.Errorf("stale = %+v, want just the complete-identity volume %q (the incomplete one must be skipped)", stale, completeName)
	}
}

// TestListStaleCachesNeverTouchesForeignOrNonCache: a foreign-controller cache
// volume and a non-cache (job-scoped) volume are never even listed, so decide is
// never consulted for them and they are never reported — even when decide would
// flag everything.
func TestListStaleCachesNeverTouchesForeignOrNonCache(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()
	failIfVolumeRemoved(t, fake)

	// Foreign controller's cache volume (different controller-id label).
	foreign := spec.CacheVolumeIdentity{ControllerID: "other-controller", RepoKey: "evict", RepoURLDigest: spec.RepoURLDigest("https://github.com/x/evict")}
	foreignName := spec.ToolcacheVolumeName("evict", "1") + "-foreign"
	seedRawCacheVolume(t, fake, foreignName, foreign.ToolcacheLabels("1", gcTime()))

	// A job-scoped (non-cache) volume for THIS controller.
	alloc := spec.AllocationIdentity{ControllerID: testControllerID, PoolID: "p", InstanceName: "job-1"}
	jobVolName := "job-1-abc-workspace"
	seedRawCacheVolume(t, fake, jobVolName, alloc.WorkspaceVolumeLabels(gcTime()))

	evictAll := func(map[string]string) (bool, string) { return true, "evict-all" }
	stale, err := m.ListStaleCaches(ctx, evictAll)
	if err != nil {
		t.Fatalf("ListStaleCaches: %v", err)
	}
	for _, s := range stale {
		if s.Name == foreignName || s.Name == jobVolName {
			t.Errorf("ListStaleCaches reported a foreign/non-cache volume %q — it must only see this controller's cache=true volumes", s.Name)
		}
	}
}

// TestListDiagVolumes returns only the COMPLETE diag-logs cache volumes for this
// controller: a foreign diag volume, a toolcache volume, and an incomplete diag
// volume (missing repo-url-digest, which the strict enumeration gate rejects) are
// all excluded.
func TestListDiagVolumes(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	id := fullCacheID("repo-a")
	diagName := spec.DiagVolumeName("repo-a")
	seedRawCacheVolume(t, fake, diagName, id.DiagLabels(gcTime()))
	// A toolcache volume (different kind) and a foreign diag volume must be excluded.
	seedRawCacheVolume(t, fake, spec.ToolcacheVolumeName("repo-a", "1"), id.ToolcacheLabels("1", gcTime()))
	foreign := spec.CacheVolumeIdentity{ControllerID: "other", RepoKey: "repo-b", RepoURLDigest: spec.RepoURLDigest("https://github.com/x/repo-b")}
	seedRawCacheVolume(t, fake, spec.DiagVolumeName("repo-b"), foreign.DiagLabels(gcTime()))
	// An INCOMPLETE diag volume (no repo-url-digest) for this controller — the
	// strict enumeration gate (NEW-H2) must exclude it so the destructive prune
	// never even enumerates it.
	seedRawCacheVolume(t, fake, spec.DiagVolumeName("repo-incomplete"), map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: testControllerID,
		spec.LabelCache:        "true",
		spec.LabelCacheKind:    string(spec.CacheKindDiagLogs),
		spec.LabelRepo:         "repo-incomplete",
		spec.LabelLastUsed:     gcTime().Format(time.RFC3339),
	})

	refs, err := m.ListDiagVolumes(ctx)
	if err != nil {
		t.Fatalf("ListDiagVolumes: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != diagName {
		t.Fatalf("ListDiagVolumes = %v, want just %q (foreign, wrong-kind, and incomplete-identity excluded)", refs, diagName)
	}
	// The snapshot carries the diag volume's full identity labels for the
	// provider's pin-then-validate re-check.
	if refs[0].Labels[spec.LabelCacheKind] != string(spec.CacheKindDiagLogs) || refs[0].Labels[spec.LabelRepo] != "repo-a" || refs[0].Labels[spec.LabelRepoURLDigest] == "" {
		t.Errorf("ListDiagVolumes snapshot labels = %v, want a full diag identity (cache-kind, repo, repo-url-digest)", refs[0].Labels)
	}
}

// TestListUnprovableCacheCruft surfaces cache-name-prefixed volumes that lack our
// cache=true label (unlabeled auto-created replacements / foreign squatters) for
// operator-visibility logging, WITHOUT ever deleting them (ADR-003
// never-delete-unprovable). A provably-ours cache and an unrelated foreign volume
// are excluded.
func TestListUnprovableCacheCruft(t *testing.T) {
	m, fake := newManager(t)
	ctx := context.Background()

	id := fullCacheID("repo-a")
	oursName := spec.ToolcacheVolumeName("repo-a", "1")
	seedRawCacheVolume(t, fake, oursName, id.ToolcacheLabels("1", gcTime()))

	cruftName := spec.ToolcacheVolumeName("repo-b", "1") // cache-named but UNLABELED
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: cruftName, Labels: map[string]string{}}); err != nil {
		t.Fatalf("seed cruft: %v", err)
	}
	if _, err := fake.VolumeCreate(ctx, volume.CreateOptions{Name: "some-foreign-vol", Labels: map[string]string{"x": "y"}}); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	cruft, err := m.ListUnprovableCacheCruft(ctx)
	if err != nil {
		t.Fatalf("ListUnprovableCacheCruft: %v", err)
	}
	if len(cruft) != 1 || cruft[0] != cruftName {
		t.Errorf("ListUnprovableCacheCruft = %v, want just %q", cruft, cruftName)
	}
	// Nothing was deleted — cruft is surfaced, never removed.
	if !rawVolumePresent(t, fake, cruftName) {
		t.Error("the unprovable cruft volume was deleted; it must be left in place")
	}
	if !rawVolumePresent(t, fake, oursName) {
		t.Error("our labeled cache was deleted")
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
