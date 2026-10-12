package provider

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// resoluteImage is the flavor image the OS-key tests select for a second pool
// of the same repository.
const resoluteImage = "ghcr.io/example/runner-resolute@sha256:feedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeedfeed"

// newTwoOSCacheProvider is newCacheProvider plus a "resolute" flavor whose image
// reports a different OS release than the default image.
func newTwoOSCacheProvider(t *testing.T) (*Provider, *docker.FakeClient) {
	t.Helper()
	p, fake := newCacheProvider(t, false)
	cfg := p.cfg
	cfg.Flavors = map[string]config.Flavor{"resolute": {RunnerImage: resoluteImage}}
	fake.ImageLabels[resoluteImage] = map[string]string{spec.LabelOSRelease: "ubuntu-26.04"}
	p2, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}
	return p2, fake
}

// TestCacheVolumesKeyedOnRunnerOSRelease: two pools of ONE repository, one on
// the default (Ubuntu 24.04) image and one on a flavor's Ubuntu 26.04 image, get
// separate toolcache and pnpm store volumes, each labelled with its OS release,
// while the diag-logs volume stays shared (ADR-003, 2026-10-12 amendment).
func TestCacheVolumesKeyedOnRunnerOSRelease(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newTwoOSCacheProvider(t)

	if _, err := p.CreateInstance(context.Background(), cacheBootstrap("noble-job", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance (noble pool): %v", err)
	}
	b := cacheBootstrap("resolute-job", cacheRepoURL, srv.URL)
	b.ExtraSpecs = json.RawMessage(`{"flavor": "resolute"}`)
	if _, err := p.CreateInstance(context.Background(), b); err != nil {
		t.Fatalf("CreateInstance (resolute pool): %v", err)
	}

	repoKey := spec.RepoKey(cacheRepoURL)
	for _, tc := range []struct {
		kind  spec.CacheKind
		names map[string]string // OS key -> expected volume name
	}{
		{spec.CacheKindToolcache, map[string]string{
			"ubuntu-24.04": spec.ToolcacheVolumeName(repoKey, "ubuntu-24.04", "1"),
			"ubuntu-26.04": spec.ToolcacheVolumeName(repoKey, "ubuntu-26.04", "1"),
		}},
		{spec.CacheKindPnpm, map[string]string{
			"ubuntu-24.04": spec.PnpmVolumeName(repoKey, "ubuntu-24.04", "9"),
			"ubuntu-26.04": spec.PnpmVolumeName(repoKey, "ubuntu-26.04", "9"),
		}},
	} {
		got := cacheVolsByKind(t, fake, tc.kind)
		if len(got) != 2 {
			t.Errorf("%s volumes = %v, want one per OS release", tc.kind, got)
		}
		for osKey, name := range tc.names {
			v, ok := volByName(t, fake, name)
			if !ok {
				t.Errorf("%s volume %q for %s is missing (have %v)", tc.kind, name, osKey, got)
				continue
			}
			if v.Labels[spec.LabelOSRelease] != osKey {
				t.Errorf("%s volume %q has %s=%q, want %q", tc.kind, name, spec.LabelOSRelease, v.Labels[spec.LabelOSRelease], osKey)
			}
		}
	}
	if diag := cacheVolsByKind(t, fake, spec.CacheKindDiagLogs); len(diag) != 1 {
		t.Errorf("diag-logs volumes = %v, want one shared by both pools of the repository", diag)
	}

	// Each runner mounts its own release's toolcache.
	for job, osKey := range map[string]string{"noble-job": "ubuntu-24.04", "resolute-job": "ubuntu-26.04"} {
		c, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName(job))
		if err != nil {
			t.Fatalf("inspect runner %s: %v", job, err)
		}
		m, ok := mountAt(c, "/opt/hostedtoolcache")
		if !ok {
			t.Fatalf("runner %s has no toolcache mount", job)
		}
		if want := spec.ToolcacheVolumeName(repoKey, osKey, "1"); m.Name != want {
			t.Errorf("runner %s mounts toolcache %q, want %q", job, m.Name, want)
		}
	}
}

// TestCacheVolumesKeyedOnImageWhenUnlabelled: a runner image without the
// garm.docker/os-release label (an operator's own image) keys its toolcache on
// the image content ID, never on a shared default bucket.
func TestCacheVolumesKeyedOnImageWhenUnlabelled(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)
	delete(fake.ImageLabels, p.cfg.RunnerImage)

	if _, err := p.CreateInstance(context.Background(), cacheBootstrap("custom-job", cacheRepoURL, srv.URL)); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	tool := cacheVolsByKind(t, fake, spec.CacheKindToolcache)
	if len(tool) != 1 {
		t.Fatalf("toolcache volumes = %v, want exactly one", tool)
	}
	v, _ := volByName(t, fake, tool[0])
	osKey := v.Labels[spec.LabelOSRelease]
	if !strings.HasPrefix(osKey, "image-") || len(osKey) != len("image-")+32 {
		t.Errorf("unlabelled image OS key = %q, want image-<32 hex>", osKey)
	}
	if want := spec.ToolcacheVolumeName(spec.RepoKey(cacheRepoURL), osKey, "1"); tool[0] != want {
		t.Errorf("toolcache volume = %q, want %q", tool[0], want)
	}
}

// TestCreateInstanceRejectsMalformedOSReleaseLabel: a malformed os-release label
// fails the allocation before any cache volume or runner exists, rather than
// guessing which caches the image may share.
func TestCreateInstanceRejectsMalformedOSReleaseLabel(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()
	p, fake := newCacheProvider(t, false)
	fake.ImageLabels[p.cfg.RunnerImage] = map[string]string{spec.LabelOSRelease: "Ubuntu 26.04"}

	_, err := p.CreateInstance(context.Background(), cacheBootstrap("bad-label-job", cacheRepoURL, srv.URL))
	if err == nil || !strings.Contains(err.Error(), spec.LabelOSRelease) {
		t.Fatalf("CreateInstance err = %v, want an %s error", err, spec.LabelOSRelease)
	}
	if n := countCacheVols(t, fake); n != 0 {
		t.Errorf("a rejected image left %d cache volumes behind", n)
	}
	if _, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName("bad-label-job")); err == nil {
		t.Error("a runner container exists for an allocation that failed on its image's OS label")
	}
	assertNoLeftovers(t, fake)
	if !slices.Contains(fake.PulledImages, p.cfg.RunnerImage) {
		t.Errorf("PulledImages = %v, want the runner image pulled before its labels are read", fake.PulledImages)
	}
}
