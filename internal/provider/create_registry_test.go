package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

func TestCreateInstanceDinDRegistrySettings(t *testing.T) {
	fixed := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + spec.DindSocketGID}

	tests := []struct {
		name      string
		mirrors   []string
		insecure  []string
		wantExtra []string
	}{
		{name: "none configured leaves the argv unchanged"},
		{
			name:      "one mirror",
			mirrors:   []string{"http://mirror.lan.example:5000"},
			wantExtra: []string{"--registry-mirror=http://mirror.lan.example:5000"},
		},
		{
			name:     "mirrors and insecure registries",
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
			srv := newJITMetadataServer(t)
			defer srv.Close()

			p, fake := newDindTestProvider(t)
			p.cfg.RegistryMirrors = tt.mirrors
			p.cfg.InsecureRegistries = tt.insecure

			if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
				t.Fatalf("CreateInstance returned unexpected error: %v", err)
			}
			dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName("Test-Instance-01"))
			if err != nil {
				t.Fatalf("DinD sidecar was not created: %v", err)
			}
			want := append(slices.Clone(fixed), tt.wantExtra...)
			if !slices.Equal([]string(dind.Config.Cmd), want) {
				t.Errorf("sidecar Cmd = %v, want %v", dind.Config.Cmd, want)
			}
		})
	}
}

// TestCreateInstanceRegistrySettingsAreNotExtraSpecs pins the trust boundary:
// a pool's extra_specs cannot set the registry lists (the extra_specs schema
// has no such key, so the allocation is rejected before any Docker work), and
// the keys it may set do not disturb the operator's configured flags.
func TestCreateInstanceRegistrySettingsAreNotExtraSpecs(t *testing.T) {
	for _, key := range []string{"registry_mirrors", "insecure_registries"} {
		t.Run("rejects "+key, func(t *testing.T) {
			p, fake := newDindTestProvider(t)
			p.cfg.RegistryMirrors = []string{"http://mirror.lan.example:5000"}

			b := jitBootstrap("https://metadata.invalid/")
			b.ExtraSpecs = []byte(`{"` + key + `": ["http://evil.example:5000"]}`)

			_, err := p.CreateInstance(context.Background(), b)
			if err == nil {
				t.Fatalf("CreateInstance accepted extra_specs.%s, want a rejection", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name the rejected key %q", err, key)
			}
			if len(fake.Created) != 0 {
				t.Errorf("rejection created %d containers, want 0", len(fake.Created))
			}
		})
	}

	t.Run("an allowed override leaves the operator's flags in place", func(t *testing.T) {
		srv := newJITMetadataServer(t)
		defer srv.Close()

		p, fake := newDindTestProvider(t)
		p.cfg.RegistryMirrors = []string{"http://mirror.lan.example:5000"}
		p.cfg.InsecureRegistries = []string{"mirror.lan.example:5000"}

		b := jitBootstrap(srv.URL)
		b.ExtraSpecs = []byte(`{"storage_driver": "vfs"}`)
		if _, err := p.CreateInstance(context.Background(), b); err != nil {
			t.Fatalf("CreateInstance returned unexpected error: %v", err)
		}
		dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName("Test-Instance-01"))
		if err != nil {
			t.Fatalf("DinD sidecar was not created: %v", err)
		}
		want := []string{
			"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=vfs", "--group=" + spec.DindSocketGID,
			"--registry-mirror=http://mirror.lan.example:5000",
			"--insecure-registry=mirror.lan.example:5000",
		}
		if !slices.Equal([]string(dind.Config.Cmd), want) {
			t.Errorf("sidecar Cmd = %v, want %v", dind.Config.Cmd, want)
		}
	})
}
