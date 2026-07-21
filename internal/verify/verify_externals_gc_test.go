//go:build dockerverify

package verify

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// TestVerifyM2W2ExternalsGCDiag is the M2-W2 real-daemon verification. It drives
// the REAL provider binary and proves, on a live daemon, the ADR-003 W2 contract:
//
//   - the externals volume is SEEDED ONCE (Node runtimes present) from the runner
//     image's own externals, mounted READ-ONLY (a runner `touch` in it fails),
//     and SHARED across two allocations for the same image digest;
//   - a DIFFERENT runner-image digest gets a SEPARATE externals volume;
//   - the opportunistic GC evicts a superseded-generation cache volume and keeps
//     a current one;
//   - the diagnostic-log prune helper deletes an aged file and keeps a recent one;
//   - the built runner image has a working pnpm honoring the provider's store.
//
// Isolation: a unique controller-id + unique repo_url, so cache volume names
// cannot collide with anything pre-existing. Cleanup is label-scoped to this
// controller-id plus explicit removal of this run's cache volumes; it NEVER
// touches foreign resources, asserted by a before/after volume snapshot.
//
// Run with: go test -tags dockerverify -v -run TestVerifyM2W2ExternalsGCDiag ./internal/verify/
func TestVerifyM2W2ExternalsGCDiag(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)
	token := randHex(t)
	repoURL := "https://github.com/garm-w2-verify/repo-" + token
	repoKey := spec.RepoKey(repoURL)

	// --- foreign snapshot ----------------------------------------------------
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
	volsBefore := volumeSet(t)

	// --- build the noble runner image (with externals + pnpm), then two
	// sleep-entrypoint variants so the runner stays up for inspection while still
	// carrying the real /actions-runner/externals payload. Variant B carries an
	// extra label so its CONTENT digest differs from A → a distinct externals key.
	nobleTag := "garm-w2-noble:" + token
	buildNobleRunnerImage(t, root, nobleTag)
	imageA := "garm-w2-runner-a:" + token
	imageB := "garm-w2-runner-b:" + token
	buildSleepVariant(t, nobleTag, imageA, "")
	buildSleepVariant(t, nobleTag, imageB, "LABEL garm.w2.variant=b")

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	configFor := func(image string) string {
		f := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, f, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
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
		return f
	}
	configA := configFor(imageA)
	configB := configFor(imageB)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	digestA := imageDigestHex(t, imageA)
	digestB := imageDigestHex(t, imageB)
	extVolA := spec.ExternalsVolumeName(digestA)
	extVolB := spec.ExternalsVolumeName(digestB)
	diagVol := spec.DiagVolumeName(repoKey)
	t.Logf("[setup] controller=%s repoKey=%s\n  extVolA=%s\n  extVolB=%s", controllerID, repoKey, extVolA, extVolB)

	defer func() {
		cleanupController(t, controllerID)
		for _, v := range []string{extVolA, extVolB, diagVol} {
			_, _ = dockerTry("volume", "rm", "-f", v)
		}
		for _, img := range []string{imageA, imageB, nobleTag} {
			_, _ = dockerTry("rmi", "-f", img)
		}
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
	// (1) externals seeded once + RO + shared across two same-digest allocations
	// =========================================================================
	bA1 := cacheBootstrapPayload("w2-job-a1", repoURL, srv.URL, caBundle)
	if out, code := runProvider(t, bin, configA, controllerID, "CreateInstance", "", &bA1); code != 0 {
		t.Fatalf("job a1 CreateInstance exit=%d, want 0; stdout=%s", code, out)
	}
	bA2 := cacheBootstrapPayload("w2-job-a2", repoURL, srv.URL, caBundle)
	if _, code := runProvider(t, bin, configA, controllerID, "CreateInstance", "", &bA2); code != 0 {
		t.Fatalf("job a2 CreateInstance exit=%d, want 0", code)
	}

	// The externals volume exists with the ADR-003 externals label set and NO
	// instance-name (so teardown/sweep never touches it) and NO repo label
	// (shared across repos).
	extLabels := dockerOut(t, "volume", "inspect", extVolA, "-f",
		"cache={{index .Labels \"garm.docker/cache\"}} kind={{index .Labels \"garm.docker/cache-kind\"}} digest={{index .Labels \"garm.docker/image-digest\"}} inst={{index .Labels \"garm.docker/instance-name\"}} repo={{index .Labels \"garm.docker/repo\"}}")
	t.Logf("[1] externals labels: %s", extLabels)
	if !strings.Contains(extLabels, "cache=true") || !strings.Contains(extLabels, "kind=externals") || !strings.Contains(extLabels, "digest="+digestA) {
		t.Errorf("(1) externals volume labels missing/incorrect: %s", extLabels)
	}
	for _, forbidden := range []string{"inst=w2-job", "repo=" + repoKey} {
		if strings.Contains(extLabels, forbidden) {
			t.Errorf("(1) externals volume must not carry %q: %s", forbidden, extLabels)
		}
	}

	// Seeded ONCE: the volume holds the runner's Node runtimes plus the .seeded
	// marker (copied from the image's own /actions-runner/externals).
	seededLs := dockerOut(t, "run", "--rm", "-v", extVolA+":/x:ro", "alpine:3.20", "ls", "-A", "/x")
	t.Logf("[1] externals volume contents: %s", strings.ReplaceAll(seededLs, "\n", " "))
	if !strings.Contains(seededLs, "node20") || !strings.Contains(seededLs, ".garm-seeded") {
		t.Errorf("(1) externals volume not seeded with Node runtimes + marker: %s", seededLs)
	}

	// Both allocations mount the SAME externals volume, READ-ONLY.
	for _, runner := range []string{"w2-job-a1", "w2-job-a2"} {
		if src := mountSource(t, runner, spec.RunnerExternalsDir); src != extVolA {
			t.Errorf("(1) %s externals mount = %q, want the shared %q", runner, src, extVolA)
		}
	}
	// The runner CANNOT write the externals (red-line F4): a touch fails.
	if out, err := dockerTry("exec", "w2-job-a1", "sh", "-c", "touch "+spec.RunnerExternalsDir+"/evil 2>&1"); err == nil {
		t.Errorf("(1) runner WROTE to the read-only externals mount — cross-repo RCE guard breached: %s", out)
	} else {
		t.Logf("[1] runner write to externals correctly denied (read-only): %s", strings.TrimSpace(out))
	}

	// Exactly one externals volume for digest A (seeded once, shared).
	if n := len(lines(dockerOut(t, "volume", "ls", "-q", "--filter", "name="+extVolA))); n != 1 {
		t.Errorf("(1) found %d externals volumes for digest A, want exactly 1 (seeded once, shared)", n)
	}

	// =========================================================================
	// (2) a DIFFERENT image digest → a SEPARATE externals volume
	// =========================================================================
	bB1 := cacheBootstrapPayload("w2-job-b1", repoURL, srv.URL, caBundle)
	if _, code := runProvider(t, bin, configB, controllerID, "CreateInstance", "", &bB1); code != 0 {
		t.Fatalf("job b1 CreateInstance exit=%d, want 0", code)
	}
	if digestA == digestB {
		t.Fatal("(2) image A and B share a content digest — the variant build failed to differ")
	}
	if _, err := dockerTry("volume", "inspect", extVolB); err != nil {
		t.Errorf("(2) a different image digest did NOT get its own externals volume %q: %v", extVolB, err)
	} else {
		t.Logf("[2] distinct digest B got a separate externals volume %s", extVolB)
	}
	if src := mountSource(t, "w2-job-b1", spec.RunnerExternalsDir); src != extVolB {
		t.Errorf("(2) job b1 externals mount = %q, want its own %q", src, extVolB)
	}

	// =========================================================================
	// (3) GC evicts a superseded-generation cache, keeps a current one
	// =========================================================================
	// A current toolcache (generation "1") already exists from job a1 and is
	// mounted in a live runner. Pre-create a SUPERSEDED one (generation "old",
	// created 2h ago, past the 30-min grace) for the same controller/repo.
	supersededVol := "garm-cache-toolcache-" + repoKey + "-old"
	twoHoursAgo := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	if out, err := dockerTry("volume", "create",
		"--label", "garm.docker/managed=true",
		"--label", "garm.docker/controller-id="+controllerID,
		"--label", "garm.docker/cache=true",
		"--label", "garm.docker/cache-kind=toolcache",
		"--label", "garm.docker/repo="+repoKey,
		"--label", "garm.docker/generation=old",
		"--label", "garm.docker/last-used="+twoHoursAgo,
		supersededVol); err != nil {
		t.Fatalf("(3) seed superseded volume: %v\n%s", err, out)
	}
	currentVol := spec.ToolcacheVolumeName(repoKey, "1")

	// Trigger an opportunistic GC pass via ListInstances (it runs runCacheGC).
	if _, code := runProvider(t, bin, configA, controllerID, "ListInstances", "", nil); code != 0 {
		t.Fatalf("(3) ListInstances exit=%d, want 0", code)
	}
	if _, err := dockerTry("volume", "inspect", supersededVol); err == nil {
		t.Errorf("(3) GC did NOT evict the superseded-generation volume %q", supersededVol)
	} else {
		t.Logf("[3] GC evicted the superseded-generation cache volume %s", supersededVol)
	}
	if _, err := dockerTry("volume", "inspect", currentVol); err != nil {
		t.Errorf("(3) GC evicted the CURRENT-generation toolcache %q — it must survive: %v", currentVol, err)
	} else {
		t.Logf("[3] the current-generation toolcache %s survived GC", currentVol)
	}

	// =========================================================================
	// (4) the diag-prune helper deletes an aged file, keeps a recent one
	// =========================================================================
	if _, err := dockerTry("volume", "inspect", diagVol); err != nil {
		t.Fatalf("(4) no diag volume %q was created by the repo-scoped allocation: %v", diagVol, err)
	}
	// Write an OLD file (mtime 10 days ago, past the 7-day retention) and a RECENT
	// one into the diag volume. Busybox `touch -t` takes an absolute [[CC]YY]
	// MMDDhhmm stamp (it does not parse relative "10 days ago"), so compute the
	// stamp here rather than in the container.
	oldStamp := time.Now().AddDate(0, 0, -10).Format("200601021504")
	if out, err := dockerTry("run", "--rm", "-v", diagVol+":/logs", "alpine:3.20", "sh", "-c",
		"touch -t "+oldStamp+" /logs/old.log && touch /logs/recent.log && ls -la /logs"); err != nil {
		t.Fatalf("(4) seed diag files: %v\n%s", err, out)
	}
	// Trigger the GC/prune pass again.
	if _, code := runProvider(t, bin, configA, controllerID, "ListInstances", "", nil); code != 0 {
		t.Fatalf("(4) ListInstances exit=%d, want 0", code)
	}
	diagLs := dockerOut(t, "run", "--rm", "-v", diagVol+":/logs", "alpine:3.20", "ls", "-A", "/logs")
	t.Logf("[4] diag volume after prune: %s", strings.ReplaceAll(diagLs, "\n", " "))
	if strings.Contains(diagLs, "old.log") {
		t.Errorf("(4) the diag prune did NOT delete the aged file old.log: %s", diagLs)
	}
	if !strings.Contains(diagLs, "recent.log") {
		t.Errorf("(4) the diag prune wrongly deleted the recent file recent.log: %s", diagLs)
	}

	// =========================================================================
	// (5) the built runner image has a working pnpm honoring the store env
	// =========================================================================
	// --platform is passed so the amd64-on-arm64 host does not emit a platform
	// WARNING to stderr (which dockerOut merges into stdout); lastLine takes the
	// command's real output line regardless of any residual daemon noise.
	pnpmVer := lastLine(dockerOut(t, "run", "--rm", "--platform", "linux/amd64", "--entrypoint", "pnpm", imageA, "--version"))
	if pnpmVer == "" {
		t.Error("(5) `pnpm --version` produced no output in the runner image")
	} else {
		t.Logf("[5] runner image pnpm --version = %s", pnpmVer)
	}
	storeDir := lastLine(dockerOut(t, "run", "--rm", "--platform", "linux/amd64", "-e", "npm_config_store_dir=/opt/pnpm-store",
		"--entrypoint", "pnpm", imageA, "config", "get", "store-dir"))
	if storeDir != "/opt/pnpm-store" {
		t.Errorf("(5) pnpm config get store-dir = %q, want /opt/pnpm-store", storeDir)
	} else {
		t.Logf("[5] pnpm honors the provider store: config get store-dir = /opt/pnpm-store")
	}

	// Tear the allocations down (their caches survive; the deferred cleanup
	// removes this run's cache volumes explicitly).
	for _, name := range []string{"w2-job-a1", "w2-job-a2", "w2-job-b1"} {
		if _, code := runProvider(t, bin, configA, controllerID, "DeleteInstance", name, nil); code != 0 {
			t.Errorf("DeleteInstance %s exit=%d, want 0", name, code)
		}
	}
	// The externals volumes must survive DeleteInstance (they carry no instance-name).
	for _, v := range []string{extVolA, extVolB} {
		if _, err := dockerTry("volume", "inspect", v); err != nil {
			t.Errorf("externals volume %q was removed by DeleteInstance — caches must outlive allocations: %v", v, err)
		}
	}
}

// buildNobleRunnerImage builds the repo's real noble runner image (with the
// externals payload and pnpm) at the given tag, linux/amd64.
func buildNobleRunnerImage(t *testing.T, root, tag string) {
	t.Helper()
	cmd := exec.Command("docker", "build", "--platform", "linux/amd64", "-t", tag, filepath.Join(root, "runner-images", "noble"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build noble runner image: %v\n%s", err, out)
	}
}

// buildSleepVariant builds a sleep-entrypoint image FROM baseTag (so it inherits
// the real /actions-runner/externals payload and pnpm) that stays running for
// inspection. extraLine, when non-empty, is inserted so a caller can perturb the
// image's content digest (a distinct externals key).
func buildSleepVariant(t *testing.T, baseTag, newTag, extraLine string) {
	t.Helper()
	dir := t.TempDir()
	df := "FROM " + baseTag + "\n"
	if extraLine != "" {
		df += extraLine + "\n"
	}
	df += "ENTRYPOINT [\"sleep\", \"infinity\"]\n"
	writeFile(t, filepath.Join(dir, "Dockerfile"), df)
	cmd := exec.Command("docker", "build", "--platform", "linux/amd64", "-t", newTag, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sleep variant %s: %v\n%s", newTag, err, out)
	}
}

// lastLine returns the last non-empty line of s (trimmed), so a command's real
// output survives any leading daemon warning dockerOut merged from stderr.
func lastLine(s string) string {
	ls := lines(s)
	if len(ls) == 0 {
		return ""
	}
	return strings.TrimSpace(ls[len(ls)-1])
}

// imageDigestHex returns a local image's content-ID hex (the "sha256:" prefix
// stripped) — the exact token the provider keys the externals volume on.
func imageDigestHex(t *testing.T, image string) string {
	t.Helper()
	id := strings.TrimSpace(dockerOut(t, "image", "inspect", image, "-f", "{{.Id}}"))
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id = id[i+1:]
	}
	if id == "" {
		t.Fatalf("image %q has no content ID", image)
	}
	return id
}
