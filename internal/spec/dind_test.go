package spec

import (
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestDindCommand(t *testing.T) {
	got := DindCommand("overlay2")
	// F1: dockerd is launched with an explicit --group so the socket is
	// group-owned by DindSocketGID (the runner user is made a member of it).
	want := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + DindSocketGID}
	if !slices.Equal(got, want) {
		t.Fatalf("DindCommand(overlay2) = %v, want %v", got, want)
	}
	// The storage driver is passed through verbatim (vfs fallback, ADR-001).
	if got := DindCommand("vfs"); got[2] != "--storage-driver=vfs" {
		t.Errorf("DindCommand(vfs) storage arg = %q, want --storage-driver=vfs", got[2])
	}
	// F1: the socket group is a fixed, provider-controlled GID, distinct from
	// the runner user's own primary GID so socket access is an explicit
	// supplementary-group membership, not conflated with the runner identity.
	if DindSocketGID == RunnerGID {
		t.Errorf("DindSocketGID (%q) must differ from RunnerGID (%q)", DindSocketGID, RunnerGID)
	}
	// DindDockerHost is the exact socket the runner's DOCKER_HOST must match.
	if DindDockerHost != "unix:///run/docker.sock" {
		t.Errorf("DindDockerHost = %q, want unix:///run/docker.sock", DindDockerHost)
	}
}

func TestBuildDindContainerPrivilegedSidecar(t *testing.T) {
	priv, runtime, err := DindRuntimeSelection(dindModePrivilegedSidecar)
	if err != nil {
		t.Fatalf("DindRuntimeSelection returned unexpected error: %v", err)
	}

	cfg, host := BuildDindContainer(DindContainerSpec{
		Image:               "docker:dind@sha256:abc",
		Labels:              map[string]string{"garm.docker/role": "dind"},
		MemoryBytes:         4 << 30,
		StorageDriver:       "overlay2",
		Privileged:          priv,
		Runtime:             runtime,
		NetworkName:         "job-1-net",
		SocketVolumeName:    "job-1-socket",
		DindStateVolumeName: "job-1-dind-state",
		WorkspaceVolumeName: "job-1-workspace",
	})

	// dockerd argv listens ONLY on the shared unix socket, with the explicit
	// storage driver and the explicit socket --group (F1).
	wantCmd := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + DindSocketGID}
	if !slices.Equal([]string(cfg.Cmd), wantCmd) {
		t.Errorf("Cmd = %v, want %v", cfg.Cmd, wantCmd)
	}

	// TLS is off, structurally: DOCKER_TLS_CERTDIR="" is always present.
	if !hasExactEnv(cfg.Env, "DOCKER_TLS_CERTDIR=") {
		t.Errorf("Env = %v, want DOCKER_TLS_CERTDIR= (TLS off)", cfg.Env)
	}

	if cfg.Image != "docker:dind@sha256:abc" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.Labels["garm.docker/role"] != "dind" {
		t.Errorf("Labels = %v, want role=dind", cfg.Labels)
	}

	// privileged-sidecar: Privileged=true, default runtime (empty).
	if !host.Privileged {
		t.Error("Privileged = false, want true for privileged-sidecar")
	}
	if host.Runtime != "" {
		t.Errorf("Runtime = %q, want empty (default runtime) for privileged-sidecar", host.Runtime)
	}

	// Same per-job network as the runner, as the sole attachment.
	if host.NetworkMode != "job-1-net" || !host.NetworkMode.IsUserDefined() {
		t.Errorf("NetworkMode = %q, want the user-defined job network", host.NetworkMode)
	}

	if host.Resources.Memory != 4<<30 {
		t.Errorf("Memory = %d, want %d", host.Resources.Memory, 4<<30)
	}

	// Three mounts: socket at /run, dind-state at /var/lib/docker, and (F2) the
	// runner's workspace at RunnerWorkDir so nested bind sources resolve to the
	// real checked-out files daemon-side.
	assertMount(t, host.Mounts, "job-1-socket", DindSocketDir)
	assertMount(t, host.Mounts, "job-1-dind-state", DindStateDir)
	assertMount(t, host.Mounts, "job-1-workspace", RunnerWorkDir)
	if len(host.Mounts) != 3 {
		t.Errorf("Mounts = %d, want exactly 3 (socket + dind-state + workspace)", len(host.Mounts))
	}

	// NO host docker.sock is ever bound into the sidecar (ADR-001 red line):
	// every mount is a named volume, none a host bind of the docker socket.
	for _, m := range host.Mounts {
		if m.Type == mount.TypeBind || strings.Contains(m.Source, "docker.sock") {
			t.Errorf("sidecar has a host/bind mount %+v; no host docker.sock is ever mounted", m)
		}
	}
}

func TestBuildDindContainerSysboxRuncFlipsOnlyTwoFields(t *testing.T) {
	// WP4 preview: the sysbox-runc mode is a thin flip of {Privileged,Runtime}
	// via DindRuntimeSelection; everything else about the built container is
	// identical to privileged-sidecar.
	priv, runtime, err := DindRuntimeSelection(dindModeSysboxRunc)
	if err != nil {
		t.Fatalf("DindRuntimeSelection returned unexpected error: %v", err)
	}
	base := DindContainerSpec{
		Image:               "docker:dind@sha256:abc",
		StorageDriver:       "overlay2",
		NetworkName:         "job-1-net",
		SocketVolumeName:    "job-1-socket",
		DindStateVolumeName: "job-1-dind-state",
	}
	base.Privileged, base.Runtime = priv, runtime
	_, host := BuildDindContainer(base)

	if host.Privileged {
		t.Error("sysbox-runc must run Privileged=false")
	}
	if host.Runtime != "sysbox-runc" {
		t.Errorf("Runtime = %q, want sysbox-runc", host.Runtime)
	}
	// Topology is unchanged from privileged-sidecar: same two named mounts.
	assertMount(t, host.Mounts, "job-1-socket", DindSocketDir)
	assertMount(t, host.Mounts, "job-1-dind-state", DindStateDir)
}

func TestBuildDindContainerAppendsCallerEnvAfterTLSOff(t *testing.T) {
	cfg, _ := BuildDindContainer(DindContainerSpec{
		Image:         "docker:dind@sha256:abc",
		StorageDriver: "vfs",
		Env:           []string{"EXTRA=1"},
	})
	// TLS-off env comes first (structural), caller env after.
	if len(cfg.Env) < 2 || cfg.Env[0] != "DOCKER_TLS_CERTDIR=" || cfg.Env[1] != "EXTRA=1" {
		t.Errorf("Env = %v, want [DOCKER_TLS_CERTDIR=, EXTRA=1]", cfg.Env)
	}
}

func TestBuildRunnerContainerDinDSocketMountAndNoHostSocket(t *testing.T) {
	_, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:               "ghcr.io/example/runner@sha256:abc",
		WorkspaceVolumeName: "job-1-workspace",
		NetworkName:         "job-1-net",
		SocketVolumeName:    "job-1-socket",
	})

	// The runner mounts the SHARED socket volume at /run (the sole
	// runner→daemon channel) plus its workspace — and nothing else.
	assertMount(t, host.Mounts, "job-1-socket", DindSocketDir)
	assertMount(t, host.Mounts, "job-1-workspace", RunnerWorkDir)
	if len(host.Mounts) != 2 {
		t.Errorf("Mounts = %d, want 2 (workspace + socket)", len(host.Mounts))
	}
	// No host docker.sock bind on the runner either.
	for _, m := range host.Mounts {
		if m.Type == mount.TypeBind || strings.Contains(m.Source, "docker.sock") {
			t.Errorf("runner has a host/bind mount %+v; no host docker.sock is ever mounted", m)
		}
	}
	// Credential tmpfs is unchanged.
	if _, ok := host.Tmpfs[CredentialDir]; !ok {
		t.Errorf("HostConfig.Tmpfs missing %q", CredentialDir)
	}
	// F1: the runner gets the DinD socket GID as a supplementary group so the
	// unprivileged runner user can reach the shared dockerd socket.
	if !slices.Contains(host.GroupAdd, DindSocketGID) {
		t.Errorf("HostConfig.GroupAdd = %v, want it to include the DinD socket GID %q", host.GroupAdd, DindSocketGID)
	}
}

func TestBuildRunnerContainerNoneModeHasNoSocketMount(t *testing.T) {
	// none mode: no SocketVolumeName → only the workspace mount, no socket.
	_, host := BuildRunnerContainer(RunnerContainerSpec{
		Image:               "ghcr.io/example/runner@sha256:abc",
		WorkspaceVolumeName: "job-1-workspace",
		NetworkName:         "job-1-net",
	})
	if len(host.Mounts) != 1 || host.Mounts[0].Target != RunnerWorkDir {
		t.Errorf("Mounts = %+v, want only the workspace mount (no socket in none mode)", host.Mounts)
	}
	// F1: no DinD socket group in none mode (no DinD daemon to reach).
	if len(host.GroupAdd) != 0 {
		t.Errorf("HostConfig.GroupAdd = %v, want empty in none mode", host.GroupAdd)
	}
}

func TestBuildRunnerEnvEmitsDockerHostOnlyInDinDModes(t *testing.T) {
	// DinD mode (JIT): DOCKER_HOST points at the sidecar socket.
	jit := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: true,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    RunnerWorkDir,
		DockerHost:       DindDockerHost,
	})
	if !hasExactEnv(jit, "DOCKER_HOST=unix:///run/docker.sock") {
		t.Errorf("JIT DinD env = %v, want DOCKER_HOST set", jit)
	}
	// F1: DinD modes also carry the socket GID so the entrypoint can add the
	// runner user to a group with it before dropping privileges.
	if !hasExactEnv(jit, DindSocketGIDEnv+"="+DindSocketGID) {
		t.Errorf("JIT DinD env = %v, want %s=%s set", jit, DindSocketGIDEnv, DindSocketGID)
	}

	// DinD mode (non-JIT): still emitted, before the entity vars.
	nonJIT := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: false,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    RunnerWorkDir,
		RunnerName:       "r1",
		Entity:           Entity{Scope: EntityOrg, Org: "acme"},
		DockerHost:       DindDockerHost,
	})
	if !hasExactEnv(nonJIT, "DOCKER_HOST=unix:///run/docker.sock") {
		t.Errorf("non-JIT DinD env = %v, want DOCKER_HOST set", nonJIT)
	}

	// none mode: DockerHost empty → DOCKER_HOST absent (M0 behavior).
	none := BuildRunnerEnv(RunnerEnvOptions{
		JITConfigEnabled: true,
		GitHubURL:        "https://github.com",
		RunnerWorkDir:    RunnerWorkDir,
	})
	for _, e := range none {
		if strings.HasPrefix(e, "DOCKER_HOST=") {
			t.Errorf("none mode must not set DOCKER_HOST, got %q", e)
		}
		if strings.HasPrefix(e, DindSocketGIDEnv+"=") {
			t.Errorf("none mode must not set %s, got %q", DindSocketGIDEnv, e)
		}
	}
}

// --- helpers -----------------------------------------------------------------

func assertMount(t *testing.T, mounts []mount.Mount, source, target string) {
	t.Helper()
	for _, m := range mounts {
		if m.Source == source {
			if m.Type != mount.TypeVolume {
				t.Errorf("mount %q Type = %v, want a named volume", source, m.Type)
			}
			if m.Target != target {
				t.Errorf("mount %q Target = %q, want %q", source, m.Target, target)
			}
			return
		}
	}
	t.Errorf("no mount with Source %q found in %+v", source, mounts)
}

func hasExactEnv(env []string, want string) bool {
	return slices.Contains(env, want)
}
