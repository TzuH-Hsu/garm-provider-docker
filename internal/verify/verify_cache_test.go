//go:build dockerverify

package verify

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// TestVerifyM2W1CacheHit is the M2-W1 real-daemon verification: it drives the
// REAL provider binary through TWO allocations for the SAME repo_url and proves
// the ADR-003 persistent-cache contract on a live daemon — the toolcache and
// pnpm store volumes are the SAME volume across both jobs (a file written into
// the toolcache in job 1 is visible in job 2), they are NOT torn down when each
// allocation is deleted, an org-scoped pool gets no persistent cache by default,
// and the pnpm store path the provider points pnpm at is honored end to end.
//
// Isolation: a UNIQUE controller-id AND a UNIQUE repo_url (random token) so the
// cache repokey — and therefore the cache volume names — cannot collide with any
// pre-existing volume. Cleanup is label-scoped to this controller-id (which the
// cache volumes carry) plus an explicit removal of this run's cache volumes by
// name; it NEVER touches foreign resources, and a before/after volume snapshot
// asserts no foreign volume was removed.
//
// Run with: go test -tags dockerverify -v -run TestVerifyM2W1CacheHit ./internal/verify/
func TestVerifyM2W1CacheHit(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	// Unique repo (two path segments => repo-scoped) and a unique org (one
	// segment => org-scoped), each with a random token so their repokeys are
	// unique to this run.
	token := randHex(t)
	repoURL := "https://github.com/garm-cache-verify/repo-" + token
	orgURL := "https://github.com/garm-cache-verify-org-" + token
	repoKey := spec.RepoKey(repoURL)
	orgKey := spec.RepoKey(orgURL)
	toolVol := spec.ToolcacheVolumeName(repoKey, "1")
	pnpmVol := spec.PnpmVolumeName(repoKey, "9")
	orgToolVol := spec.ToolcacheVolumeName(orgKey, "1")
	t.Logf("[setup] controller=%s repoKey=%s\n  toolVol=%s\n  pnpmVol=%s", controllerID, repoKey, toolVol, pnpmVol)

	// --- foreign snapshot: containers AND volumes must be untouched -----------
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
	volsBefore := volumeSet(t)

	// --- build sleep runner image + the REAL provider binary -----------------
	imageTag := "garm-m2w1-verify-sleep:latest"
	buildSleepImage(t, imageTag)
	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// --- provider config with the persistent cache enabled -------------------
	configFile := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, configFile, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true

[cache]
enabled = true
generation = "1"
pnpm_major = "9"
toolcache_path = "/opt/hostedtoolcache"
pnpm_store_path = "/opt/pnpm-store"
allow_org_shared = false
`, imageTag))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	// --- cleanup: label-scoped to THIS controller (cache volumes carry the
	// controller-id label too) + explicit removal of this run's cache volumes by
	// name, then assert no FOREIGN volume was removed and the foreign containers
	// are intact. Caches are meant to persist across allocations, so the test
	// itself is what removes them, at the very end.
	defer func() {
		cleanupController(t, controllerID)
		for _, v := range []string{toolVol, pnpmVol, orgToolVol} {
			_, _ = dockerTry("volume", "rm", "-f", v)
		}
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
		volsAfter := volumeSet(t)
		for name := range volsBefore {
			if !volsAfter[name] {
				t.Errorf("[cleanup] foreign volume %q was removed by this test — cleanup must be label-scoped and never touch foreign volumes", name)
			}
		}
		// Our cache volumes must be gone (the test cleaned them up).
		for _, v := range []string{toolVol, pnpmVol} {
			if volsAfter[v] {
				t.Errorf("[cleanup] this run's cache volume %q was left behind", v)
			}
		}
	}()

	// =========================================================================
	// JOB A: create -> caches provisioned, mounted, env set; write a marker.
	// =========================================================================
	bA := cacheBootstrapPayload("m2w1-job-a", repoURL, srv.URL, caBundle)
	stdout, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &bA)
	if code != 0 {
		t.Fatalf("job A CreateInstance exit=%d, want 0; stdout=%s", code, stdout)
	}
	runnerA := "m2w1-job-a"

	// (a) toolcache + pnpm volumes exist with the ADR-003 cache labels and NO
	// instance-name (so they are excluded from teardown).
	assertCacheVolumeLabels(t, toolVol, repoKey, string(spec.CacheKindToolcache))
	assertCacheVolumeLabels(t, pnpmVol, repoKey, string(spec.CacheKindPnpm))

	// (b) both are mounted at their configured paths in job A's runner.
	if src := mountSource(t, runnerA, "/opt/hostedtoolcache"); src != toolVol {
		t.Errorf("(b) job A toolcache mount source = %q, want %q", src, toolVol)
	}
	if src := mountSource(t, runnerA, "/opt/pnpm-store"); src != pnpmVol {
		t.Errorf("(b) job A pnpm mount source = %q, want %q", src, pnpmVol)
	}

	// (c) cache env is set.
	envA := containerEnv(t, runnerA)
	if !strings.Contains(envA, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
		t.Errorf("(c) job A missing RUNNER_TOOL_CACHE")
	}
	if !strings.Contains(envA, "npm_config_store_dir=/opt/pnpm-store") {
		t.Errorf("(c) job A missing npm_config_store_dir")
	}

	// (d) write a marker into the toolcache volume through job A's runner.
	marker := "cachehit-" + randHex(t)
	if out, err := dockerTry("exec", runnerA, "sh", "-c", "echo "+marker+" > /opt/hostedtoolcache/marker.txt && sync"); err != nil {
		t.Fatalf("(d) writing toolcache marker in job A failed: %v\n%s", err, out)
	}
	t.Logf("[d] wrote marker %q into the toolcache volume via job A's runner", marker)

	// =========================================================================
	// Delete job A: the allocation is torn down but the caches SURVIVE.
	// =========================================================================
	if _, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", runnerA, nil); delCode != 0 {
		t.Errorf("job A DeleteInstance exit=%d, want 0", delCode)
	}
	if _, err := dockerTry("inspect", runnerA); err == nil {
		t.Error("job A runner container still present after DeleteInstance")
	}
	for _, v := range []string{toolVol, pnpmVol} {
		if _, err := dockerTry("volume", "inspect", v); err != nil {
			t.Fatalf("cache volume %q was removed by DeleteInstance — caches must outlive allocations: %v", v, err)
		}
	}
	t.Logf("[delete] job A allocation torn down; both cache volumes survived")

	// =========================================================================
	// JOB B: same repo_url -> SAME cache volumes; marker from job A is visible.
	// =========================================================================
	bB := cacheBootstrapPayload("m2w1-job-b", repoURL, srv.URL, caBundle)
	if _, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &bB); code != 0 {
		t.Fatalf("job B CreateInstance exit=%d, want 0", code)
	}
	runnerB := "m2w1-job-b"

	if src := mountSource(t, runnerB, "/opt/hostedtoolcache"); src != toolVol {
		t.Errorf("job B toolcache mount source = %q, want the SAME volume %q as job A", src, toolVol)
	}
	if src := mountSource(t, runnerB, "/opt/pnpm-store"); src != pnpmVol {
		t.Errorf("job B pnpm mount source = %q, want the SAME volume %q as job A", src, pnpmVol)
	}

	// THE cache-hit proof: the marker written in job A is present in job B,
	// because both jobs mounted the SAME persistent toolcache volume.
	got, err := dockerTry("exec", runnerB, "cat", "/opt/hostedtoolcache/marker.txt")
	if err != nil {
		t.Fatalf("cache MISS: job A's toolcache marker is not readable in job B: %v\n%s", err, got)
	}
	if strings.TrimSpace(got) != marker {
		t.Errorf("cache MISS: job B read %q from the toolcache, want %q (the marker job A wrote)", strings.TrimSpace(got), marker)
	} else {
		t.Logf("[cache-hit] job B read the marker job A wrote into the SAME toolcache volume: %q", marker)
	}

	// Exactly one toolcache + one pnpm + one diag volume for this repokey (job B
	// reused job A's; it did not create a second of any repo-keyed kind). The
	// shared externals volume carries no repo label, so it is not counted here.
	if n := len(lines(dockerOut(t, "volume", "ls", "-q", "--filter", "label=garm.docker/repo="+repoKey))); n != 3 {
		t.Errorf("repo has %d repo-keyed cache volumes, want 3 (toolcache + pnpm + diag, reused across both jobs)", n)
	}

	// =========================================================================
	// pnpm store path end-to-end: pnpm honors the provider's volume + env.
	// (Runs pnpm against the PROVIDER-created pnpm volume; needs network for
	// corepack, which this dockerverify suite already requires — best-effort so
	// a transient corepack/network failure does not mask the provider result.)
	// =========================================================================
	pnpmOut, pnpmErr := dockerTry("run", "--rm",
		"-e", "npm_config_store_dir=/opt/pnpm-store",
		"-e", "COREPACK_HOME=/tmp/corepack",
		"-v", pnpmVol+":/opt/pnpm-store",
		"node:20-alpine", "sh", "-c",
		"corepack enable && corepack prepare pnpm@9.15.9 --activate >/dev/null 2>&1 && pnpm config get store-dir")
	switch {
	case pnpmErr != nil:
		t.Logf("[pnpm-e2e] best-effort store-dir check skipped (corepack/network): %v\n%s", pnpmErr, pnpmOut)
	case strings.TrimSpace(pnpmOut) != "/opt/pnpm-store":
		t.Errorf("[pnpm-e2e] pnpm config get store-dir = %q on the provider's pnpm volume, want /opt/pnpm-store", strings.TrimSpace(pnpmOut))
	default:
		t.Logf("[pnpm-e2e] pnpm honors the provider's store: `pnpm config get store-dir` = /opt/pnpm-store on the provider-created volume %s", pnpmVol)
	}

	// =========================================================================
	// ORG scope: a single-segment repo_url gets NO persistent cache by default.
	// =========================================================================
	bOrg := cacheBootstrapPayload("m2w1-job-org", orgURL, srv.URL, caBundle)
	if _, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &bOrg); code != 0 {
		t.Fatalf("job org CreateInstance exit=%d, want 0", code)
	}
	runnerOrg := "m2w1-job-org"
	if _, err := dockerTry("volume", "inspect", orgToolVol); err == nil {
		t.Errorf("org-scoped pool created a persistent toolcache volume %q without allow_org_shared", orgToolVol)
	}
	if src := mountSource(t, runnerOrg, "/opt/pnpm-store"); src != "" {
		t.Errorf("org-scoped pool mounted a persistent pnpm store %q without opt-in", src)
	}
	envOrg := containerEnv(t, runnerOrg)
	if !strings.Contains(envOrg, "RUNNER_TOOL_CACHE=/opt/hostedtoolcache") {
		t.Error("org-scoped runner should still get an ephemeral RUNNER_TOOL_CACHE")
	}
	if strings.Contains(envOrg, "npm_config_store_dir=") {
		t.Error("org-scoped runner must NOT get npm_config_store_dir (no persistent pnpm store)")
	}
	if n := len(lines(dockerOut(t, "volume", "ls", "-q", "--filter", "label=garm.docker/repo="+orgKey))); n != 0 {
		t.Errorf("org pool produced %d cache volumes, want 0 (withheld by default)", n)
	}
	t.Logf("[org] org-scoped pool got no persistent cache (RUNNER_TOOL_CACHE ephemeral, no pnpm store)")

	if _, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", runnerOrg, nil); delCode != 0 {
		t.Errorf("job org DeleteInstance exit=%d, want 0", delCode)
	}
}

// --- helpers -----------------------------------------------------------------

func randHex(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func cacheBootstrapPayload(name, repoURL, metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             name,
		RepoURL:          repoURL,
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken,
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           hostOSArch(),
		PoolID:           poolID,
		JitConfigEnabled: true,
	}
}

// volumeSet returns the set of all volume names on the host, for the
// foreign-non-interference before/after snapshot.
func volumeSet(t *testing.T) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	for _, n := range lines(dockerOut(t, "volume", "ls", "-q")) {
		set[n] = true
	}
	return set
}

// assertCacheVolumeLabels asserts a cache volume carries the ADR-003 label set
// (managed+cache+repo+cache-kind) and NO instance-name (the exclusion marker).
func assertCacheVolumeLabels(t *testing.T, name, repoKey, kind string) {
	t.Helper()
	labels := dockerOut(t, "volume", "inspect", name, "-f",
		"managed={{index .Labels \"garm.docker/managed\"}} cache={{index .Labels \"garm.docker/cache\"}} repo={{index .Labels \"garm.docker/repo\"}} kind={{index .Labels \"garm.docker/cache-kind\"}}")
	t.Logf("[cache-vol] %s labels: %s", name, labels)
	if !strings.Contains(labels, "managed=true") ||
		!strings.Contains(labels, "cache=true") ||
		!strings.Contains(labels, "repo="+repoKey) ||
		!strings.Contains(labels, "kind="+kind) {
		t.Errorf("cache volume %q labels missing/incorrect: %s", name, labels)
	}
	// The cache volume must NOT carry an instance-name (the ADR-004 teardown
	// exclusion). A label absent from the map renders empty via `index` for a
	// map[string]string, so an empty value here proves the key is not set.
	inst := strings.TrimSpace(dockerOut(t, "volume", "inspect", name, "-f",
		"{{index .Labels \"garm.docker/instance-name\"}}"))
	if inst != "" && inst != "<no value>" {
		t.Errorf("cache volume %q must NOT carry an instance-name label, got %q", name, inst)
	}
}

// mountSource returns the volume name mounted at dest in container, or "" if
// nothing is mounted there.
func mountSource(t *testing.T, container, dest string) string {
	t.Helper()
	return strings.TrimSpace(dockerOut(t, "inspect", container, "-f",
		"{{range .Mounts}}{{if eq .Destination \""+dest+"\"}}{{.Name}}{{end}}{{end}}"))
}

// containerEnv returns a container's full environment, newline-joined.
func containerEnv(t *testing.T, container string) string {
	t.Helper()
	return dockerOut(t, "inspect", container, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
}
