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

	// A retention-scoped `find -delete`, NOT any unscoped destructive op. L8: the
	// window is minute-precise (-mmin +<days*1440>), not -mtime (which rounds down
	// to whole days and effectively widens 7d to 8d).
	for _, want := range []string{"find " + diagPruneMountDir, "-type f", "-mmin +10080", "-delete"} {
		if !strings.Contains(script, want) {
			t.Errorf("prune script missing %q\nscript: %s", want, script)
		}
	}
	if strings.Contains(script, "-mtime") {
		t.Errorf("prune script must use minute-precise -mmin, not the day-rounding -mtime: %s", script)
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
	// No network (L9): the prune joins the "none" network, not the default bridge.
	if !host.NetworkMode.IsNone() {
		t.Errorf("prune NetworkMode = %q, want none", host.NetworkMode)
	}
}

// TestDiagPruneRetentionInScript: the configured retention window appears in the
// find -mmin predicate exactly, converted to minutes (L8).
func TestDiagPruneRetentionInScript(t *testing.T) {
	for _, days := range []int{1, 7, 30} {
		cfg, _ := BuildDiagPruneContainer(DiagPruneContainerSpec{RetentionDays: days})
		if want := "-mmin +" + itoa(days*24*60); !strings.Contains(cfg.Entrypoint[2], want) {
			t.Errorf("retention %d: prune script missing %q", days, want)
		}
	}
}

// TestDiagPruneStampName: the stamp name is deterministic per controller (it is
// the cross-process claim), differs between controllers sharing a daemon, and is
// a legal container name whatever the controller ID's spelling.
func TestDiagPruneStampName(t *testing.T) {
	a := DiagPruneStampName("controller-abc")
	if a != DiagPruneStampName("controller-abc") {
		t.Errorf("DiagPruneStampName is not deterministic: %q", a)
	}
	if b := DiagPruneStampName("controller-xyz"); a == b {
		t.Errorf("two controllers share the stamp name %q", a)
	}
	for _, id := range []string{"controller-abc", "Ctrl With Spaces/and:colons", ""} {
		name := DiagPruneStampName(id)
		if !strings.HasPrefix(name, "garm-diagprune-stamp-") {
			t.Errorf("DiagPruneStampName(%q) = %q, want the garm-diagprune-stamp- prefix", id, name)
		}
		if err := ValidateDerivedName("diag-prune stamp", name); err != nil {
			t.Errorf("DiagPruneStampName(%q) = %q is not a legal name: %v", id, name, err)
		}
	}
}

// TestBuildDiagPruneStampContainer: the stamp mounts nothing (so it never pins a
// volume against an operator's prune), joins no network, carries the given
// labels, and does nothing if someone starts it.
func TestBuildDiagPruneStampContainer(t *testing.T) {
	labels := map[string]string{LabelRole: RoleDiagPruneStamp}
	cfg, host := BuildDiagPruneStampContainer("runner@sha256:abc", labels)
	if cfg.Image != "runner@sha256:abc" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.Labels[LabelRole] != RoleDiagPruneStamp {
		t.Errorf("Labels = %v, want role=%s", cfg.Labels, RoleDiagPruneStamp)
	}
	if len(cfg.Entrypoint) != 1 || cfg.Entrypoint[0] != "true" || len(cfg.Cmd) != 0 {
		t.Errorf("Entrypoint/Cmd = %v/%v, want [true]/[]", cfg.Entrypoint, cfg.Cmd)
	}
	if len(host.Mounts) != 0 || len(host.Binds) != 0 {
		t.Errorf("stamp mounts %v / binds %v, want none", host.Mounts, host.Binds)
	}
	if !host.NetworkMode.IsNone() {
		t.Errorf("NetworkMode = %q, want none", host.NetworkMode)
	}
}
