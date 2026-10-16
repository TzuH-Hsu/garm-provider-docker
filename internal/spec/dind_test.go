package spec

import (
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestDindCommand(t *testing.T) {
	got := DindCommand("overlay2", nil, nil)
	// F1: dockerd is launched with an explicit --group so the socket is
	// group-owned by DindSocketGID (the runner user is made a member of it).
	want := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + DindSocketGID}
	if !slices.Equal(got, want) {
		t.Fatalf("DindCommand(overlay2) = %v, want %v", got, want)
	}
	// The storage driver is passed through verbatim (vfs fallback, ADR-001).
	if got := DindCommand("vfs", nil, nil); got[2] != "--storage-driver=vfs" {
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

// TestDindCommandRegistryFlags covers the typed registry flags: one
// --registry-mirror per mirror and one --insecure-registry per entry, in
// configured order, always AFTER the fixed flags (host, storage driver,
// group), which stay first and unchanged. Empty and nil lists add nothing.
func TestDindCommandRegistryFlags(t *testing.T) {
	fixed := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + DindSocketGID}

	tests := []struct {
		name      string
		mirrors   []string
		insecure  []string
		wantExtra []string
	}{
		{name: "nil lists add nothing"},
		{name: "empty lists add nothing", mirrors: []string{}, insecure: []string{}},
		{
			name:      "one mirror",
			mirrors:   []string{"http://mirror.lan.example:5000"},
			wantExtra: []string{"--registry-mirror=http://mirror.lan.example:5000"},
		},
		{
			name:      "one insecure registry",
			insecure:  []string{"mirror.lan.example:5000"},
			wantExtra: []string{"--insecure-registry=mirror.lan.example:5000"},
		},
		{
			name:     "many of each, mirrors first, order preserved",
			mirrors:  []string{"https://mirror.gcr.io", "http://10.0.0.5:5000"},
			insecure: []string{"10.0.0.5:5000", "192.168.0.0/16"},
			wantExtra: []string{
				"--registry-mirror=https://mirror.gcr.io",
				"--registry-mirror=http://10.0.0.5:5000",
				"--insecure-registry=10.0.0.5:5000",
				"--insecure-registry=192.168.0.0/16",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DindCommand("overlay2", tt.mirrors, tt.insecure)
			want := append(slices.Clone(fixed), tt.wantExtra...)
			if !slices.Equal(got, want) {
				t.Errorf("DindCommand = %v, want %v", got, want)
			}
		})
	}
}

func TestBuildDindContainerRegistryFlags(t *testing.T) {
	cfg, _ := BuildDindContainer(DindContainerSpec{
		Image:               "docker:dind@sha256:abc",
		StorageDriver:       "overlay2",
		SocketVolumeName:    "s",
		DindStateVolumeName: "d",
		RegistryMirrors:     []string{"http://mirror.lan.example:5000"},
		InsecureRegistries:  []string{"mirror.lan.example:5000"},
	})
	want := []string{
		"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + DindSocketGID,
		"--registry-mirror=http://mirror.lan.example:5000",
		"--insecure-registry=mirror.lan.example:5000",
	}
	if !slices.Equal([]string(cfg.Cmd), want) {
		t.Errorf("Cmd = %v, want %v", cfg.Cmd, want)
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
		NetnsContainerID:    "runner-id-1",
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

	// Joins the runner's network namespace (shared netns amendment), so the
	// inner daemon's published ports are reachable on the runner's 127.0.0.1.
	if host.NetworkMode != "container:runner-id-1" || !host.NetworkMode.IsContainer() || host.NetworkMode.ConnectedContainer() != "runner-id-1" {
		t.Errorf("NetworkMode = %q, want container:runner-id-1 (the runner's namespace)", host.NetworkMode)
	}
	// The sidecar sets no sysctl of its own: the runner owns the namespace
	// and its IPv6 sysctl (RunnerIPv6Sysctl) covers both.
	if len(host.Sysctls) != 0 {
		t.Errorf("Sysctls = %v, want none on the sidecar", host.Sysctls)
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
	// Same two named mounts as privileged-sidecar.
	assertMount(t, host.Mounts, "job-1-socket", DindSocketDir)
	assertMount(t, host.Mounts, "job-1-dind-state", DindStateDir)
	// sysbox-runc keeps its own namespace on the per-job network
	// (DindSharesRunnerNetns is false for it).
	if host.NetworkMode != "job-1-net" || !host.NetworkMode.IsUserDefined() {
		t.Errorf("NetworkMode = %q, want the user-defined job network", host.NetworkMode)
	}
}

func TestBuildDindContainerNetnsContainerIDTakesPrecedence(t *testing.T) {
	_, host := BuildDindContainer(DindContainerSpec{
		Image:               "docker:dind@sha256:abc",
		StorageDriver:       "overlay2",
		NetnsContainerID:    "runner-id-1",
		NetworkName:         "job-1-net",
		SocketVolumeName:    "job-1-socket",
		DindStateVolumeName: "job-1-dind-state",
	})
	if host.NetworkMode != ContainerNetworkMode("runner-id-1") {
		t.Errorf("NetworkMode = %q, want %q: NetnsContainerID wins over NetworkName", host.NetworkMode, ContainerNetworkMode("runner-id-1"))
	}
}

func TestBuildDindContainerWithoutPlacementLeavesNetworkModeUnset(t *testing.T) {
	_, host := BuildDindContainer(DindContainerSpec{
		Image:               "docker:dind@sha256:abc",
		StorageDriver:       "overlay2",
		SocketVolumeName:    "job-1-socket",
		DindStateVolumeName: "job-1-dind-state",
	})
	if host.NetworkMode != "" {
		t.Errorf("NetworkMode = %q, want empty when neither NetnsContainerID nor NetworkName is set", host.NetworkMode)
	}
}

func TestDindSharesRunnerNetns(t *testing.T) {
	tests := []struct {
		mode string
		want bool
	}{
		{dindModePrivilegedSidecar, true},
		{dindModeSysboxRunc, false},
		{dindModeNone, false},
		{"", false},
		{"bogus", false},
	}
	for _, tc := range tests {
		if got := DindSharesRunnerNetns(tc.mode); got != tc.want {
			t.Errorf("DindSharesRunnerNetns(%q) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestContainerNetworkMode(t *testing.T) {
	got := ContainerNetworkMode("abc123")
	if got != "container:abc123" || !got.IsContainer() || got.ConnectedContainer() != "abc123" {
		t.Errorf("ContainerNetworkMode(abc123) = %q, want container:abc123", got)
	}
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
