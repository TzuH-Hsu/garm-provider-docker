package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
)

func TestFakeNetworkCreateErrCleanFailure(t *testing.T) {
	f := NewFakeClient()
	f.NetworkCreateErr = errors.New("daemon boom")

	_, err := f.NetworkCreate(context.Background(), "job-1-net", network.CreateOptions{})
	if !errors.Is(err, f.NetworkCreateErr) {
		t.Fatalf("NetworkCreate err = %v, want the injected error", err)
	}
	// A clean failure records nothing.
	out, err := f.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("clean NetworkCreate failure recorded %d networks, want 0", len(out))
	}
}

func TestFakeNetworkCreateErrLeaks(t *testing.T) {
	f := NewFakeClient()
	f.NetworkCreateErr = errors.New("ambiguous daemon")
	f.NetworkCreateErrLeaks = true

	resp, err := f.NetworkCreate(context.Background(), "job-1-net", network.CreateOptions{
		Labels: map[string]string{"garm.docker/create-nonce": "nonce-1"},
	})
	if !errors.Is(err, f.NetworkCreateErr) {
		t.Fatalf("NetworkCreate err = %v, want the injected error", err)
	}
	if resp.ID == "" {
		t.Fatal("ambiguous NetworkCreate should still return the leaked network ID")
	}
	// The leaked network IS recorded, so a cleanup path can find and remove it.
	out, err := f.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("ambiguous NetworkCreate recorded %d networks, want 1 (leaked)", len(out))
	}
}

func TestFakeNetworkRemoveErr(t *testing.T) {
	f := NewFakeClient()
	if _, err := f.NetworkCreate(context.Background(), "job-1-net", network.CreateOptions{}); err != nil {
		t.Fatalf("NetworkCreate returned unexpected error: %v", err)
	}
	f.NetworkRemoveErr = errors.New("remove boom")
	if err := f.NetworkRemove(context.Background(), "job-1-net"); !errors.Is(err, f.NetworkRemoveErr) {
		t.Errorf("NetworkRemove err = %v, want the injected error", err)
	}
}

func TestFakeVolumeCreateErr(t *testing.T) {
	f := NewFakeClient()
	f.VolumeCreateErr = errors.New("no space")

	_, err := f.VolumeCreate(context.Background(), volume.CreateOptions{Name: "job-1-workspace"})
	if !errors.Is(err, f.VolumeCreateErr) {
		t.Fatalf("VolumeCreate err = %v, want the injected error", err)
	}
	out, err := f.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	if len(out.Volumes) != 0 {
		t.Errorf("failed VolumeCreate recorded %d volumes, want 0", len(out.Volumes))
	}
}

func TestFakeVolumeRemoveErr(t *testing.T) {
	f := NewFakeClient()
	if _, err := f.VolumeCreate(context.Background(), volume.CreateOptions{Name: "job-1-workspace"}); err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}
	f.VolumeRemoveErr = errors.New("remove boom")
	if err := f.VolumeRemove(context.Background(), "job-1-workspace", true); !errors.Is(err, f.VolumeRemoveErr) {
		t.Errorf("VolumeRemove err = %v, want the injected error", err)
	}
}
