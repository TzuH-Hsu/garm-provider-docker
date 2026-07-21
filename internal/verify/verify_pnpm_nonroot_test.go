//go:build dockerverify

// This file adds the M2 H4 real-daemon verification, run AS THE ACTUAL NON-ROOT
// runner user (uid/gid 1001) doing a REAL `pnpm install` into the provider-created
// pnpm store volume — the exact condition the earlier root-run / config-only
// checks masked (the M1 F1/F2 root-masking lesson). It proves that a fresh,
// root-owned cache volume is made writable by the unprivileged runner BEFORE the
// gosu privilege drop, so `pnpm add` succeeds and populates the store rather than
// failing EACCES.
//
//	go test -tags dockerverify -v -run TestVerifyM2H4NonRootPnpmInstall ./internal/verify/
//
// It deliberately uses a CUSTOM [cache].pnpm_store_path that is NOT created in the
// runner Dockerfile, so Moby's image-ownership copy cannot help and ONLY the
// entrypoint's prepare_cache_dirs chown (the H4 fix, exercised VERBATIM by sourcing
// the real entrypoint) can make the fresh volume writable — proving the fix handles
// the operator-configurable store path, not just the baked-in default. Everything is
// scoped to a unique controller-id and torn down by label; it snapshots host
// containers/volumes before/after and never touches foreign resources.
package verify

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const (
	h4Instance  = "h4-verify-01"
	h4StorePath = "/opt/garm-pnpm-h4-custom" // deliberately NOT created in the Dockerfile
)

// buildPnpmWrapperImage layers a wrapper on the REAL production runner image
// (runner-images/noble — its real Dockerfile + real entrypoint.sh) that SOURCES
// the real /entrypoint.sh and invokes the REAL prepare_cache_dirs under that
// file's own `set -euo pipefail`, then sleeps as PID 1 so the harness can
// `docker exec -u runner` a real pnpm install. This is the H4 code path verbatim:
// were the fix absent, the fresh root-owned store volume would stay root-owned and
// the non-root `pnpm add` below would fail EACCES.
func buildPnpmWrapperImage(t *testing.T, root, tag string) {
	t.Helper()

	nobleTag := "garm-h4-noble-real:latest"
	if out, err := dockerTry("build", "--platform", "linux/amd64", "-t", nobleTag, filepath.Join(root, "runner-images", "noble")); err != nil {
		t.Fatalf("build real runner-images/noble image: %v\n%s", err, out)
	}

	dir := t.TempDir()
	wrapper := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"# Source the REAL production entrypoint (main is guarded not to run when\n" +
		"# sourced) and invoke the REAL prepare_cache_dirs under its own\n" +
		"# `set -euo pipefail`: this IS the H4 code path, verbatim, against the\n" +
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

func TestVerifyM2H4NonRootPnpmInstall(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)
	token := randHex(t)
	repoURL := "https://github.com/garm-h4-verify/repo-" + token
	repoKey := spec.RepoKey(repoURL)
	pnpmVol := spec.PnpmVolumeName(repoKey, "9")

	// --- foreign snapshot (must be identical after) --------------------------
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
	beforeC := snapshotIDs(t, "ps", "-a", "--format", "{{.Names}}")
	beforeV := snapshotIDs(t, "volume", "ls", "--format", "{{.Name}}")

	runnerTag := "garm-h4-verify-cli:" + token
	buildPnpmWrapperImage(t, root, runnerTag)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	// CUSTOM pnpm_store_path — not created in the Dockerfile, so only the
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
		`pnpm_store_path = "` + h4StorePath + `"`,
		"",
	}, "\n"))

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		_, _ = dockerTry("volume", "rm", "-f", pnpmVol, spec.ToolcacheVolumeName(repoKey, "1"), spec.DiagVolumeName(repoKey))
		_, _ = dockerTry("rmi", "-f", runnerTag)
		assertForeignPresent(t, "hbot-lab-mongodb")
		assertForeignPresent(t, "hummingbot")
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

	// =========================================================================
	// CreateInstance (repo-scoped, cache enabled) → provider creates + mounts the
	// pnpm store volume at the custom path and sets npm_config_store_dir.
	// =========================================================================
	b := cacheBootstrapPayload(h4Instance, repoURL, srv.URL, caBundle)
	stdout, code := runProvider(t, bin, configFile, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("CreateInstance exit=%d, want 0; stdout=%s", code, stdout)
	}
	runner := spec.RunnerContainerName(h4Instance)

	if src := mountSource(t, runner, h4StorePath); src != pnpmVol {
		t.Fatalf("pnpm store mount at %s = %q, want the provider volume %q", h4StorePath, src, pnpmVol)
	}
	if !strings.Contains(containerEnvVerify(t, runner), "npm_config_store_dir="+h4StorePath) {
		t.Errorf("runner is missing npm_config_store_dir=%s", h4StorePath)
	}

	// The store dir must be owned by the runner user (the H4 entrypoint chown ran
	// on the fresh, root-owned volume before privileges were dropped).
	owner := strings.TrimSpace(mustExecRunner(t, runner, "stat", "-c", "%U", h4StorePath))
	if owner != "runner" {
		t.Fatalf("[H4] pnpm store %s is owned by %q, want runner — the entrypoint chown did not run on the fresh root-owned volume", h4StorePath, owner)
	}
	t.Logf("[H4] pnpm store %s is runner-owned before the privilege drop", h4StorePath)

	// =========================================================================
	// THE PROOF: a REAL `pnpm add` AS THE NON-ROOT runner into the provider store.
	// If the fresh volume were still root-owned this would fail EACCES.
	// =========================================================================
	install := "set -e; d=$(mktemp -d); cd \"$d\"; " +
		"printf '{\"name\":\"h4\",\"version\":\"1.0.0\"}' > package.json; " +
		"pnpm add is-number@7.0.0; " +
		"test -e node_modules/is-number/package.json; " +
		"echo H4_PNPM_INSTALL_OK"
	out, err := execRunner(runner, "sh", "-c", install)
	if err != nil {
		t.Fatalf("[H4] real `pnpm add` as the NON-ROOT runner FAILED (the EACCES the fix closes): %v\n%s", err, out)
	}
	if !strings.Contains(out, "H4_PNPM_INSTALL_OK") {
		t.Fatalf("[H4] pnpm install did not complete: %s", out)
	}
	t.Logf("[H4] real `pnpm add is-number` as the non-root runner SUCCEEDED")

	// The provider-created store volume is now POPULATED (the content-addressed
	// store holds the fetched package), not empty.
	storeLs := mustExecRunner(t, runner, "sh", "-c", "ls -A "+h4StorePath)
	if strings.TrimSpace(storeLs) == "" {
		t.Errorf("[H4] the pnpm store %s is still empty after a successful install", h4StorePath)
	} else {
		t.Logf("[H4] provider pnpm store %s populated by the non-root install: %s", h4StorePath, strings.ReplaceAll(strings.TrimSpace(storeLs), "\n", " "))
	}

	if _, delCode := runProvider(t, bin, configFile, controllerID, "DeleteInstance", h4Instance, nil); delCode != 0 {
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
