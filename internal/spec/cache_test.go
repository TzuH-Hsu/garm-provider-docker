package spec

import (
	"strings"
	"testing"
	"time"
)

func TestRepoKeyNormalization(t *testing.T) {
	// All of these are spellings of the SAME repository and MUST produce the
	// same repokey — that is the whole point of normalization (a cache HIT
	// across URL spellings, ADR-003).
	sameRepo := []struct {
		name    string
		repoURL string
	}{
		{name: "canonical", repoURL: "https://github.com/octo-org/octo-repo"},
		{name: "uppercase", repoURL: "https://github.com/Octo-Org/Octo-Repo"},
		{name: "trailing .git", repoURL: "https://github.com/octo-org/octo-repo.git"},
		{name: "trailing slash", repoURL: "https://github.com/octo-org/octo-repo/"},
		{name: "trailing .git and slash", repoURL: "https://github.com/octo-org/octo-repo.git/"},
		{name: "http scheme", repoURL: "http://github.com/octo-org/octo-repo"},
		{name: "surrounding whitespace", repoURL: "  https://github.com/octo-org/octo-repo  "},
		{name: "no scheme", repoURL: "github.com/octo-org/octo-repo"},
	}
	want := RepoKey(sameRepo[0].repoURL)
	for _, tt := range sameRepo {
		t.Run(tt.name, func(t *testing.T) {
			if got := RepoKey(tt.repoURL); got != want {
				t.Errorf("RepoKey(%q) = %q, want %q (must equal the canonical spelling's key)", tt.repoURL, got, want)
			}
		})
	}
}

func TestRepoKeyShape(t *testing.T) {
	tests := []struct {
		name       string
		repoURL    string
		wantPrefix string // human-readable slug prefix (host dropped)
	}{
		{name: "repo URL", repoURL: "https://github.com/octo-org/octo-repo", wantPrefix: "octo-org-octo-repo-"},
		{name: "org URL", repoURL: "https://github.com/octo-org", wantPrefix: "octo-org-"},
		{name: "enterprise URL", repoURL: "https://github.com/enterprises/octo-ent", wantPrefix: "enterprises-octo-ent-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := RepoKey(tt.repoURL)
			if !strings.HasPrefix(key, tt.wantPrefix) {
				t.Errorf("RepoKey(%q) = %q, want prefix %q", tt.repoURL, key, tt.wantPrefix)
			}
			// The suffix must be exactly repoKeyHashLen lowercase-hex chars.
			hash := strings.TrimPrefix(key, tt.wantPrefix)
			if len(hash) != repoKeyHashLen {
				t.Errorf("RepoKey(%q) hash suffix = %q (%d chars), want %d", tt.repoURL, hash, len(hash), repoKeyHashLen)
			}
			for _, r := range hash {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
					t.Errorf("RepoKey(%q) hash suffix %q is not lowercase hex", tt.repoURL, hash)
					break
				}
			}
			// The whole repokey must be a valid Docker resource-name fragment.
			if err := ValidateDerivedName("repokey", key); err != nil {
				t.Errorf("RepoKey(%q) = %q is not a valid Docker name: %v", tt.repoURL, key, err)
			}
		})
	}
}

// TestRepoKeyDistinctRepos: two different repositories must not collide on the
// repokey (different slug AND different hash).
func TestRepoKeyDistinctRepos(t *testing.T) {
	a := RepoKey("https://github.com/octo-org/repo-a")
	b := RepoKey("https://github.com/octo-org/repo-b")
	if a == b {
		t.Errorf("distinct repos produced the same repokey %q", a)
	}
	// Same path on a different host must also differ (the hash covers the host).
	gh := RepoKey("https://github.com/octo-org/octo-repo")
	gitea := RepoKey("https://gitea.example.com/octo-org/octo-repo")
	if gh == gitea {
		t.Errorf("same path on different hosts collided on repokey %q — the hash must cover the host", gh)
	}
}

// TestRepoKeyDegeneratePath: a path that sanitises to nothing must still yield
// a valid, non-empty repokey (the hash alone).
func TestRepoKeyDegeneratePath(t *testing.T) {
	key := RepoKey("https://github.com/////")
	if key == "" {
		t.Fatal("RepoKey of a punctuation-only path returned empty")
	}
	if strings.HasPrefix(key, "-") || strings.HasSuffix(key, "-") {
		t.Errorf("degenerate repokey %q must not start or end with a dash", key)
	}
	if err := ValidateDerivedName("repokey", key); err != nil {
		t.Errorf("degenerate repokey %q is not a valid Docker name: %v", key, err)
	}
}

func TestDetectCacheEntityScope(t *testing.T) {
	tests := []struct {
		name    string
		repoURL string
		want    CacheEntityScope
	}{
		{name: "repo", repoURL: "https://github.com/octo-org/octo-repo", want: CacheScopeRepo},
		{name: "repo with .git", repoURL: "https://github.com/octo-org/octo-repo.git", want: CacheScopeRepo},
		{name: "org", repoURL: "https://github.com/octo-org", want: CacheScopeOrg},
		{name: "org trailing slash", repoURL: "https://github.com/octo-org/", want: CacheScopeOrg},
		{name: "enterprise", repoURL: "https://github.com/enterprises/octo-ent", want: CacheScopeEnterprise},
		{name: "empty path is unknown", repoURL: "https://github.com", want: CacheScopeUnknown},
		{name: "over-deep path is unknown", repoURL: "https://github.com/a/b/c", want: CacheScopeUnknown},
		{name: "malformed enterprise is unknown", repoURL: "https://github.com/enterprises", want: CacheScopeUnknown},
		{name: "garbage is unknown", repoURL: "::::not a url", want: CacheScopeUnknown},
		{name: "empty string is unknown", repoURL: "", want: CacheScopeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DetectCacheEntityScope(tt.repoURL); got != tt.want {
				t.Errorf("DetectCacheEntityScope(%q) = %q, want %q", tt.repoURL, got, tt.want)
			}
		})
	}
}

func TestCacheScopeAllowed(t *testing.T) {
	tests := []struct {
		name           string
		scope          CacheEntityScope
		allowOrgShared bool
		want           bool
	}{
		{name: "repo always allowed", scope: CacheScopeRepo, allowOrgShared: false, want: true},
		{name: "repo allowed even with opt-in", scope: CacheScopeRepo, allowOrgShared: true, want: true},
		{name: "org withheld by default", scope: CacheScopeOrg, allowOrgShared: false, want: false},
		{name: "org allowed with opt-in", scope: CacheScopeOrg, allowOrgShared: true, want: true},
		{name: "enterprise withheld by default", scope: CacheScopeEnterprise, allowOrgShared: false, want: false},
		{name: "enterprise allowed with opt-in", scope: CacheScopeEnterprise, allowOrgShared: true, want: true},
		{name: "unknown withheld by default", scope: CacheScopeUnknown, allowOrgShared: false, want: false},
		{name: "unknown withheld EVEN with opt-in (no confident identity)", scope: CacheScopeUnknown, allowOrgShared: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CacheScopeAllowed(tt.scope, tt.allowOrgShared); got != tt.want {
				t.Errorf("CacheScopeAllowed(%q, %v) = %v, want %v", tt.scope, tt.allowOrgShared, got, tt.want)
			}
		})
	}
}

func TestCacheVolumeNames(t *testing.T) {
	const repoKey = "octo-org-octo-repo-0123456789ab"
	if got, want := ToolcacheVolumeName(repoKey, "1"), "garm-cache-toolcache-"+repoKey+"-1"; got != want {
		t.Errorf("ToolcacheVolumeName = %q, want %q", got, want)
	}
	if got, want := PnpmVolumeName(repoKey, "9"), "garm-cache-pnpm-"+repoKey+"-9"; got != want {
		t.Errorf("PnpmVolumeName = %q, want %q", got, want)
	}
	// A generation/pnpm-major bump produces a DIFFERENT volume name (fresh cache).
	if ToolcacheVolumeName(repoKey, "1") == ToolcacheVolumeName(repoKey, "2") {
		t.Error("a generation bump must produce a distinct toolcache volume name")
	}
	if PnpmVolumeName(repoKey, "9") == PnpmVolumeName(repoKey, "10") {
		t.Error("a pnpm-major bump must produce a distinct pnpm volume name")
	}
	// Both are valid Docker resource names.
	for _, n := range []string{ToolcacheVolumeName(repoKey, "1"), PnpmVolumeName(repoKey, "9")} {
		if err := ValidateDerivedName("cache volume", n); err != nil {
			t.Errorf("cache volume name %q is invalid: %v", n, err)
		}
	}
}

func TestCacheVolumeLabels(t *testing.T) {
	id := CacheVolumeIdentity{ControllerID: "ctrl-1", RepoKey: "octo-org-octo-repo-0123456789ab"}
	lastUsed := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	tool := id.ToolcacheLabels("3", lastUsed)
	assertLabel(t, tool, LabelManaged, "true")
	assertLabel(t, tool, LabelControllerID, "ctrl-1")
	assertLabel(t, tool, LabelCache, "true")
	assertLabel(t, tool, LabelCacheKind, string(CacheKindToolcache))
	assertLabel(t, tool, LabelRepo, "octo-org-octo-repo-0123456789ab")
	assertLabel(t, tool, LabelGeneration, "3")
	assertLabel(t, tool, LabelLastUsed, "2026-07-21T12:00:00Z")

	pnpm := id.PnpmLabels("9", lastUsed)
	assertLabel(t, pnpm, LabelCacheKind, string(CacheKindPnpm))
	assertLabel(t, pnpm, LabelPnpmMajor, "9")
	assertLabel(t, pnpm, LabelLastUsed, "2026-07-21T12:00:00Z")

	// CRITICAL (ADR-003/ADR-004): a cache volume carries NO instance-name, so
	// it is structurally excluded from the teardown/orphan-sweep predicate.
	for _, labels := range []map[string]string{tool, pnpm} {
		if _, ok := labels[LabelInstanceName]; ok {
			t.Error("cache volume labels must NOT carry an instance-name")
		}
		if _, ok := labels[LabelCreateNonce]; ok {
			t.Error("cache volume labels must NOT carry a create-nonce")
		}
		if _, ok := labels[LabelResource]; ok {
			t.Error("cache volume labels must NOT carry a job-scoped resource label")
		}
	}
}

// TestCacheVolumesExcludedFromTeardownPredicate is the load-bearing ADR-004
// reconciliation: the teardown/orphan-sweep/rollback predicate (MatchesPredicate)
// and the runner-identity check (IsManagedRunner) must NEVER match a cache
// volume, in EITHER controller-scope, so teardown and sweep leave caches alone.
func TestCacheVolumesExcludedFromTeardownPredicate(t *testing.T) {
	id := CacheVolumeIdentity{ControllerID: "ctrl-1", RepoKey: "octo-org-octo-repo-0123456789ab"}
	lastUsed := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

	cacheLabelSets := map[string]map[string]string{
		"toolcache": id.ToolcacheLabels("1", lastUsed),
		"pnpm":      id.PnpmLabels("9", lastUsed),
	}
	for kind, labels := range cacheLabelSets {
		if MatchesPredicate(labels, "ctrl-1") {
			t.Errorf("%s cache volume MATCHED the ADR-004 teardown predicate for its own controller — teardown would delete it", kind)
		}
		if IsManagedRunner(labels, "ctrl-1") {
			t.Errorf("%s cache volume was mistaken for a managed runner", kind)
		}
		// Also not matched under a different controller (defense-in-depth).
		if MatchesPredicate(labels, "ctrl-2") {
			t.Errorf("%s cache volume matched the predicate for a foreign controller", kind)
		}
	}
}

func assertLabel(t *testing.T, labels map[string]string, key, want string) {
	t.Helper()
	if got := labels[key]; got != want {
		t.Errorf("label %q = %q, want %q", key, got, want)
	}
}
