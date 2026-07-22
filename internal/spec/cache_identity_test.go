package spec

import (
	"testing"
	"time"
)

// fullIdentity returns a genuinely-ours cache identity (controller + repokey +
// full repo-url-digest) so the strict per-kind builders below produce a COMPLETE
// label set that ValidateCacheVolumeKind must accept.
func fullIdentity() CacheVolumeIdentity {
	return CacheVolumeIdentity{
		ControllerID:  "ctrl-1",
		RepoKey:       "octo-org-octo-repo-0123456789ab",
		RepoURLDigest: RepoURLDigest("https://github.com/octo-org/octo-repo"),
	}
}

// TestValidateCacheVolumeKindAcceptsFullIdentity: a complete, provider-built
// label set for every kind passes the strict kind-aware validation.
func TestValidateCacheVolumeKindAcceptsFullIdentity(t *testing.T) {
	id := fullIdentity()
	ext := ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: "deadbeefcafe"}
	now := time.Now()

	valid := map[string]map[string]string{
		"toolcache": id.ToolcacheLabels("1", now),
		"pnpm":      id.PnpmLabels("9", now),
		"diag-logs": id.DiagLabels(now),
		"externals": ext.ExternalsLabels(now),
	}
	for kind, labels := range valid {
		if err := ValidateCacheVolumeKind(labels); err != nil {
			t.Errorf("%s: complete provider label set rejected by strict validation: %v", kind, err)
		}
	}
}

// TestValidateCacheVolumeKindRejectsIncomplete: a volume missing ANY required key
// for its kind (or missing the managed/cache markers, or with an unknown kind) is
// rejected — it is not provably ours.
func TestValidateCacheVolumeKindRejectsIncomplete(t *testing.T) {
	id := fullIdentity()
	now := time.Now()

	// A helper to shallow-copy then drop a key.
	without := func(base map[string]string, drop string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			if k == drop {
				continue
			}
			out[k] = v
		}
		return out
	}

	tests := []struct {
		name   string
		labels map[string]string
	}{
		{"toolcache missing repo-url-digest", without(id.ToolcacheLabels("1", now), LabelRepoURLDigest)},
		{"toolcache missing generation", without(id.ToolcacheLabels("1", now), LabelGeneration)},
		{"toolcache missing repo", without(id.ToolcacheLabels("1", now), LabelRepo)},
		{"pnpm missing repo-url-digest", without(id.PnpmLabels("9", now), LabelRepoURLDigest)},
		{"pnpm missing pnpm-major", without(id.PnpmLabels("9", now), LabelPnpmMajor)},
		{"diag missing repo-url-digest", without(id.DiagLabels(now), LabelRepoURLDigest)},
		{"diag missing repo", without(id.DiagLabels(now), LabelRepo)},
		{"externals missing image-digest", without((ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: "d"}).ExternalsLabels(now), LabelImageDigest)},
		{"missing managed", without(id.ToolcacheLabels("1", now), LabelManaged)},
		{"missing cache marker", without(id.ToolcacheLabels("1", now), LabelCache)},
		{"unknown kind", map[string]string{LabelManaged: "true", LabelCache: "true", LabelCacheKind: "mystery", LabelRepo: "r", LabelRepoURLDigest: "d"}},
		{"absent kind", map[string]string{LabelManaged: "true", LabelCache: "true"}},
		{"unlabeled auto-created replacement", map[string]string{}},
		{"foreign non-cache volume", map[string]string{"foreign": "yes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateCacheVolumeKind(tt.labels); err == nil {
				t.Errorf("ValidateCacheVolumeKind accepted an incomplete/foreign volume (%s): %v", tt.name, tt.labels)
			}
		})
	}
}

// TestValidateDiagPruneTargetMatchesSnapshot: the pinned diag volume prunes only
// when its FULL identity matches the enumeration snapshot (kind + controller +
// repo + repo-url-digest); any mismatch — a different repo, a wrong controller,
// an unlabeled/foreign volume, or a non-diag kind — is rejected so the destructive
// `find -delete` never runs against it.
func TestValidateDiagPruneTargetMatchesSnapshot(t *testing.T) {
	now := time.Now()
	snapID := CacheVolumeIdentity{
		ControllerID:  "ctrl-1",
		RepoKey:       "repo-a",
		RepoURLDigest: RepoURLDigest("https://github.com/octo-org/repo-a"),
	}
	snapshot := snapID.DiagLabels(now)

	// The genuinely-ours, unchanged pinned volume passes.
	if err := ValidateDiagPruneTarget(snapID.DiagLabels(now), snapshot, "ctrl-1"); err != nil {
		t.Fatalf("a matching pinned diag volume was rejected: %v", err)
	}

	otherRepo := CacheVolumeIdentity{
		ControllerID:  "ctrl-1",
		RepoKey:       "repo-B",
		RepoURLDigest: RepoURLDigest("https://github.com/octo-org/repo-b"),
	}

	tests := []struct {
		name         string
		got          map[string]string
		controllerID string
	}{
		{"different repo (same-name replacement of another repo)", otherRepo.DiagLabels(now), "ctrl-1"},
		{"wrong controller", (CacheVolumeIdentity{ControllerID: "ctrl-2", RepoKey: "repo-a", RepoURLDigest: snapID.RepoURLDigest}).DiagLabels(now), "ctrl-1"},
		{"unlabeled auto-created replacement", map[string]string{}, "ctrl-1"},
		{"wrong kind (toolcache under the diag name)", snapID.ToolcacheLabels("1", now), "ctrl-1"},
		{"incomplete identity (no repo-url-digest)", map[string]string{LabelManaged: "true", LabelCache: "true", LabelCacheKind: string(CacheKindDiagLogs), LabelControllerID: "ctrl-1", LabelRepo: "repo-a"}, "ctrl-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateDiagPruneTarget(tt.got, snapshot, tt.controllerID); err == nil {
				t.Errorf("ValidateDiagPruneTarget accepted a mismatched pinned volume (%s) — the destructive prune would run against it", tt.name)
			}
		})
	}
}
