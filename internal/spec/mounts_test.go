package spec

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

// parseTmpfsOptions splits a HostConfig.Tmpfs short-syntax option string
// ("rw,noexec,…,mode=0700,uid=1001,gid=1001") into a set of flags and a
// key→value map, so a test can assert the individual options without
// depending on their order.
func parseTmpfsOptions(t *testing.T, opts string) (flags map[string]bool, kv map[string]string) {
	t.Helper()
	flags = map[string]bool{}
	kv = map[string]string{}
	for _, part := range strings.Split(opts, ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			kv[k] = v
		} else {
			flags[part] = true
		}
	}
	return flags, kv
}

func TestCredentialTmpfsMap(t *testing.T) {
	m := CredentialTmpfsMap()
	opts, ok := m[CredentialDir]
	if !ok {
		t.Fatalf("CredentialTmpfsMap missing the %q entry: %v", CredentialDir, m)
	}
	if len(m) != 1 {
		t.Errorf("CredentialTmpfsMap has %d entries, want exactly 1 (the credential dir)", len(m))
	}

	flags, kv := parseTmpfsOptions(t, opts)

	// The tmpfs must be owned by the runner uid/gid so the unprivileged
	// delivery exec can write into it and the runner can read it back
	// (ADR-002 F2). Short-syntax uid/gid is exactly what the real daemon
	// honors (and the long-syntax it rejects), so this is the load-bearing
	// assertion for defect 1.
	if kv["uid"] != RunnerUID || kv["gid"] != RunnerGID {
		t.Errorf("tmpfs uid/gid = uid=%q gid=%q, want uid=%s gid=%s", kv["uid"], kv["gid"], RunnerUID, RunnerGID)
	}
	if kv["mode"] != "0700" {
		t.Errorf("tmpfs mode = %q, want 0700", kv["mode"])
	}
	// A size cap must be present (ADR-002 F2) and non-empty.
	if kv["size"] == "" {
		t.Errorf("tmpfs size cap missing in options %q", opts)
	}
	// Hardening flags.
	for _, want := range []string{"noexec", "nosuid", "nodev"} {
		if !flags[want] {
			t.Errorf("tmpfs options %q missing hardening flag %q", opts, want)
		}
	}
}

func TestWorkspaceMountIsAnonymousVolume(t *testing.T) {
	m := WorkspaceMount()
	if m.Type != mount.TypeVolume {
		t.Errorf("Type = %q, want volume", m.Type)
	}
	if m.Target != RunnerWorkDir {
		t.Errorf("Target = %q, want %q", m.Target, RunnerWorkDir)
	}
	// Empty Source == anonymous volume (M0). A named source here would
	// mean a persistent/shared volume, which is an M1 concern.
	if m.Source != "" {
		t.Errorf("workspace Source = %q, want empty (anonymous volume)", m.Source)
	}
}

func TestBuildRunnerContainer(t *testing.T) {
	labels := map[string]string{"garm.docker/managed": "true"}
	env := []string{"JIT_CONFIG_ENABLED=true"}

	cfg, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:  "ghcr.io/example/runner@sha256:abc",
		Env:    env,
		Labels: labels,
	})

	if cfg.Image != "ghcr.io/example/runner@sha256:abc" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "JIT_CONFIG_ENABLED=true" {
		t.Errorf("Env = %v", cfg.Env)
	}
	if cfg.Labels["garm.docker/managed"] != "true" {
		t.Errorf("Labels = %v", cfg.Labels)
	}

	// The credential tmpfs lives on HostConfig.Tmpfs (short syntax), NOT in
	// Mounts — a Mounts long-syntax tmpfs carrying uid/gid is what the real
	// daemon rejects (defect 1). No tmpfs may appear in Mounts.
	tmpfsOpts, ok := host.Tmpfs[CredentialDir]
	if !ok {
		t.Errorf("HostConfig.Tmpfs missing the %q entry: %v", CredentialDir, host.Tmpfs)
	}
	if _, kv := parseTmpfsOptions(t, tmpfsOpts); kv["uid"] != RunnerUID || kv["gid"] != RunnerGID {
		t.Errorf("HostConfig.Tmpfs[%q] = %q, want runner uid/gid", CredentialDir, tmpfsOpts)
	}

	// Mounts holds ONLY the workspace volume now.
	if len(host.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (workspace only)", len(host.Mounts))
	}
	if host.Mounts[0].Target != RunnerWorkDir || host.Mounts[0].Type != mount.TypeVolume {
		t.Errorf("Mounts[0] = %+v, want a volume at the runner workdir", host.Mounts[0])
	}
	for _, m := range host.Mounts {
		if m.Type == mount.TypeTmpfs {
			t.Errorf("no tmpfs may appear in Mounts (long-syntax uid/gid is daemon-rejected); got %+v", m)
		}
	}

	// M0 default: no memory limit.
	if host.Resources.Memory != 0 {
		t.Errorf("Memory = %d, want 0 (unlimited) by default", host.Resources.Memory)
	}
}

func TestBuildRunnerContainerMemoryLimit(t *testing.T) {
	_, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:       "x",
		MemoryBytes: 512 << 20,
	})
	if host.Resources.Memory != 512<<20 {
		t.Errorf("Memory = %d, want %d", host.Resources.Memory, 512<<20)
	}
}
