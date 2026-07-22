//go:build dockerverify

// This file adds the M2 cross-family cache-safety real-daemon verification:
//
//   - H1: an org-scoped SCHEME-LESS repo_url gets NO persistent cache (fail safe);
//
//   - H2: a non-canonical/reserved cache path is rejected at config load;
//
//   - RECONCILE (structural redesign 2026-07-22): a FOREIGN volume squatting a
//     deterministic cache name is NEITHER adopted NOR deleted — CreateInstance
//     reconciles AROUND it to an alternate name and succeeds, and an UNLABELED
//     reincarnation of the name does NOT wedge a subsequent create;
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

	// reconciledExternalsByLabel returns this controller's externals cache volume
	// names discovered BY LABEL (cache-kind=externals + image-digest) — the same
	// discovery EnsureCacheVolume uses, so a reconciled alternate name is found.
	reconciledExternalsByLabel := func(t *testing.T, controllerID, digest string) []string {
		return lines(dockerOut(t, "volume", "ls", "-q",
			"--filter", "label=garm.docker/controller-id="+controllerID,
			"--filter", "label=garm.docker/cache-kind="+string(spec.CacheKindExternals),
			"--filter", "label=garm.docker/image-digest="+digest))
	}

	// =========================================================================
	// RECONCILE: a FOREIGN volume squatting the deterministic externals name is
	// NEITHER adopted NOR deleted — CreateInstance reconciles AROUND it to an
	// alternate name and SUCCEEDS; the foreign volume is left untouched (B2).
	// =========================================================================
	t.Run("foreign_externals_squatter_reconciled_not_deleted", func(t *testing.T) {
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

		repoURL := "https://github.com/garm-reconcile-verify/repo-" + randHex(t)
		b := cacheBootstrapPayload("reconcile-squatter", repoURL, srv.URL, caBundle)
		out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
		if code != 0 {
			t.Fatalf("[reconcile] CreateInstance must reconcile around a foreign squatter and SUCCEED, exit=%d\n%s", code, out)
		}
		t.Logf("[reconcile] CreateInstance succeeded by reconciling around the foreign squatter (exit=%d)", code)

		// The foreign volume is PRESERVED and still UNLABELED (never adopted, never
		// deleted — it is not ours to touch, B2).
		if _, err := dockerTry("volume", "inspect", extName); err != nil {
			t.Fatalf("[reconcile] the foreign externals volume %q was DELETED — never delete a volume we cannot prove is ours", extName)
		}
		got := strings.TrimSpace(dockerOut(t, "volume", "inspect", extName, "-f", "{{index .Labels \"garm.docker/cache\"}}"))
		if got == "true" {
			t.Errorf("[reconcile] the foreign externals volume was relabeled as our cache — it must be left untouched")
		} else {
			t.Logf("[reconcile] the foreign externals volume was PRESERVED unlabeled (not adopted, not deleted)")
		}

		// Our externals cache exists under an ALTERNATE name, discoverable by label.
		ours := reconciledExternalsByLabel(t, controllerID, digest)
		if len(ours) != 1 {
			t.Fatalf("[reconcile] want exactly 1 reconciled externals cache by label, got %v", ours)
		}
		if ours[0] == extName {
			t.Errorf("[reconcile] our externals cache took the squatted name %q; want a reconciled alternate", extName)
		} else {
			t.Logf("[reconcile] our externals cache reconciled to alternate %q (foreign %q untouched)", ours[0], extName)
		}
	})

	// =========================================================================
	// NO WEDGE: an UNLABELED reincarnation of the deterministic externals name
	// (as Moby auto-creates after an evict) does NOT permanently block a
	// subsequent create — it reconciles around it and succeeds (B3).
	// =========================================================================
	t.Run("unlabeled_reincarnation_does_not_wedge", func(t *testing.T) {
		controllerID := randControllerID(t)
		cfg := cacheConfig(t)
		digest := imageDigestHex(t, imageTag)
		extName := spec.ExternalsVolumeName(digest)

		if out, err := dockerTry("volume", "create", extName); err != nil {
			t.Fatalf("seed unlabeled reincarnation: %v\n%s", err, out)
		}
		defer func() {
			cleanupController(t, controllerID)
			_, _ = dockerTry("volume", "rm", "-f", extName)
		}()

		repoURL := "https://github.com/garm-nowedge-verify/repo-" + randHex(t)
		b := cacheBootstrapPayload("nowedge", repoURL, srv.URL, caBundle)
		if out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b); code != 0 {
			t.Fatalf("[no-wedge] CreateInstance WEDGED on an unlabeled deterministic externals name, exit=%d\n%s", code, out)
		}
		t.Logf("[no-wedge] CreateInstance reconciled around the unlabeled reincarnation and succeeded")

		// The unlabeled squatter is preserved; our cache is under an alternate name.
		got := strings.TrimSpace(dockerOut(t, "volume", "inspect", extName, "-f", "{{index .Labels \"garm.docker/cache\"}}"))
		if got == "true" {
			t.Errorf("[no-wedge] the unlabeled reincarnation was adopted/relabeled; must be left untouched")
		}
		ours := reconciledExternalsByLabel(t, controllerID, digest)
		if len(ours) != 1 || ours[0] == extName {
			t.Errorf("[no-wedge] want exactly 1 reconciled externals cache under an alternate name, got %v", ours)
		} else {
			t.Logf("[no-wedge] our externals cache is under the alternate %q; the unlabeled name %q is left as cruft", ours[0], extName)
		}
	})

	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
}
