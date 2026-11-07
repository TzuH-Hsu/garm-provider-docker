//go:build dockerverify

// Runs a real `pnpm install` as the non-root runner user (uid/gid 1001) into the
// provider-created pnpm store volume. A root-run or config-only check would mask the
// problem: a fresh, root-owned cache volume must be made writable by the
// unprivileged runner before the gosu privilege drop, so `pnpm add` succeeds and
// populates the store instead of failing with EACCES.
//
//	go test -tags dockerverify -v -run TestVerifyNonRootPnpmInstall ./internal/verify/
//
// It uses a custom [cache].pnpm_store_path that the runner Dockerfile does not
// create, so Moby's image-ownership copy cannot help and only the entrypoint's
// prepare_cache_dirs chown (run verbatim by sourcing the real entrypoint) can make
// the fresh volume writable. That shows the fix handles the operator-configurable
// store path, not just the baked-in default. Everything is scoped to a unique
// controller ID and torn down by label, and a before/after snapshot of host
// containers and volumes checks that no foreign resource was touched.
package verify

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const (
	pnpmInstance  = "pnpm-verify-01"
	pnpmStorePath = "/opt/garm-pnpm-custom" // deliberately NOT created in the Dockerfile
)

// buildPnpmWrapperImage layers a wrapper on the production runner image
// (runner-images/noble: its real Dockerfile and entrypoint.sh). The wrapper sources
// the real /entrypoint.sh, calls the real prepare_cache_dirs under that file's own
// `set -euo pipefail`, then sleeps as PID 1 so the test can `docker exec -u runner`
// a real pnpm install. Without the chown in prepare_cache_dirs, the fresh root-owned
// store volume would stay root-owned and the non-root `pnpm add` would fail with
// EACCES.
func buildPnpmWrapperImage(t *testing.T, root, tag string) {
	t.Helper()

	nobleTag := "garm-pnpm-noble-real:latest"
	if out, err := dockerTry(append([]string{"build", "--platform", "linux/amd64", "-t", nobleTag}, runnerImageBuildArgs(root, "noble")...)...); err != nil {
		t.Fatalf("build real runner-images/noble image: %v\n%s", err, out)
	}

	dir := t.TempDir()
	wrapper := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"# Source the REAL production entrypoint (main is guarded not to run when\n" +
		"# sourced) and invoke the REAL prepare_cache_dirs under its own\n" +
		"# `set -euo pipefail`: this IS the pnpm-store chown code path, verbatim, against the\n" +
		"# provider-set npm_config_store_dir / RUNNER_TOOL_CACHE.\n" +
		"# shellcheck source=/dev/null\n" +
		"source /entrypoint.sh\n" +
		"prepare_cache_dirs\n" +
		"mkdir -p /actions-runner/_work\n" +
		"chown runner:runner /actions-runner/_work\n" +
		"exec sleep infinity\n"
	writeFile(t, filepath.Join(dir, "test-entrypoint.sh"), wrapper)
	dockerfile := "FROM " + nobleTag + "\n" +
		"COPY --chmod=0755 test-entrypoint.sh /test-entrypoint.sh\n" +
		"ENTRYPOINT [\"/test-entrypoint.sh\"]\n"
	writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
	if out, err := dockerTry("build", "--platform", "linux/amd64", "-t", tag, dir); err != nil {
		t.Fatalf("build pnpm wrapper image: %v\n%s", out, err)
	}
}

func TestVerifyNonRootPnpmInstall(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)
	token := randHex(t)
	repoURL := "https://github.com/garm-pnpm-verify/repo-" + token
	repoKey := spec.RepoKey(repoURL)

	// Foreign snapshot, compared after the run.
	canaries := ensureForeignCanaries(t)
	assertForeignPresent(t, canaries[0])
	assertForeignPresent(t, canaries[1])
	beforeC := snapshotIDs(t, "ps", "-a", "--format", "{{.Names}}")
	beforeV := snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}")

	runnerTag := "garm-pnpm-verify-cli:" + token
	buildPnpmWrapperImage(t, root, runnerTag)
	osKey := imageOSKey(t, runnerTag)
	pnpmVol := spec.PnpmVolumeName(repoKey, osKey, "9")

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// A custom pnpm_store_path that the Dockerfile does not create, so only the
	// entrypoint chown can make the fresh, root-owned volume writable.
	configFile := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, configFile, strings.Join([]string{
		`docker_host = "unix:///var/run/docker.sock"`,
		`runner_image = "` + runnerTag + `"`,
		`allow_unpinned_runner_image = true`,
		`[cache]`,
		`enabled = true`,
		`generation = "1"`,
		`pnpm_major = "9"`,
		`toolcache_path = "/opt/hostedtoolcache"`,
		`pnpm_store_path = "` + pnpmStorePath + `"`,
		"",
	}, "\n"))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		_, _ = dockerTry("volume", "rm", "-f", pnpmVol, spec.ToolcacheVolumeName(repoKey, osKey, "1"), spec.DiagVolumeName(repoKey))
		_, _ = dockerTry("rmi", "-f", runnerTag)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
		assertSnapshotIdentical(t, "containers", beforeC, "ps", "-a", "--format", "{{.Names}}")
		// Cache volumes we created are removed above; assert no FOREIGN volume vanished.
		afterV := map[string]bool{}
		for _, n := range snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}") {
			afterV[n] = true
		}
		for _, n := range beforeV {
			if !afterV[n] {
				t.Errorf("[cleanup] foreign volume %q was removed by this test — cleanup must be label-scoped", n)
			}
		}
	}()

	// CreateInstance (repo-scoped, cache enabled): the provider creates the pnpm store
	// volume, mounts it at the custom path and sets npm_config_store_dir.
	b := cacheBootstrapPayload(pnpmInstance, repoURL, srv.URL, caBundle)
	stdout, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, stdout)
	}
	runner := spec.RunnerContainerName(pnpmInstance)

	if src := mountSource(t, runner, pnpmStorePath); src != pnpmVol {
		t.Fatalf("pnpm store mount at %s = %q, want the provider volume %q", pnpmStorePath, src, pnpmVol)
	}
	if !strings.Contains(containerEnvVerify(t, runner), "npm_config_store_dir="+pnpmStorePath) {
		t.Errorf("runner is missing npm_config_store_dir=%s", pnpmStorePath)
	}
	if !strings.Contains(containerEnvVerify(t, runner), "pnpm_config_store_dir="+pnpmStorePath) {
		t.Errorf("runner is missing pnpm_config_store_dir=%s", pnpmStorePath)
	}

	// The metadata cache envs point at a SUB-directory of the store mount (never
	// its root: `pnpm store prune` would otherwise be able to delete the store).
	wantCacheDir := pnpmStorePath + "/" + spec.PnpmMetadataCacheSubdir
	for _, name := range []string{spec.PnpmCacheDirEnvNpm, spec.PnpmCacheDirEnvPnpm} {
		if !strings.Contains(containerEnvVerify(t, runner), name+"="+wantCacheDir+"\n") {
			t.Errorf("runner is missing %s=%s", name, wantCacheDir)
		}
	}
	// pnpm, running as the runner user inside the real runner image, resolves
	// cache-dir to that sub-directory (the env is honored, not overridden).
	if got := strings.TrimSpace(mustExecRunner(t, runner, "pnpm", "config", "get", "cache-dir")); got != wantCacheDir {
		t.Errorf("[cache-dir] `pnpm config get cache-dir` = %q, want %q", got, wantCacheDir)
	} else {
		t.Logf("[cache-dir] `pnpm config get cache-dir` = %s", got)
	}

	// The store dir must be owned by the runner user: the entrypoint chown ran on the
	// fresh, root-owned volume before privileges were dropped.
	owner := strings.TrimSpace(mustExecRunner(t, runner, "stat", "-c", "%U", pnpmStorePath))
	if owner != "runner" {
		t.Fatalf("[pnpm] pnpm store %s is owned by %q, want runner — the entrypoint chown did not run on the fresh root-owned volume", pnpmStorePath, owner)
	}
	t.Logf("[pnpm] pnpm store %s is runner-owned before the privilege drop", pnpmStorePath)

	// The proof: a real `pnpm add` as the non-root runner into the provider's store.
	// If the fresh volume were still root-owned, this would fail with EACCES.
	install := "set -e; d=$(mktemp -d); cd \"$d\"; " +
		"printf '{\"name\":\"pnpm-verify\",\"version\":\"1.0.0\"}' > package.json; " +
		"pnpm add is-number@7.0.0; " +
		"test -e node_modules/is-number/package.json; " +
		"echo H4_PNPM_INSTALL_OK"
	out, err := execRunner(runner, "sh", "-c", install)
	if err != nil {
		t.Fatalf("[pnpm] real `pnpm add` as the NON-ROOT runner FAILED (the EACCES the fix closes): %v\n%s", err, out)
	}
	if !strings.Contains(out, "H4_PNPM_INSTALL_OK") {
		t.Fatalf("[pnpm] pnpm install did not complete: %s", out)
	}
	t.Logf("[pnpm] real `pnpm add is-number` as the non-root runner SUCCEEDED")

	// The provider-created store volume is now POPULATED (the content-addressed
	// store holds the fetched package), not empty.
	storeLs := mustExecRunner(t, runner, "sh", "-c", "ls -A "+pnpmStorePath)
	if strings.TrimSpace(storeLs) == "" {
		t.Errorf("[pnpm] the pnpm store %s is still empty after a successful install", pnpmStorePath)
	} else {
		t.Logf("[pnpm] provider pnpm store %s populated by the non-root install: %s", pnpmStorePath, strings.ReplaceAll(strings.TrimSpace(storeLs), "\n", " "))
	}

	// The install, run as the unprivileged runner, created the metadata cache
	// sub-directory itself (the entrypoint only owns the store mount) and wrote
	// package metadata into it; the sub-directory is runner-owned.
	cacheOwner := strings.TrimSpace(mustExecRunner(t, runner, "stat", "-c", "%U", wantCacheDir))
	if cacheOwner != "runner" {
		t.Errorf("[cache-dir] %s is owned by %q, want runner", wantCacheDir, cacheOwner)
	}
	cacheLs := mustExecRunner(t, runner, "sh", "-c", "ls -A "+wantCacheDir)
	if strings.TrimSpace(cacheLs) == "" {
		t.Errorf("[cache-dir] metadata cache %s is empty after a successful install", wantCacheDir)
	} else {
		t.Logf("[cache-dir] metadata cache %s written by the non-root install: %s", wantCacheDir, strings.ReplaceAll(strings.TrimSpace(cacheLs), "\n", " "))
	}

	if _, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", pnpmInstance, nil); delCode != 0 {
		t.Errorf("DeleteInstance exit=%d, want 0", delCode)
	}
	// The pnpm cache volume must SURVIVE DeleteInstance (it carries no instance-name).
	if _, err := dockerTry("volume", "inspect", pnpmVol); err != nil {
		t.Errorf("pnpm store volume %q was removed by DeleteInstance — caches must outlive allocations: %v", pnpmVol, err)
	}
}

// mustExecRunner runs a command AS THE NON-ROOT runner user, failing on error.
func mustExecRunner(t *testing.T, container string, args ...string) string {
	t.Helper()
	out, err := execRunner(container, args...)
	if err != nil {
		t.Fatalf("exec (as runner) %v failed: %v\n%s", args, err, out)
	}
	return out
}

// containerEnvVerify returns a container's environment, newline-joined.
func containerEnvVerify(t *testing.T, container string) string {
	t.Helper()
	return dockerOut(t, "inspect", container, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
}
