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

func TestNamedWorkspaceMount(t *testing.T) {
	m := NamedWorkspaceMount("test-instance-01-workspace")
	if m.Type != mount.TypeVolume {
		t.Errorf("Type = %q, want volume", m.Type)
	}
	if m.Source != "test-instance-01-workspace" {
		t.Errorf("Source = %q, want the named workspace volume", m.Source)
	}
	// WP9-flagged JIT-workdir check: the workspace mount target MUST equal the
	// runner image's RUNNER_WORKDIR (/actions-runner/_work) so a job's _work
	// lands on the dedicated volume, not the container rootfs (ADR-002).
	if m.Target != RunnerWorkDir {
		t.Errorf("Target = %q, want %q (runner image RUNNER_WORKDIR)", m.Target, RunnerWorkDir)
	}
	if RunnerWorkDir != "/actions-runner/_work" {
		t.Errorf("RunnerWorkDir = %q, want /actions-runner/_work (runner-images/noble RUNNER_DIR + _work)", RunnerWorkDir)
	}
}

func TestBuildRunnerContainerNamedWorkspaceAndNetwork(t *testing.T) {
	cfg, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:               "ghcr.io/example/runner@sha256:abc",
		WorkspaceVolumeName: "job-1-workspace",
		NetworkName:         "job-1-net",
	})
	if cfg.Image != "ghcr.io/example/runner@sha256:abc" {
		t.Errorf("Image = %q", cfg.Image)
	}

	// The workspace is the NAMED volume mounted at the runner workdir.
	if len(host.Mounts) != 1 {
		t.Fatalf("Mounts = %d, want 1 (workspace only)", len(host.Mounts))
	}
	w := host.Mounts[0]
	if w.Type != mount.TypeVolume || w.Source != "job-1-workspace" || w.Target != RunnerWorkDir {
		t.Errorf("workspace mount = %+v, want the named volume at %q", w, RunnerWorkDir)
	}

	// The container joins the per-job network as its sole network.
	if host.NetworkMode != "job-1-net" {
		t.Errorf("NetworkMode = %q, want the job network", host.NetworkMode)
	}
	if !host.NetworkMode.IsUserDefined() {
		t.Errorf("NetworkMode %q should be user-defined (not the default bridge)", host.NetworkMode)
	}

	// The credential tmpfs is unchanged (short-syntax, runner uid/gid).
	if _, ok := host.Tmpfs[CredentialDir]; !ok {
		t.Errorf("HostConfig.Tmpfs missing the %q entry: %v", CredentialDir, host.Tmpfs)
	}
}

func TestBuildRunnerContainerDefaultsToAnonymousWorkspaceAndNoNetwork(t *testing.T) {
	// No WorkspaceVolumeName / NetworkName: the M0 fallback shape.
	_, host := BuildRunnerContainer(RunnerContainerSpec{Image: "x"})
	if len(host.Mounts) != 1 || host.Mounts[0].Source != "" {
		t.Errorf("Mounts = %+v, want a single anonymous (empty Source) workspace volume", host.Mounts)
	}
	if host.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty (default bridge) when no network is set", host.NetworkMode)
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

func TestDindRuntimeSelection(t *testing.T) {
	tests := []struct {
		name        string
		dindMode    string
		wantPriv    bool
		wantRuntime string
		wantErr     bool
	}{
		{
			name:        "privileged-sidecar runs privileged with the default runtime",
			dindMode:    "privileged-sidecar",
			wantPriv:    true,
			wantRuntime: "",
		},
		{
			name:        "sysbox-runc runs unprivileged with the sysbox-runc runtime",
			dindMode:    "sysbox-runc",
			wantPriv:    false,
			wantRuntime: "sysbox-runc",
		},
		{
			name:     "none has no runtime selection",
			dindMode: "none",
			wantErr:  true,
		},
		{
			name:     "unknown mode is an error",
			dindMode: "bogus",
			wantErr:  true,
		},
		{
			name:     "empty mode is an error",
			dindMode: "",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, runtime, err := DindRuntimeSelection(tt.dindMode)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DindRuntimeSelection(%q) succeeded, want error", tt.dindMode)
				}
				return
			}
			if err != nil {
				t.Fatalf("DindRuntimeSelection(%q) returned unexpected error: %v", tt.dindMode, err)
			}
			if priv != tt.wantPriv {
				t.Errorf("Privileged = %v, want %v", priv, tt.wantPriv)
			}
			if runtime != tt.wantRuntime {
				t.Errorf("Runtime = %q, want %q", runtime, tt.wantRuntime)
			}
		})
	}
}

func TestDindRuntimeSelectionModesAreMutuallyExclusive(t *testing.T) {
	// privileged-sidecar and sysbox-runc must never produce the same
	// {Privileged, Runtime} pair — that would make them indistinguishable
	// on the wire, defeating the whole point of ADR-001's two modes.
	privPriv, privRuntime, err := DindRuntimeSelection("privileged-sidecar")
	if err != nil {
		t.Fatalf("DindRuntimeSelection(privileged-sidecar) returned unexpected error: %v", err)
	}
	sysboxPriv, sysboxRuntime, err := DindRuntimeSelection("sysbox-runc")
	if err != nil {
		t.Fatalf("DindRuntimeSelection(sysbox-runc) returned unexpected error: %v", err)
	}
	if privPriv == sysboxPriv && privRuntime == sysboxRuntime {
		t.Fatalf("privileged-sidecar and sysbox-runc produced the identical pair {%v,%q}", privPriv, privRuntime)
	}
	// sysbox-runc must specifically be unprivileged (ADR-001's stated
	// security rationale for offering it at all).
	if sysboxPriv {
		t.Error("sysbox-runc mode must run Privileged=false")
	}
}

func TestDindContainerSpecFieldsAreIndependentOfRunnerContainerSpec(t *testing.T) {
	// DindContainerSpec is a genuinely separate type from
	// RunnerContainerSpec — populating one must never reach into or be
	// confused with the other. This is mostly a compile-time guarantee
	// (they're different struct types), but assert the values round-trip
	// independently as a smoke test against an accidental shared-field typo.
	spec := DindContainerSpec{
		Image:         "docker:dind@sha256:abc",
		Env:           []string{"DOCKER_TLS_CERTDIR="},
		Labels:        map[string]string{"garm.docker/role": "dind"},
		MemoryBytes:   4 << 30,
		StorageDriver: "overlay2",
		Privileged:    true,
		Runtime:       "",
	}

	if spec.Image != "docker:dind@sha256:abc" {
		t.Errorf("Image = %q", spec.Image)
	}
	if len(spec.Env) != 1 || spec.Env[0] != "DOCKER_TLS_CERTDIR=" {
		t.Errorf("Env = %v", spec.Env)
	}
	if spec.Labels["garm.docker/role"] != "dind" {
		t.Errorf("Labels = %v", spec.Labels)
	}
	if spec.MemoryBytes != 4<<30 {
		t.Errorf("MemoryBytes = %d", spec.MemoryBytes)
	}
	if spec.StorageDriver != "overlay2" {
		t.Errorf("StorageDriver = %q", spec.StorageDriver)
	}
	if !spec.Privileged {
		t.Error("Privileged = false, want true")
	}
	if spec.Runtime != "" {
		t.Errorf("Runtime = %q, want empty", spec.Runtime)
	}
}
