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
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:0000000000000000000000000000000000000000000000000000000000aa",
		},
		{
			name:       "docker_host defaults when omitted",
			path:       "testdata/valid_defaults.toml",
			wantHost:   defaultDockerHost,
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:0000000000000000000000000000000000000000000000000000000000aa",
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
			wantRunner: "ghcr.io/example/garm-runner-noble@sha256:0000000000000000000000000000000000000000000000000000000000aa",
		},
		{
			name:    "nonexistent file is rejected",
			path:    "testdata/does-not-exist.toml",
			wantErr: true,
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
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name: "runner_image set",
			cfg:  Config{DockerHost: defaultDockerHost, RunnerImage: "ghcr.io/example/runner:latest"},
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
