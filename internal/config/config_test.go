package config

import (
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantErr    bool
		wantHost   string
		wantRunner string
	}{
		{
			name:       "valid config with explicit docker_host",
			path:       "testdata/valid.toml",
			wantHost:   "unix:///var/run/docker-custom.sock",
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:       "docker_host defaults when omitted",
			path:       "testdata/valid_defaults.toml",
			wantHost:   defaultDockerHost,
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:    "missing runner_image is rejected",
			path:    "testdata/missing_runner_image.toml",
			wantErr: true,
		},
		{
			name:    "malformed TOML is rejected",
			path:    "testdata/bad.toml",
			wantErr: true,
		},
		{
			name:       "full ADR-005-shaped config is tolerated (forward-compat)",
			path:       "testdata/forward_compat.toml",
			wantHost:   "unix:///var/run/docker.sock",
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		},
		{
			name:    "nonexistent file is rejected",
			path:    "testdata/does-not-exist.toml",
			wantErr: true,
		},
		{
			name:    "tag-only runner_image is rejected by default",
			path:    "testdata/unpinned_rejected.toml",
			wantErr: true,
		},
		{
			name:       "tag-only runner_image accepted with the dev escape hatch",
			path:       "testdata/unpinned_allowed.toml",
			wantHost:   defaultDockerHost,
			wantRunner: "ghcr.io/example/garm-runner-noble:dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load(%q) succeeded, want error", tt.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(%q) returned unexpected error: %v", tt.path, err)
			}
			if cfg.DockerHost != tt.wantHost {
				t.Errorf("DockerHost = %q, want %q", cfg.DockerHost, tt.wantHost)
			}
			if cfg.RunnerImage != tt.wantRunner {
				t.Errorf("RunnerImage = %q, want %q", cfg.RunnerImage, tt.wantRunner)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	// A valid, digest-pinned reference (name@sha256:<64-hex>).
	const digestRef = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "digest-pinned runner_image accepted",
			cfg:  Config{DockerHost: defaultDockerHost, RunnerImage: digestRef},
		},
		{
			name:    "tag-only runner_image rejected by default",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner:latest"},
			wantErr: true,
		},
		{
			name: "tag-only runner_image accepted with the escape hatch",
			cfg:  Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner:latest", AllowUnpinnedRunnerImage: true},
		},
		{
			name:    "malformed digest rejected",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner@sha256:00aa"},
			wantErr: true,
		},
		{
			name:    "malformed digest rejected even with the escape hatch",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner@sha256:00aa", AllowUnpinnedRunnerImage: true},
			wantErr: true,
		},
		{
			name:    "runner_image empty",
			cfg:     Config{DockerHost: defaultDockerHost, RunnerImage: ""},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() returned unexpected error: %v", err)
			}
		})
	}
}
