package spec

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
)

func TestDiagVolumeName(t *testing.T) {
	const repoKey = "octo-org-octo-repo-0123456789ab"
	got := DiagVolumeName(repoKey)
	want := "garm-cache-diag-logs-" + repoKey
	if got != want {
		t.Errorf("DiagVolumeName = %q, want %q", got, want)
	}
	if err := ValidateDerivedName("diag volume", got); err != nil {
		t.Errorf("diag volume name %q is invalid: %v", got, err)
	}
}

func TestDiagLabels(t *testing.T) {
	id := CacheVolumeIdentity{ControllerID: "ctrl-1", RepoKey: "octo-org-octo-repo-0123456789ab"}
	lastUsed := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	labels := id.DiagLabels(lastUsed)

	assertLabel(t, labels, LabelManaged, "true")
	assertLabel(t, labels, LabelControllerID, "ctrl-1")
	assertLabel(t, labels, LabelCache, "true")
	assertLabel(t, labels, LabelCacheKind, string(CacheKindDiagLogs))
	assertLabel(t, labels, LabelRepo, "octo-org-octo-repo-0123456789ab")
	assertLabel(t, labels, LabelLastUsed, "2026-07-22T09:00:00Z")

	// A diag volume has no generation/pnpm salt and, like every cache volume, no
	// instance-name — so teardown/sweep never matches it.
	if _, ok := labels[LabelGeneration]; ok {
		t.Error("diag volume should not carry a generation salt")
	}
	if _, ok := labels[LabelInstanceName]; ok {
		t.Error("diag volume must NOT carry an instance-name label")
	}
	if MatchesPredicate(labels, "ctrl-1") {
		t.Error("diag volume MATCHED the ADR-004 teardown predicate — teardown would delete it")
	}
}

func TestDiagLogsMount(t *testing.T) {
	m := DiagLogsMount("garm-cache-diag-logs-repo")
	if m.Type != mount.TypeVolume || m.Source != "garm-cache-diag-logs-repo" {
		t.Errorf("diag mount = %+v, want the diag volume", m)
	}
	if m.Target != RunnerDiagDir {
		t.Errorf("diag mount target = %q, want %q", m.Target, RunnerDiagDir)
	}
	// The runner WRITES its diagnostics here: read-write is required.
	if m.ReadOnly {
		t.Error("diag mount must be read-write (the runner writes its logs there)")
	}
}

func TestBuildDiagPruneContainer(t *testing.T) {
	cfg, host := BuildDiagPruneContainer(DiagPruneContainerSpec{
		Image:         "runner@sha256:abc",
		VolumeName:    "garm-cache-diag-logs-repo",
		RetentionDays: 7,
		Labels:        map[string]string{LabelRole: RoleCacheHelper},
	})

	if len(cfg.Entrypoint) < 3 || cfg.Entrypoint[0] != "/bin/sh" {
		t.Fatalf("prune entrypoint = %v, want an sh -ec override", cfg.Entrypoint)
	}
	script := cfg.Entrypoint[2]

	// A retention-scoped `find -delete`, NOT any unscoped destructive op.
	for _, want := range []string{"find " + diagPruneMountDir, "-type f", "-mtime +7", "-delete"} {
		if !strings.Contains(script, want) {
			t.Errorf("prune script missing %q\nscript: %s", want, script)
		}
	}
	if strings.Contains(script, "system prune") || strings.Contains(script, "docker ") {
		t.Errorf("prune script must be an allowlist-scoped find, never a docker/system prune: %s", script)
	}

	// It mounts ONLY the one diag volume (allowlist-safe) read-write.
	if len(host.Mounts) != 1 {
		t.Fatalf("prune has %d mounts, want exactly 1 (the diag volume)", len(host.Mounts))
	}
	if host.Mounts[0].Source != "garm-cache-diag-logs-repo" || host.Mounts[0].Target != diagPruneMountDir {
		t.Errorf("prune mount = %+v, want the diag volume at %q", host.Mounts[0], diagPruneMountDir)
	}
}

// TestDiagPruneRetentionInScript: the configured retention window appears in the
// find -mtime predicate exactly.
func TestDiagPruneRetentionInScript(t *testing.T) {
	for _, days := range []int{1, 7, 30} {
		cfg, _ := BuildDiagPruneContainer(DiagPruneContainerSpec{RetentionDays: days})
		if want := "-mtime +" + itoa(days); !strings.Contains(cfg.Entrypoint[2], want) {
			t.Errorf("retention %d: prune script missing %q", days, want)
		}
	}
}
