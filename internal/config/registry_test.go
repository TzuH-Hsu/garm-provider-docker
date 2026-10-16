package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const registryTestRunnerImage = "ghcr.io/example/garm-runner-noble@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

func TestConfigValidateRegistryMirrors(t *testing.T) {
	tests := []struct {
		name    string
		mirrors []string
		// wantErr is a substring of the expected error; empty means valid.
		wantErr string
	}{
		{name: "unset is valid", mirrors: nil},
		{name: "empty list is valid", mirrors: []string{}},
		{name: "https host", mirrors: []string{"https://mirror.gcr.io"}},
		{name: "http host and port", mirrors: []string{"http://mirror.lan.example:5000"}},
		{name: "trailing slash allowed", mirrors: []string{"https://mirror.gcr.io/"}},
		{name: "IPv4 host and port", mirrors: []string{"http://10.0.0.5:5000"}},
		{name: "bracketed IPv6 host and port", mirrors: []string{"http://[fd00::5]:5000"}},
		{name: "mixed-case host", mirrors: []string{"https://Mirror.Example"}},
		{name: "uppercase scheme", mirrors: []string{"HTTPS://mirror.example"}, wantErr: "scheme"},
		{name: "several distinct mirrors", mirrors: []string{"https://mirror.gcr.io", "http://10.0.0.5:5000"}},

		{name: "empty entry", mirrors: []string{""}, wantErr: "registry_mirrors"},
		{name: "blank entry", mirrors: []string{"  "}, wantErr: "registry_mirrors"},
		{name: "no scheme", mirrors: []string{"mirror.lan.example:5000"}, wantErr: "scheme"},
		{name: "no scheme, bare host", mirrors: []string{"mirror.lan.example"}, wantErr: "scheme"},
		{name: "ftp scheme", mirrors: []string{"ftp://mirror.lan.example"}, wantErr: "scheme"},
		{name: "unix scheme", mirrors: []string{"unix:///var/run/mirror.sock"}, wantErr: "scheme"},
		{name: "no host", mirrors: []string{"http://"}, wantErr: "host"},
		{name: "no host, port only", mirrors: []string{"http://:5000"}, wantErr: "host"},
		{name: "path", mirrors: []string{"https://mirror.example/dockerhub"}, wantErr: "path"},
		{name: "nested path", mirrors: []string{"https://mirror.example/a/b/"}, wantErr: "path"},
		{name: "query", mirrors: []string{"https://mirror.example/?x=1"}, wantErr: "query"},
		{name: "empty query marker", mirrors: []string{"https://mirror.example?"}, wantErr: "query"},
		{name: "fragment", mirrors: []string{"https://mirror.example/#frag"}, wantErr: "fragment"},
		{name: "empty fragment marker", mirrors: []string{"https://mirror.example#"}, wantErr: "fragment"},
		{name: "userinfo", mirrors: []string{"https://user:pass@mirror.example"}, wantErr: "userinfo"},
		{name: "userinfo without password", mirrors: []string{"https://user@mirror.example"}, wantErr: "userinfo"},
		{name: "port out of range", mirrors: []string{"http://mirror.example:70000"}, wantErr: "port"},
		{name: "port zero", mirrors: []string{"http://mirror.example:0"}, wantErr: "port"},
		{name: "non-numeric port", mirrors: []string{"http://mirror.example:abc"}, wantErr: "registry_mirrors"},
		{name: "space in host", mirrors: []string{"http://mirror example"}, wantErr: "registry_mirrors"},
		{name: "invalid host characters", mirrors: []string{"http://mirror_host!.example"}, wantErr: "host"},

		{
			name:    "exact duplicate",
			mirrors: []string{"https://mirror.gcr.io", "https://mirror.gcr.io"},
			wantErr: "duplicate",
		},
		{
			name:    "duplicate differing only by trailing slash",
			mirrors: []string{"https://mirror.gcr.io", "https://mirror.gcr.io/"},
			wantErr: "duplicate",
		},
		{
			name:    "duplicate differing only by host case",
			mirrors: []string{"https://mirror.gcr.io", "https://Mirror.GCR.io"},
			wantErr: "duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig(registryTestRunnerImage, false)
			cfg.RegistryMirrors = tt.mirrors
			assertValidation(t, cfg.Validate(), tt.wantErr)
		})
	}
}

func TestConfigValidateInsecureRegistries(t *testing.T) {
	tests := []struct {
		name       string
		registries []string
		wantErr    string
	}{
		{name: "unset is valid", registries: nil},
		{name: "empty list is valid", registries: []string{}},
		{name: "host", registries: []string{"mirror.lan.example"}},
		{name: "host and port", registries: []string{"mirror.lan.example:5000"}},
		{name: "single-label host", registries: []string{"mirror:5000"}},
		{name: "IPv4 and port", registries: []string{"10.0.0.5:5000"}},
		{name: "IPv4 only", registries: []string{"10.0.0.5"}},
		{name: "bracketed IPv6 and port", registries: []string{"[fd00::5]:5000"}},
		{name: "bare IPv6", registries: []string{"fd00::5"}},
		{name: "IPv4 CIDR", registries: []string{"192.168.0.0/16"}},
		{name: "IPv6 CIDR", registries: []string{"fd00::/8"}},
		{name: "several distinct", registries: []string{"mirror.lan.example:5000", "10.0.0.0/8"}},

		{name: "empty entry", registries: []string{""}, wantErr: "insecure_registries"},
		{name: "blank entry", registries: []string{" "}, wantErr: "insecure_registries"},
		{name: "http scheme", registries: []string{"http://mirror.lan.example:5000"}, wantErr: "scheme"},
		{name: "https scheme", registries: []string{"https://mirror.lan.example"}, wantErr: "scheme"},
		{name: "path", registries: []string{"mirror.lan.example:5000/v2"}, wantErr: "insecure_registries"},
		{name: "leading hyphen", registries: []string{"-mirror.example"}, wantErr: "hyphen"},
		{name: "trailing hyphen", registries: []string{"mirror.example-"}, wantErr: "hyphen"},
		{name: "port out of range", registries: []string{"mirror.example:70000"}, wantErr: "port"},
		{name: "non-numeric port", registries: []string{"mirror.example:abc"}, wantErr: "insecure_registries"},
		{name: "invalid host characters", registries: []string{"mirror_host!.example"}, wantErr: "host"},
		{name: "CIDR with bad prefix length", registries: []string{"10.0.0.0/33"}, wantErr: "insecure_registries"},
		{name: "space in entry", registries: []string{"mirror example"}, wantErr: "insecure_registries"},

		{
			name:       "exact duplicate",
			registries: []string{"mirror.lan.example:5000", "mirror.lan.example:5000"},
			wantErr:    "duplicate",
		},
		{
			name:       "duplicate differing only by case",
			registries: []string{"Mirror.LAN.example:5000", "mirror.lan.example:5000"},
			wantErr:    "duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBaseConfig(registryTestRunnerImage, false)
			cfg.InsecureRegistries = tt.registries
			assertValidation(t, cfg.Validate(), tt.wantErr)
		})
	}
}

// TestRegistrySettingsValidatedInNoneMode pins that a malformed entry is
// caught at load even when dind_mode is "none", so a typo does not wait for
// the operator to enable DinD (the same stance as dind_image).
func TestRegistrySettingsValidatedInNoneMode(t *testing.T) {
	cfg := validBaseConfig(registryTestRunnerImage, false)
	cfg.DindMode = DindModeNone
	cfg.RegistryMirrors = []string{"not-a-url"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() succeeded for a malformed mirror under dind_mode=none, want error")
	}
}

// TestLoadRegistrySettings drives the real TOML loader: the keys are top-level
// (next to dind_image and storage_driver), default to empty, and a bad value
// fails Load with a message naming the key.
func TestLoadRegistrySettings(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return path
	}
	base := "runner_image = \"" + registryTestRunnerImage + "\"\n"

	t.Run("defaults are empty", func(t *testing.T) {
		cfg, err := Load(write(t, base))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.RegistryMirrors) != 0 || len(cfg.InsecureRegistries) != 0 {
			t.Errorf("defaults = %v / %v, want both empty", cfg.RegistryMirrors, cfg.InsecureRegistries)
		}
	})

	t.Run("parses both lists in order", func(t *testing.T) {
		cfg, err := Load(write(t, base+
			"registry_mirrors = [\"http://mirror.lan.example:5000\", \"https://mirror.gcr.io\"]\n"+
			"insecure_registries = [\"mirror.lan.example:5000\", \"10.0.0.0/8\"]\n"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if want := []string{"http://mirror.lan.example:5000", "https://mirror.gcr.io"}; !slices.Equal(cfg.RegistryMirrors, want) {
			t.Errorf("RegistryMirrors = %v, want %v", cfg.RegistryMirrors, want)
		}
		if want := []string{"mirror.lan.example:5000", "10.0.0.0/8"}; !slices.Equal(cfg.InsecureRegistries, want) {
			t.Errorf("InsecureRegistries = %v, want %v", cfg.InsecureRegistries, want)
		}
	})

	t.Run("a bad mirror fails Load and names the key", func(t *testing.T) {
		_, err := Load(write(t, base+"registry_mirrors = [\"https://mirror.example/path\"]\n"))
		if err == nil || !strings.Contains(err.Error(), "registry_mirrors") {
			t.Fatalf("Load error = %v, want one naming registry_mirrors", err)
		}
	})

	t.Run("a bad insecure registry fails Load and names the key", func(t *testing.T) {
		_, err := Load(write(t, base+"insecure_registries = [\"http://mirror.example\"]\n"))
		if err == nil || !strings.Contains(err.Error(), "insecure_registries") {
			t.Fatalf("Load error = %v, want one naming insecure_registries", err)
		}
	})
}

// TestFlavorCannotCarryRegistrySettings pins the global-only decision: the
// registry lists are not Flavor fields, so a [flavors.<name>] table naming them
// leaves them undecoded rather than overriding the global setting.
func TestFlavorCannotCarryRegistrySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "runner_image = \"" + registryTestRunnerImage + "\"\n" +
		"[flavors.big]\nregistry_mirrors = [\"https://mirror.gcr.io\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.RegistryMirrors) != 0 {
		t.Errorf("a flavor key must not set the global RegistryMirrors, got %v", cfg.RegistryMirrors)
	}
}

func assertValidation(t *testing.T, err error, wantErr string) {
	t.Helper()
	if wantErr == "" {
		if err != nil {
			t.Fatalf("Validate() returned unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("Validate() succeeded, want an error containing %q", wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("Validate() error = %q, want it to contain %q", err, wantErr)
	}
}
