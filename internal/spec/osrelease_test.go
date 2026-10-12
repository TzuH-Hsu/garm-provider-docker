package spec

import (
	"strings"
	"testing"
	"time"
)

// testOSKey is the OS key the spec tests key toolcache and pnpm volumes on: the
// value the Noble runner image's garm.docker/os-release label carries.
const testOSKey = "ubuntu-24.04"

const testImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRunnerOSKey(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		imageID string
		want    string
		wantErr bool
	}{
		{name: "noble label", labels: map[string]string{LabelOSRelease: "ubuntu-24.04"}, imageID: testImageID, want: "ubuntu-24.04"},
		{name: "resolute label", labels: map[string]string{LabelOSRelease: "ubuntu-26.04"}, imageID: testImageID, want: "ubuntu-26.04"},
		{name: "label wins over the image ID", labels: map[string]string{LabelOSRelease: "debian-13", "other": "x"}, imageID: "", want: "debian-13"},
		{name: "no label derives from the image ID", labels: map[string]string{"org.opencontainers.image.version": "26.04"}, imageID: testImageID, want: "image-0123456789abcdef0123456789abcdef"},
		{name: "nil labels derive from the image ID", labels: nil, imageID: testImageID, want: "image-0123456789abcdef0123456789abcdef"},
		{name: "image ID without algorithm prefix", labels: nil, imageID: strings.TrimPrefix(testImageID, "sha256:"), want: "image-0123456789abcdef0123456789abcdef"},
		{name: "uppercase label rejected", labels: map[string]string{LabelOSRelease: "Ubuntu-26.04"}, imageID: testImageID, wantErr: true},
		{name: "empty label rejected", labels: map[string]string{LabelOSRelease: ""}, imageID: testImageID, wantErr: true},
		{name: "label with a slash rejected", labels: map[string]string{LabelOSRelease: "ubuntu/26.04"}, imageID: testImageID, wantErr: true},
		{name: "label using the derived-key prefix rejected", labels: map[string]string{LabelOSRelease: "image-0123"}, imageID: testImageID, wantErr: true},
		{name: "overlong label rejected", labels: map[string]string{LabelOSRelease: strings.Repeat("a", 65)}, imageID: testImageID, wantErr: true},
		{name: "no label and no image ID", labels: nil, imageID: "", wantErr: true},
		{name: "no label and a short image ID", labels: nil, imageID: "sha256:abc", wantErr: true},
		{name: "no label and a non-hex image ID", labels: nil, imageID: "sha256:" + strings.Repeat("z", 64), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RunnerOSKey(tc.labels, tc.imageID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("RunnerOSKey = %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunnerOSKey returned an unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("RunnerOSKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOSKeySeparatesToolcacheAndPnpmButNotDiag(t *testing.T) {
	const repoKey = "octo-org-octo-repo-0123456789ab"
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	noble := CacheVolumeIdentity{ControllerID: "c", RepoKey: repoKey, RepoURLDigest: "d", OSKey: "ubuntu-24.04"}
	resolute := noble
	resolute.OSKey = "ubuntu-26.04"

	if ToolcacheVolumeName(repoKey, noble.OSKey, "1") == ToolcacheVolumeName(repoKey, resolute.OSKey, "1") {
		t.Error("toolcache volumes of two OS releases must have distinct names")
	}
	if PnpmVolumeName(repoKey, noble.OSKey, "9") == PnpmVolumeName(repoKey, resolute.OSKey, "9") {
		t.Error("pnpm store volumes of two OS releases must have distinct names")
	}

	for kind, labels := range map[string]map[string]string{
		"toolcache": noble.ToolcacheLabels("1", now),
		"pnpm":      noble.PnpmLabels("9", now),
	} {
		if got := labels[LabelOSRelease]; got != "ubuntu-24.04" {
			t.Errorf("%s labels carry %s=%q, want ubuntu-24.04", kind, LabelOSRelease, got)
		}
		if err := ValidateCacheVolumeKind(labels); err != nil {
			t.Errorf("%s labels do not validate: %v", kind, err)
		}
	}
	if _, ok := noble.DiagLabels(now)[LabelOSRelease]; ok {
		t.Error("diag-logs labels must not carry the OS key: diag volumes stay shared across OS releases")
	}

	// A volume created for one release is never adopted for another.
	err := ValidateAdoptedCacheVolume("v", noble.ToolcacheLabels("1", now), resolute.ToolcacheLabels("1", now))
	if err == nil || !strings.Contains(err.Error(), LabelOSRelease) {
		t.Errorf("adopting a toolcache volume across OS releases: err = %v, want an %s mismatch", err, LabelOSRelease)
	}
	if err := ValidateAdoptedCacheVolume("v", noble.PnpmLabels("9", now), noble.PnpmLabels("9", now)); err != nil {
		t.Errorf("same-release pnpm adoption rejected: %v", err)
	}

	// A label set without the OS key (one created before it existed) is still a
	// valid, provably-ours cache volume, so the GC reports it rather than
	// treating it as foreign.
	legacy := noble
	legacy.OSKey = ""
	if err := ValidateCacheVolumeKind(legacy.ToolcacheLabels("1", now)); err != nil {
		t.Errorf("pre-OS-key toolcache labels must stay valid: %v", err)
	}
}

func TestOSKeyedVolumeNamesStayValid(t *testing.T) {
	// The longest repokey (40-char slug + 32-hex hash) with the longest label
	// value and the derived image key must still be a valid Docker name.
	repoKey := strings.Repeat("s", repoSlugMaxLen) + "-" + strings.Repeat("0", repoKeyHashLen)
	derived, err := RunnerOSKey(nil, testImageID)
	if err != nil {
		t.Fatal(err)
	}
	for _, osKey := range []string{strings.Repeat("a", 64), derived, testOSKey} {
		for _, n := range []string{ToolcacheVolumeName(repoKey, osKey, "generation-salt"), PnpmVolumeName(repoKey, osKey, "10")} {
			if err := ValidateDerivedName("cache volume", n); err != nil {
				t.Errorf("cache volume name %q is invalid: %v", n, err)
			}
		}
	}
}
