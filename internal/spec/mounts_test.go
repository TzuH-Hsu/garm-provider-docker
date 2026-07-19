package spec

import (
	"os"
	"testing"

	"github.com/docker/docker/api/types/mount"
)

func TestCredentialTmpfsMount(t *testing.T) {
	m := CredentialTmpfsMount()
	if m.Type != mount.TypeTmpfs {
		t.Errorf("Type = %q, want tmpfs", m.Type)
	}
	if m.Target != CredentialDir {
		t.Errorf("Target = %q, want %q", m.Target, CredentialDir)
	}
	if m.Source != "" {
		t.Errorf("tmpfs Source must be empty, got %q", m.Source)
	}
	if m.TmpfsOptions == nil {
		t.Fatal("TmpfsOptions is nil, want mode set")
	}
	if m.TmpfsOptions.Mode != os.FileMode(0o700) {
		t.Errorf("tmpfs Mode = %o, want 0700", m.TmpfsOptions.Mode)
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

	// Both mounts must be present: credential tmpfs and workspace volume.
	if len(host.Mounts) != 2 {
		t.Fatalf("Mounts = %d, want 2", len(host.Mounts))
	}
	var haveTmpfs, haveWorkspace bool
	for _, m := range host.Mounts {
		switch m.Target {
		case CredentialDir:
			haveTmpfs = m.Type == mount.TypeTmpfs
		case RunnerWorkDir:
			haveWorkspace = m.Type == mount.TypeVolume
		}
	}
	if !haveTmpfs {
		t.Error("expected a tmpfs mount at the credential dir")
	}
	if !haveWorkspace {
		t.Error("expected a volume mount at the runner workdir")
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
