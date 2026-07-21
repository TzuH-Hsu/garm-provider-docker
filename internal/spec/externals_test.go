package spec

import (
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
)

func TestExternalsVolumeName(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got := ExternalsVolumeName(digest)
	want := "garm-cache-externals-" + digest
	if got != want {
		t.Errorf("ExternalsVolumeName = %q, want %q", got, want)
	}
	// A different digest → a different volume (a new image self-seeds a fresh one).
	if ExternalsVolumeName(digest) == ExternalsVolumeName("ff"+digest[2:]) {
		t.Error("distinct image digests must produce distinct externals volume names")
	}
	// It is a valid Docker resource name.
	if err := ValidateDerivedName("externals volume", got); err != nil {
		t.Errorf("externals volume name %q is invalid: %v", got, err)
	}
}

func TestExternalsLabels(t *testing.T) {
	id := ExternalsVolumeIdentity{ControllerID: "ctrl-1", ImageDigest: "deadbeefcafe"}
	lastUsed := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	labels := id.ExternalsLabels(lastUsed)

	assertLabel(t, labels, LabelManaged, "true")
	assertLabel(t, labels, LabelControllerID, "ctrl-1")
	assertLabel(t, labels, LabelCache, "true")
	assertLabel(t, labels, LabelCacheKind, string(CacheKindExternals))
	assertLabel(t, labels, LabelImageDigest, "deadbeefcafe")
	assertLabel(t, labels, LabelLastUsed, "2026-07-22T12:00:00Z")

	// Shared across repos → NO repo label. And, like every cache volume, NO
	// instance-name (so ADR-004's teardown/sweep predicate structurally excludes
	// it) and no job-scoped labels.
	if _, ok := labels[LabelRepo]; ok {
		t.Error("externals volume must NOT carry a repo label (it is shared across repos)")
	}
	if _, ok := labels[LabelInstanceName]; ok {
		t.Error("externals volume must NOT carry an instance-name label")
	}
	if _, ok := labels[LabelCreateNonce]; ok {
		t.Error("externals volume must NOT carry a create-nonce label")
	}

	// The exclusion is load-bearing: neither teardown predicate may match it.
	if MatchesPredicate(labels, "ctrl-1") {
		t.Error("externals volume MATCHED the ADR-004 teardown predicate — teardown would delete it")
	}
	if IsManagedRunner(labels, "ctrl-1") {
		t.Error("externals volume was mistaken for a managed runner")
	}
}

func TestExternalsROMount(t *testing.T) {
	m := ExternalsROMount("garm-cache-externals-abc")
	if m.Type != mount.TypeVolume {
		t.Errorf("externals mount type = %q, want volume", m.Type)
	}
	if m.Source != "garm-cache-externals-abc" {
		t.Errorf("externals mount source = %q, want the externals volume", m.Source)
	}
	if m.Target != RunnerExternalsDir {
		t.Errorf("externals mount target = %q, want %q", m.Target, RunnerExternalsDir)
	}
	// RED-LINE F4: the runner must NOT be able to write the shared externals.
	if !m.ReadOnly {
		t.Error("externals mount MUST be read-only (cross-repo RCE prevention, ADR-003 F4)")
	}
}

func TestBuildExternalsSeedContainer(t *testing.T) {
	cfg, host := BuildExternalsSeedContainer(ExternalsSeedContainerSpec{
		Image:      "runner@sha256:abc",
		VolumeName: "garm-cache-externals-abc",
		Labels:     map[string]string{LabelRole: RoleCacheHelper},
	})

	// The image entrypoint is fully overridden with the seed script (the job's
	// own entrypoint/workflow never runs here).
	if len(cfg.Entrypoint) < 3 || cfg.Entrypoint[0] != "/bin/sh" {
		t.Fatalf("seed entrypoint = %v, want an sh -ec override", cfg.Entrypoint)
	}
	script := cfg.Entrypoint[2]

	// The lock + atomic-marker + copy-from-image mechanics must all be present.
	for _, want := range []string{
		"flock 9",                       // exactly-one-seeder lock (F15)
		externalsSeededMarker,           // the .seeded sentinel
		"cp -a \"" + RunnerExternalsDir, // copy FROM the image's own externals
		externalsSeedStagingDir,         // copy INTO the staged volume mount
	} {
		if !strings.Contains(script, want) {
			t.Errorf("seed script missing %q\nscript:\n%s", want, script)
		}
	}
	// The marker is written LAST, AFTER the copy — otherwise a crash mid-copy
	// would leave a "seeded" but partial tree (F15).
	if strings.Index(script, "cp -a") > strings.LastIndex(script, "touch \""+externalsSeededMarker) {
		t.Error("seed script must write the .seeded marker AFTER the copy, not before")
	}
	// The lock guard must precede the copy.
	if strings.Index(script, "flock 9") > strings.Index(script, "cp -a") {
		t.Error("seed script must take the flock BEFORE copying")
	}

	// The image CMD is dropped so nothing is appended to the seed argv.
	if len(cfg.Cmd) != 0 {
		t.Errorf("seed Cmd = %v, want empty (image CMD dropped)", cfg.Cmd)
	}

	// The volume is mounted READ-WRITE at the staging dir (the one place it is
	// ever written), NOT at the runner externals dir (which would shadow the
	// copy source), and it is the only mount.
	if len(host.Mounts) != 1 {
		t.Fatalf("seed has %d mounts, want exactly 1 (the externals volume)", len(host.Mounts))
	}
	seedMount := host.Mounts[0]
	if seedMount.Source != "garm-cache-externals-abc" || seedMount.Target != externalsSeedStagingDir {
		t.Errorf("seed mount = %+v, want the externals volume at %q", seedMount, externalsSeedStagingDir)
	}
	if seedMount.ReadOnly {
		t.Error("seed mount must be read-WRITE (it populates the volume)")
	}
	if seedMount.Target == RunnerExternalsDir {
		t.Error("seed must not mount the volume over the image's own externals (its copy source)")
	}
	// No network for the seed (the copy is purely local).
	if host.NetworkMode != "" {
		t.Errorf("seed NetworkMode = %q, want empty (no network)", host.NetworkMode)
	}
}
