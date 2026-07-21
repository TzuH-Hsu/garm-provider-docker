package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// seedHelperContainer creates a cache-helper container with the given created-at
// label and Docker state, returning its ID — so the H5 leaked-helper reaper can
// be driven across the (state, age) matrix.
func seedHelperContainer(t *testing.T, fake *docker.FakeClient, controllerID string, createdAt time.Time, state string) string {
	t.Helper()
	labels := map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: controllerID,
		spec.LabelRole:         spec.RoleCacheHelper,
		spec.LabelCreatedAt:    createdAt.UTC().Format(time.RFC3339),
	}
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Image: "runner", Labels: labels}, &container.HostConfig{}, nil, nil, "")
	if err != nil {
		t.Fatalf("seed helper container: %v", err)
	}
	if state != "created" {
		fake.SetState(resp.ID, state, false)
	}
	return resp.ID
}

func helperContainerExists(t *testing.T, fake *docker.FakeClient, id string) bool {
	t.Helper()
	_, err := fake.ContainerInspect(context.Background(), id)
	return err == nil
}

// TestReapLeakedHelpersRespectsStateAndAge is the H5 guard: the leaked-helper
// reaper never reaps a helper that could be a live peer's in-flight handoff
// (a briefly `created` one, or a `running` seed/prune) yet DOES eventually reap a
// wedged/crashed one so it cannot hold the externals seeding flock forever.
func TestReapLeakedHelpersRespectsStateAndAge(t *testing.T) {
	p, fake := newCacheProvider(t, false)
	now := time.Now()

	freshCreated := seedHelperContainer(t, fake, p.controllerID, now, "created")
	freshRunning := seedHelperContainer(t, fake, p.controllerID, now, "running")
	staleRunning := seedHelperContainer(t, fake, p.controllerID, now.Add(-(helperRunningMaxAge + time.Minute)), "running")
	freshExited := seedHelperContainer(t, fake, p.controllerID, now, "exited")
	staleExited := seedHelperContainer(t, fake, p.controllerID, now.Add(-(helperTerminalGrace + time.Minute)), "exited")

	p.reapLeakedHelpers(context.Background())

	for _, tc := range []struct {
		name       string
		id         string
		wantReaped bool
	}{
		{"created (fresh) not reaped — a peer between create and start", freshCreated, false},
		{"running (fresh) not reaped — a peer's in-flight seed/prune", freshRunning, false},
		{"running (stale) reaped — a wedged seeder holding the flock", staleRunning, true},
		{"exited (fresh) not reaped — its own provider's defer will remove it", freshExited, false},
		{"exited (stale) reaped — a genuine leak", staleExited, true},
	} {
		gone := !helperContainerExists(t, fake, tc.id)
		if gone != tc.wantReaped {
			t.Errorf("%s: reaped=%v, want %v", tc.name, gone, tc.wantReaped)
		}
	}
}

// seedHelperScript finds the externals-seed helper recorded in the fake's
// Created log (by its cache-helper role + flock script) and returns its shell
// script, or fails.
func seedHelperScript(t *testing.T, fake *docker.FakeClient) string {
	t.Helper()
	for _, c := range fake.Created {
		if c.Labels[spec.LabelRole] != spec.RoleCacheHelper {
			continue
		}
		if len(c.Entrypoint) == 3 && strings.Contains(c.Entrypoint[2], "flock") {
			return c.Entrypoint[2]
		}
	}
	t.Fatal("no externals-seed helper was created")
	return ""
}

// TestCreateInstanceExternalsSeededAndMountedReadOnly: a repo-scoped create
// seeds the externals volume (a helper ran the flock+copy script) and mounts it
// READ-ONLY into the runner, and mounts the diag volume + sets GARM_DIAG_DIR.
func TestCreateInstanceExternalsSeededAndMountedReadOnly(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)

	if _, err := p.CreateInstance(context.Background(), cacheBootstrap("job-1", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}

	// The seed helper ran and its script has the flock + atomic marker + copy.
	script := seedHelperScript(t, fake)
	for _, want := range []string{"flock", ".garm-seeded", "cp -a"} {
		if !strings.Contains(script, want) {
			t.Errorf("seed helper script missing %q:\n%s", want, script)
		}
	}

	// Exactly one externals volume, mounted READ-ONLY at the externals dir.
	ext := cacheVolsByKind(t, fake, spec.CacheKindExternals)
	if len(ext) != 1 {
		t.Fatalf("externals volumes = %v, want exactly 1", ext)
	}
	c := inspectRunner(t, fake, "job-1")
	m, ok := mountAt(c, spec.RunnerExternalsDir)
	if !ok {
		t.Fatalf("runner missing externals mount at %s", spec.RunnerExternalsDir)
	}
	if m.Name != ext[0] {
		t.Errorf("externals mount source = %q, want %q", m.Name, ext[0])
	}
	if m.RW {
		t.Error("externals mount is read-WRITE — it MUST be read-only (cross-repo RCE guard, F4)")
	}

	// Diag volume mounted read-write and GARM_DIAG_DIR set for the entrypoint.
	dm, ok := mountAt(c, spec.RunnerDiagDir)
	if !ok || !dm.RW {
		t.Errorf("diag mount at %s = %+v (ok=%v), want a read-write mount", spec.RunnerDiagDir, dm, ok)
	}
	if !envHas(c, spec.RunnerDiagDirEnv+"="+spec.RunnerDiagDir) {
		t.Errorf("%s env not set to %s", spec.RunnerDiagDirEnv, spec.RunnerDiagDir)
	}
}

// TestCreateInstanceSeedFailureFailsCreate: a seed helper that exits non-zero
// fails the allocation; the guarded rollback removes the allocation's own
// resources (no runner container), while the externals cache volume — created
// before the seed — is left intact for the next attempt (a cache is never rolled
// back).
func TestCreateInstanceSeedFailureFailsCreate(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)
	fake.BatchExitCode = 1 // the seed helper "runs" but fails

	_, err := p.CreateInstance(context.Background(), cacheBootstrap("job-1", cacheRepoURL, srv.URL))
	if err == nil {
		t.Fatal("CreateInstance succeeded despite a failing externals seed, want an error")
	}
	if !strings.Contains(err.Error(), "externals") {
		t.Errorf("error %q does not mention the externals seed failure", err)
	}
	// The runner container must not exist (guarded rollback).
	if _, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName("job-1")); err == nil {
		t.Error("a runner container survived a failed externals seed — the guard must roll it back")
	}
}

// TestRunCacheGCEvictsSupersededKeepsCurrent: the opportunistic GC evicts a
// superseded-generation toolcache volume (aged past the grace) while keeping the
// current-generation one — proving the eviction wiring end-to-end via the fake.
func TestRunCacheGCEvictsSupersededKeepsCurrent(t *testing.T) {
	p, fake := newCacheProvider(t, false) // current generation "1"
	ctx := context.Background()

	// Directly seed two toolcache volumes for the same repo: a superseded
	// generation "0" created 2h ago (past the 30m grace) and the current
	// generation "1" created just now.
	id := spec.CacheVolumeIdentity{ControllerID: "controller-abc", RepoKey: "repo-x"}
	superseded := spec.ToolcacheVolumeName("repo-x", "0")
	current := spec.ToolcacheVolumeName("repo-x", "1")
	seedVol(t, fake, superseded, id.ToolcacheLabels("0", time.Now().Add(-2*time.Hour)))
	seedVol(t, fake, current, id.ToolcacheLabels("1", time.Now()))

	p.runCacheGC(ctx)

	if volPresent(t, fake, superseded) {
		t.Error("GC did not evict the superseded-generation toolcache volume")
	}
	if !volPresent(t, fake, current) {
		t.Error("GC evicted the CURRENT-generation toolcache volume — it must be kept")
	}
}

// TestRunCacheGCPrunesDiagVolume: after a repo-scoped create makes a diag
// volume, an opportunistic GC pass runs a diag-prune helper against it — with the
// retention-scoped `find -mmin -delete` (L8) and the diag volume mounted.
func TestRunCacheGCPrunesDiagVolume(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)
	ctx := context.Background()

	if _, err := p.CreateInstance(ctx, cacheBootstrap("job-1", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	diag := cacheVolsByKind(t, fake, spec.CacheKindDiagLogs)
	if len(diag) != 1 {
		t.Fatalf("diag volumes = %v, want 1", diag)
	}

	// Run the GC pass explicitly and look for the diag-prune helper it created.
	before := len(fake.Created)
	p.runCacheGC(ctx)

	var pruneScript string
	var pruneMounts []string
	var pruneNetIsNone bool
	found := false
	for _, c := range fake.Created[before:] {
		if c.Labels[spec.LabelRole] != spec.RoleCacheHelper || len(c.Entrypoint) != 3 {
			continue
		}
		if strings.Contains(c.Entrypoint[2], "find") {
			found = true
			pruneScript = c.Entrypoint[2]
			pruneNetIsNone = c.NetworkMode.IsNone()
			for _, m := range c.Mounts {
				pruneMounts = append(pruneMounts, m.Source)
			}
		}
	}
	if !found {
		t.Fatal("GC did not run a diag-prune helper")
	}
	// L9: the helper joins the "none" network, not the default bridge.
	if !pruneNetIsNone {
		t.Error("diag-prune helper NetworkMode is not none (L9)")
	}
	for _, want := range []string{"find", "-mmin +", "-delete"} {
		if !strings.Contains(pruneScript, want) {
			t.Errorf("diag-prune script missing %q: %s", want, pruneScript)
		}
	}
	if len(pruneMounts) != 1 || pruneMounts[0] != diag[0] {
		t.Errorf("diag-prune mounts = %v, want just the diag volume %q", pruneMounts, diag[0])
	}
}

// seedVol creates a volume with explicit labels.
func seedVol(t *testing.T, fake *docker.FakeClient, name string, labels map[string]string) {
	t.Helper()
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: name, Labels: labels}); err != nil {
		t.Fatalf("seed volume %q: %v", name, err)
	}
}

// volPresent reports whether a volume of the given name exists.
func volPresent(t *testing.T, fake *docker.FakeClient, name string) bool {
	t.Helper()
	_, ok := volByName(t, fake, name)
	return ok
}
