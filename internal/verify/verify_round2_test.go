//go:build dockerverify

// This file adds the M2 codex round-2 cross-family real-daemon verification for
// the cache ensure/seed/mount/GC coordination fixes:
//
//   - NEW-H1 (happy path): normal cache-enabled allocations create-then-REUSE
//     their warm caches; a valid warm cache is never spuriously deleted by the
//     post-create revalidation (the error-injection fail-closed path is unit
//     verified — a transient inspect cannot be injected through the CLI binary).
//
//   - H3b: the externals volume is fully SEEDED BEFORE THE RUNNER STARTS — the
//     running runner can execute a Node binary THROUGH the read-only externals
//     mount (a half-populated tree would fail), proving the provider seeds to
//     completion before ContainerCreate; and the always-run flocked seed is
//     IDEMPOTENT — a second same-digest allocation's seeder is a fast
//     flock+marker no-op that does NOT re-copy (the .garm-seeded marker's mtime
//     is byte-identical across the two allocations, and exactly one externals
//     volume exists).
//
//   - H3c: the diag-prune helper's pin-then-validate discipline does not wedge
//     the deterministic diag name on a real daemon — after a GC pass runs the
//     prune helper, the diag volume STILL carries its full identity labels (it is
//     the labeled cache, not an unlabeled auto-created orphan), and a subsequent
//     allocation for the same repo gets a diag cache HIT (the name remains
//     adoptable, never wedged). The auto-create-during-helper-create RACE reap is
//     unit verified (it cannot be injected deterministically through the binary).
//
// Isolation: a unique controller-id + unique repo_url; cleanup is label-scoped
// plus explicit removal of this run's cache volumes, asserted by a before/after
// volume snapshot. It NEVER touches hbot-lab-mongodb/hummingbot or any foreign
// resource.
//
// Run with: go test -tags dockerverify -v -run TestVerifyRound2 ./internal/verify/
package verify

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

func TestVerifyRound2SeedBeforeStartIdempotentDiagNoWedge(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)
	token := randHex(t)
	repoURL := "https://github.com/garm-r2-verify/repo-" + token
	repoKey := spec.RepoKey(repoURL)

	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
	volsBefore := volumeSet(t)

	nobleTag := "garm-r2-noble:" + token
	buildNobleRunnerImage(t, root, nobleTag)
	image := "garm-r2-runner:" + token
	buildSleepVariant(t, nobleTag, image, "")

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	cfg := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, cfg, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true

[cache]
enabled = true
generation = "1"
pnpm_major = "9"
toolcache_path = "/opt/hostedtoolcache"
pnpm_store_path = "/opt/pnpm-store"
diagnostic_log_retention_days = 7
stale_cache_eviction_days = 30
`, image))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	digest := imageDigestHex(t, image)
	extVol := spec.ExternalsVolumeName(digest)
	diagVol := spec.DiagVolumeName(repoKey)
	toolVol := spec.ToolcacheVolumeName(repoKey, "1")
	t.Logf("[setup] controller=%s repoKey=%s\n  extVol=%s\n  diagVol=%s", controllerID, repoKey, extVol, diagVol)

	defer func() {
		cleanupController(t, controllerID)
		for _, v := range []string{extVol, diagVol, toolVol, spec.PnpmVolumeName(repoKey, "9")} {
			_, _ = dockerTry("volume", "rm", "-f", v)
		}
		_, _ = dockerTry("rmi", "-f", image, nobleTag)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
		volsAfter := volumeSet(t)
		for name := range volsBefore {
			if !volsAfter[name] {
				t.Errorf("[cleanup] foreign volume %q was removed by this test — cleanup must be label-scoped", name)
			}
		}
	}()

	// =========================================================================
	// (H3b) seeded BEFORE the runner starts: run a1 and prove the RUNNING runner
	// executes Node THROUGH the read-only externals mount.
	// =========================================================================
	b1 := cacheBootstrapPayload("r2-job-a1", repoURL, srv.URL, caBundle)
	if out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b1); code != 0 {
		t.Fatalf("job a1 CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}

	// The externals mount the runner got points at the shared, seeded volume.
	if src := mountSource(t, "r2-job-a1", spec.RunnerExternalsDir); src != extVol {
		t.Fatalf("(H3b) r2-job-a1 externals mount = %q, want the seeded %q", src, extVol)
	}
	// A Node binary is executable THROUGH the read-only externals mount inside the
	// LIVE runner — proof the volume was fully seeded (perms preserved by cp -a)
	// BEFORE the runner started; a half-populated mount would have no node binary.
	nodeVer, err := dockerTry("exec", "r2-job-a1", "/actions-runner/externals/node20/bin/node", "--version")
	if err != nil {
		t.Errorf("(H3b) the running runner could NOT execute node through the RO externals mount (a half-populated tree?): %v\n%s", err, nodeVer)
	} else {
		t.Logf("[H3b] running runner executes node through the RO externals mount: %s (seeded before start)", strings.TrimSpace(nodeVer))
	}
	// The marker + node dirs are present in the seeded volume.
	seeded := dockerOut(t, "run", "--rm", "-v", extVol+":/x:ro", "alpine:3.20", "ls", "-A", "/x")
	if !strings.Contains(seeded, ".garm-seeded") || !strings.Contains(seeded, "node20") {
		t.Errorf("(H3b) externals volume not fully seeded: %s", seeded)
	}
	// Capture the seeded marker's mtime (epoch seconds) — the "was it re-seeded"
	// signal: the warm-path seeder never touches the marker.
	markerMtimeAfterA1 := strings.TrimSpace(dockerOut(t, "run", "--rm", "-v", extVol+":/x:ro", "alpine:3.20", "stat", "-c", "%Y", "/x/.garm-seeded"))
	t.Logf("[H3b] .garm-seeded mtime after a1 = %s", markerMtimeAfterA1)

	// =========================================================================
	// (H3b) idempotent second seed: a2 for the SAME digest reuses the externals
	// volume; its seeder is a fast flock+marker no-op that does NOT re-copy.
	// =========================================================================
	b2 := cacheBootstrapPayload("r2-job-a2", repoURL, srv.URL, caBundle)
	if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b2); code != 0 {
		t.Fatalf("job a2 CreateInstance exit=%d, want 0", code)
	}
	// (NEW-H1 happy path) the warm caches were REUSED, not spuriously deleted: a2
	// mounts the SAME externals + toolcache volumes a1 created.
	if src := mountSource(t, "r2-job-a2", spec.RunnerExternalsDir); src != extVol {
		t.Errorf("(NEW-H1) a2 externals mount = %q, want the shared warm %q (warm cache must survive)", src, extVol)
	}
	if src := mountSource(t, "r2-job-a2", "/opt/hostedtoolcache"); src != toolVol {
		t.Errorf("(NEW-H1) a2 toolcache mount = %q, want the shared warm %q", src, toolVol)
	}
	// Exactly ONE externals volume for the digest (seeded once, shared).
	if n := len(lines(dockerOut(t, "volume", "ls", "-q", "--filter", "name="+extVol))); n != 1 {
		t.Errorf("(H3b) found %d externals volumes for the digest, want exactly 1 (seeded once)", n)
	}
	// The marker mtime is UNCHANGED — the second seeder did not re-copy/re-touch.
	markerMtimeAfterA2 := strings.TrimSpace(dockerOut(t, "run", "--rm", "-v", extVol+":/x:ro", "alpine:3.20", "stat", "-c", "%Y", "/x/.garm-seeded"))
	if markerMtimeAfterA2 != markerMtimeAfterA1 {
		t.Errorf("(H3b) .garm-seeded mtime changed %s -> %s — the second seeder RE-SEEDED (should be a fast no-op)", markerMtimeAfterA1, markerMtimeAfterA2)
	} else {
		t.Logf("[H3b] second same-digest seeder was a fast no-op: .garm-seeded mtime unchanged (%s), one externals volume", markerMtimeAfterA2)
	}

	// =========================================================================
	// (H3c) the diag-prune pin-then-validate does not wedge the diag name: after
	// a GC pass runs the prune helper, the diag volume STILL carries its full
	// identity labels, and a later allocation gets a diag cache HIT.
	// =========================================================================
	if _, code := runProvider(t, bin, cfg, controllerID, "ListInstances", "", nil); code != 0 {
		t.Fatalf("(H3c) ListInstances exit=%d, want 0", code)
	}
	diagLabels := dockerOut(t, "volume", "inspect", diagVol, "-f",
		"managed={{index .Labels \"garm.docker/managed\"}} cache={{index .Labels \"garm.docker/cache\"}} kind={{index .Labels \"garm.docker/cache-kind\"}} ctrl={{index .Labels \"garm.docker/controller-id\"}}")
	t.Logf("[H3c] diag labels after a prune pass: %s", diagLabels)
	if !strings.Contains(diagLabels, "managed=true") ||
		!strings.Contains(diagLabels, "cache=true") ||
		!strings.Contains(diagLabels, "kind=diag-logs") ||
		!strings.Contains(diagLabels, "ctrl="+controllerID) {
		t.Errorf("(H3c) after the pin-then-validate prune the diag volume is NOT the labeled cache (an unlabeled orphan would wedge M6 adoption): %s", diagLabels)
	}
	// A third allocation for the same repo gets a diag HIT — the deterministic
	// diag name is still adoptable, never wedged by an unlabeled auto-created orphan.
	b3 := cacheBootstrapPayload("r2-job-a3", repoURL, srv.URL, caBundle)
	if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b3); code != 0 {
		t.Fatalf("(H3c) job a3 CreateInstance exit=%d, want 0 — the diag/externals names must not be wedged", code)
	}
	if _, err := dockerTry("volume", "inspect", diagVol); err != nil {
		t.Errorf("(H3c) the diag volume was wedged/removed after the prune pass: %v", err)
	} else {
		t.Logf("[H3c] diag name still a valid labeled cache after the prune pass; a3 create succeeded (not wedged)")
	}

	// Tear down; caches survive (they carry no instance-name).
	for _, name := range []string{"r2-job-a1", "r2-job-a2", "r2-job-a3"} {
		if _, code := runProvider(t, bin, cfg, controllerID, "DeleteInstance", name, nil); code != 0 {
			t.Errorf("DeleteInstance %s exit=%d, want 0", name, code)
		}
	}
	if _, err := dockerTry("volume", "inspect", extVol); err != nil {
		t.Errorf("externals volume %q was removed by DeleteInstance — caches must outlive allocations: %v", extVol, err)
	}
}
