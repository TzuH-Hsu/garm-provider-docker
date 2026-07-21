//go:build dockerverify

// This file adds the M2 cross-family cache-safety real-daemon verification:
//
//   - H1: an org-scoped SCHEME-LESS repo_url gets NO persistent cache (fail safe);
//
//   - H2: a non-canonical/reserved cache path is rejected at config load;
//
//   - M6: a FOREIGN volume squatting a deterministic cache name is NOT adopted —
//     CreateInstance fails CLOSED and never mounts it (the same detection the H3
//     GC-during-create fail-closed rests on);
//
//   - the real-daemon FACT the H3 fail-closed premise rests on: ContainerCreate
//     AUTO-CREATES a missing named volume UNLABELED (so the provider can detect
//     an evicted-and-auto-recreated cache and refuse to run against it).
//
//     go test -tags dockerverify -v -run TestVerifyM2CacheSafety ./internal/verify/
//
// Everything is scoped to a unique controller-id and torn down by label; it never
// touches hbot-lab-mongodb/hummingbot or any foreign resource.
package verify

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

func TestVerifyM2CacheSafety(t *testing.T) {
	root := repoRoot(t)
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")

	imageTag := "garm-m2safety-sleep:latest"
	buildSleepImage(t, imageTag)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	cacheConfig := func(t *testing.T) string {
		f := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, f, strings.Join([]string{
			`docker_host = "unix:///var/run/docker.sock"`,
			`runner_image = "` + imageTag + `"`,
			`allow_unpinned_runner_image = true`,
			`[cache]`,
			`enabled = true`,
			`generation = "1"`,
			`pnpm_major = "9"`,
			`toolcache_path = "/opt/hostedtoolcache"`,
			`pnpm_store_path = "/opt/pnpm-store"`,
			"",
		}, "\n"))
		return f
	}

	// =========================================================================
	// FACT (H3 premise): the daemon auto-creates a missing named volume UNLABELED.
	// =========================================================================
	t.Run("daemon_autocreates_missing_named_volume_unlabeled", func(t *testing.T) {
		vol := "garm-m2safety-missing-" + randHex(t)
		cname := "garm-m2safety-ac-" + randHex(t)
		defer func() {
			_, _ = dockerTry("rm", "-f", cname)
			_, _ = dockerTry("volume", "rm", "-f", vol)
		}()
		if out, err := dockerTry("create", "--name", cname, "-v", vol+":/data", imageTag); err != nil {
			t.Fatalf("docker create referencing a missing named volume failed: %v\n%s", err, out)
		}
		labels := strings.TrimSpace(dockerOut(t, "volume", "inspect", vol, "-f", "{{.Labels}}"))
		if labels != "map[]" && labels != "" && labels != "<no value>" {
			t.Errorf("[H3-fact] auto-created volume carries labels %q, want none (real Moby copies no labels)", labels)
		} else {
			t.Logf("[H3-fact] the daemon auto-created the missing named volume UNLABELED (%q) — the premise the fail-closed rests on", labels)
		}
	})

	// =========================================================================
	// H2: a bad cache path is rejected at config load (any command fails).
	// =========================================================================
	t.Run("bad_cache_path_rejected_at_load", func(t *testing.T) {
		controllerID := randControllerID(t)
		bad := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, bad, strings.Join([]string{
			`docker_host = "unix:///var/run/docker.sock"`,
			`runner_image = "` + imageTag + `"`,
			`allow_unpinned_runner_image = true`,
			`[cache]`,
			`enabled = true`,
			`pnpm_store_path = "/var/run"`, // aliases onto /run — must be rejected (H2)
			"",
		}, "\n"))
		out, code := runProvider(t, bin, bad, controllerID, "ListInstances", "", nil)
		if code == 0 {
			t.Errorf("[H2] provider accepted a cache path that aliases onto /run; want a config-load failure. stdout=%s", out)
		} else {
			t.Logf("[H2] provider rejected the /var/run cache path at config load (exit=%d)", code)
		}
	})

	// =========================================================================
	// H1: an org-scoped SCHEME-LESS repo_url gets NO persistent cache.
	// =========================================================================
	t.Run("schemeless_org_url_gets_no_cache", func(t *testing.T) {
		controllerID := randControllerID(t)
		cfg := cacheConfig(t)
		schemeless := "github.com/garm-h1-verify-org-" + randHex(t) // scheme-less org
		defer cleanupController(t, controllerID)

		b := cacheBootstrapPayload("h1-schemeless", schemeless, srv.URL, caBundle)
		if _, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b); code != 0 {
			t.Fatalf("[H1] CreateInstance with a scheme-less repo_url should still succeed (JIT), exit=%d", code)
		}
		// No REPO-KEYED cache volume of any kind for this controller (the shared
		// externals volume carries no repo label and may exist).
		repoKeyed := lines(dockerOut(t, "volume", "ls", "-q",
			"--filter", "label=garm.docker/controller-id="+controllerID,
			"--filter", "label=garm.docker/cache=true"))
		var withRepo []string
		for _, v := range repoKeyed {
			repo := strings.TrimSpace(dockerOut(t, "volume", "inspect", v, "-f", "{{index .Labels \"garm.docker/repo\"}}"))
			if repo != "" && repo != "<no value>" {
				withRepo = append(withRepo, v)
			}
		}
		if len(withRepo) != 0 {
			t.Errorf("[H1] scheme-less org repo_url produced repo-keyed cache volumes %v, want none (fail safe)", withRepo)
		} else {
			t.Logf("[H1] scheme-less org repo_url got NO persistent (repo-keyed) cache — fail safe")
		}
	})

	// =========================================================================
	// M6: a FOREIGN volume squatting a deterministic cache name is NOT adopted;
	// CreateInstance fails CLOSED and leaves the foreign volume untouched.
	// =========================================================================
	t.Run("foreign_externals_squatter_fails_closed", func(t *testing.T) {
		controllerID := randControllerID(t)
		cfg := cacheConfig(t)
		digest := imageDigestHex(t, imageTag)
		extName := spec.ExternalsVolumeName(digest)

		// Pre-create a FOREIGN, UNLABELED volume squatting the externals name.
		if out, err := dockerTry("volume", "create", extName); err != nil {
			t.Fatalf("seed foreign externals squatter: %v\n%s", err, out)
		}
		defer func() {
			cleanupController(t, controllerID)
			_, _ = dockerTry("volume", "rm", "-f", extName)
		}()

		repoURL := "https://github.com/garm-m6-verify/repo-" + randHex(t)
		b := cacheBootstrapPayload("m6-squatter", repoURL, srv.URL, caBundle)
		out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
		if code == 0 {
			t.Fatalf("[M6] CreateInstance adopted a foreign externals volume and succeeded; want a fail-closed error. stdout=%s", out)
		}
		t.Logf("[M6] CreateInstance failed closed against a foreign externals squatter (exit=%d)", code)

		// No runner container was left behind.
		if _, err := dockerTry("inspect", spec.RunnerContainerName("m6-squatter")); err == nil {
			t.Error("[M6] a runner container was left after the fail-closed create")
		}
		// The foreign volume must still exist and remain UNLABELED (never adopted,
		// never deleted — it is not ours to touch).
		got := strings.TrimSpace(dockerOut(t, "volume", "inspect", extName, "-f", "{{index .Labels \"garm.docker/cache\"}}"))
		if got == "true" {
			t.Errorf("[M6] the foreign externals volume was relabeled as our cache — it must be left untouched")
		} else {
			t.Logf("[M6] the foreign externals volume was left untouched (not adopted, not deleted)")
		}
	})

	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
}
