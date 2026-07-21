package spec

import (
	"testing"
	"time"
)

// gcNow is the fixed "current" instant every GC-decision case is evaluated at.
var gcNow = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

// lu builds a last-used (creation) label value `d` in the past from gcNow.
func lu(d time.Duration) string {
	return gcNow.Add(-d).UTC().Format(time.RFC3339)
}

func TestEvaluateCacheEviction(t *testing.T) {
	const digest = "aaaaaaaaaaaa"
	cfg := CacheGCConfig{
		Generation:  "2",
		PnpmMajor:   "9",
		ImageDigest: digest,
		StaleDays:   30,
		Grace:       30 * time.Minute,
	}
	id := CacheVolumeIdentity{ControllerID: "ctrl-1", RepoKey: "repo-abc"}
	ext := ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: digest}

	tests := []struct {
		name      string
		labels    map[string]string
		wantEvict bool
	}{
		{
			name:      "current toolcache generation, fresh → keep",
			labels:    withLastUsed(id.ToolcacheLabels("2", time.Time{}), lu(time.Hour)),
			wantEvict: false,
		},
		{
			name:      "superseded toolcache generation, past grace → evict",
			labels:    withLastUsed(id.ToolcacheLabels("1", time.Time{}), lu(2*time.Hour)),
			wantEvict: true,
		},
		{
			name:      "superseded toolcache generation, WITHIN grace → keep (in-flight older job)",
			labels:    withLastUsed(id.ToolcacheLabels("1", time.Time{}), lu(5*time.Minute)),
			wantEvict: false,
		},
		{
			name:      "current pnpm-major, fresh → keep",
			labels:    withLastUsed(id.PnpmLabels("9", time.Time{}), lu(time.Hour)),
			wantEvict: false,
		},
		{
			name:      "superseded pnpm-major, past grace → evict",
			labels:    withLastUsed(id.PnpmLabels("8", time.Time{}), lu(2*time.Hour)),
			wantEvict: true,
		},
		{
			name:      "externals current digest, fresh → keep",
			labels:    withLastUsed(ext.ExternalsLabels(time.Time{}), lu(time.Hour)),
			wantEvict: false,
		},
		{
			name:      "externals superseded digest, past grace → evict",
			labels:    withLastUsed(ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: "bbbbbbbbbbbb"}.ExternalsLabels(time.Time{}), lu(2*time.Hour)),
			wantEvict: true,
		},
		{
			name:      "externals superseded digest, WITHIN grace → keep",
			labels:    withLastUsed(ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: "bbbbbbbbbbbb"}.ExternalsLabels(time.Time{}), lu(time.Minute)),
			wantEvict: false,
		},
		{
			name:      "current-salt but AGED past stale window → evict (coarse backstop)",
			labels:    withLastUsed(id.ToolcacheLabels("2", time.Time{}), lu(31*24*time.Hour)),
			wantEvict: true,
		},
		{
			name:      "current-salt, just under the stale window → keep",
			labels:    withLastUsed(id.ToolcacheLabels("2", time.Time{}), lu(29*24*time.Hour)),
			wantEvict: false,
		},
		{
			name:      "diag-logs has no salt: never superseded, but ages out",
			labels:    withLastUsed(id.DiagLabels(time.Time{}), lu(31*24*time.Hour)),
			wantEvict: true,
		},
		{
			name:      "diag-logs fresh → keep",
			labels:    withLastUsed(id.DiagLabels(time.Time{}), lu(time.Hour)),
			wantEvict: false,
		},
		{
			name:      "unparseable last-used → conservatively keep",
			labels:    withLastUsed(id.ToolcacheLabels("1", time.Time{}), "not-a-timestamp"),
			wantEvict: false,
		},
		{
			name:      "missing last-used → conservatively keep",
			labels:    id.ToolcacheLabels("1", time.Time{}),
			wantEvict: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The "missing last-used" case: remove the label entirely.
			if tt.name == "missing last-used → conservatively keep" {
				delete(tt.labels, LabelLastUsed)
			}
			got, reason := EvaluateCacheEviction(tt.labels, cfg, gcNow)
			if got != tt.wantEvict {
				t.Errorf("EvaluateCacheEviction evict=%v (reason %q), want %v", got, reason, tt.wantEvict)
			}
			if got && reason == "" {
				t.Error("an eviction must carry a non-empty reason for the log")
			}
		})
	}
}

// TestEvaluateCacheEvictionNeverTouchesNonCache: a job-scoped or foreign label
// set (no cache=true) is NEVER evicted here, even if it happens to look aged —
// defense-in-depth so the GC decision can never reap a non-cache resource.
func TestEvaluateCacheEvictionNeverTouchesNonCache(t *testing.T) {
	cfg := CacheGCConfig{Generation: "2", PnpmMajor: "9", StaleDays: 1, Grace: time.Minute}

	// A job-scoped workspace volume's labels (managed + instance-name, NO cache).
	alloc := AllocationIdentity{ControllerID: "ctrl-1", PoolID: "p", InstanceName: "job-1"}
	jobLabels := alloc.WorkspaceVolumeLabels(gcNow.Add(-100 * 24 * time.Hour))
	if evict, _ := EvaluateCacheEviction(jobLabels, cfg, gcNow); evict {
		t.Error("GC decision evicted a non-cache (job-scoped) volume — it must only ever touch cache=true volumes")
	}

	// A foreign volume with a totally unrelated label map.
	foreign := map[string]string{"com.example/owner": "someone-else", LabelLastUsed: lu(100 * 24 * time.Hour)}
	if evict, _ := EvaluateCacheEviction(foreign, cfg, gcNow); evict {
		t.Error("GC decision evicted a foreign volume (no cache=true) — it must never")
	}
}

// TestEvaluateCacheEvictionUnknownDigestSkipsSupersession: when the provider
// could not resolve the current runner-image digest this pass (cfg.ImageDigest
// == ""), externals supersession is skipped (self-healing next pass) — the
// externals volume is only evicted if it also ages out.
func TestEvaluateCacheEvictionUnknownDigestSkipsSupersession(t *testing.T) {
	cfg := CacheGCConfig{Generation: "2", PnpmMajor: "9", ImageDigest: "", StaleDays: 30, Grace: time.Minute}
	ext := ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: "old-digest"}

	fresh := withLastUsed(ext.ExternalsLabels(time.Time{}), lu(time.Hour))
	if evict, _ := EvaluateCacheEviction(fresh, cfg, gcNow); evict {
		t.Error("with an unknown current digest, a fresh externals volume must NOT be evicted (supersession skipped)")
	}
	aged := withLastUsed(ext.ExternalsLabels(time.Time{}), lu(40*24*time.Hour))
	if evict, _ := EvaluateCacheEviction(aged, cfg, gcNow); !evict {
		t.Error("an aged externals volume must still be evicted by the age backstop even when the digest is unknown")
	}
}

// TestEvaluateCacheEvictionStaleDaysZeroDisablesAge: StaleDays<=0 disables the
// age backstop, so a very old but current-salt volume is kept.
func TestEvaluateCacheEvictionStaleDaysZeroDisablesAge(t *testing.T) {
	cfg := CacheGCConfig{Generation: "2", PnpmMajor: "9", StaleDays: 0, Grace: time.Minute}
	id := CacheVolumeIdentity{ControllerID: "ctrl-1", RepoKey: "repo-abc"}
	old := withLastUsed(id.ToolcacheLabels("2", time.Time{}), lu(1000*24*time.Hour))
	if evict, _ := EvaluateCacheEviction(old, cfg, gcNow); evict {
		t.Error("StaleDays=0 must disable age eviction; a current-salt volume must be kept regardless of age")
	}
}

// withLastUsed overwrites the last-used label on a copy of the given label map,
// so a test can place a cache volume's creation instant at a chosen age.
func withLastUsed(labels map[string]string, lastUsed string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	out[LabelLastUsed] = lastUsed
	return out
}
