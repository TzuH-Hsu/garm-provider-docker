package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
)

func TestFakeClientNetworkCreateAndList(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	managed, err := f.NetworkCreate(ctx, "my-instance-net", network.CreateOptions{
		Labels: map[string]string{"garm.docker/managed": "true", "garm.docker/instance-name": "my-instance"},
	})
	if err != nil {
		t.Fatalf("NetworkCreate returned unexpected error: %v", err)
	}
	if managed.ID == "" {
		t.Fatal("NetworkCreate returned an empty ID")
	}

	if _, err := f.NetworkCreate(ctx, "other-net", network.CreateOptions{
		Labels: map[string]string{"some.other/label": "true"},
	}); err != nil {
		t.Fatalf("NetworkCreate returned unexpected error: %v", err)
	}

	tests := []struct {
		name    string
		filters filters.Args
		wantLen int
	}{
		{name: "no filter returns everything", filters: filters.NewArgs(), wantLen: 2},
		{
			name:    "managed=true matches only the managed network",
			filters: filters.NewArgs(filters.Arg("label", "garm.docker/managed=true")),
			wantLen: 1,
		},
		{
			name:    "unmatched label returns nothing",
			filters: filters.NewArgs(filters.Arg("label", "garm.docker/managed=false")),
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := f.NetworkList(ctx, network.ListOptions{Filters: tt.filters})
			if err != nil {
				t.Fatalf("NetworkList returned unexpected error: %v", err)
			}
			if len(out) != tt.wantLen {
				t.Fatalf("NetworkList returned %d networks, want %d", len(out), tt.wantLen)
			}
		})
	}

	out, err := f.NetworkList(ctx, network.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", "garm.docker/instance-name=my-instance")),
	})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	if len(out) != 1 || out[0].ID != managed.ID || out[0].Name != "my-instance-net" {
		t.Fatalf("NetworkList = %+v, want exactly the managed network", out)
	}
}

// TestFakeClientNetworkCreateDuplicateNameConflict is the regression guard
// for the fake matching the REAL daemon's network-create behavior (checked
// live against Docker Engine 29.6.1/API 1.55 while building this interface,
// see the doc comment on FakeClient.NetworkCreate): a second create reusing
// an in-use name is rejected with a Conflict, unconditionally.
func TestFakeClientNetworkCreateDuplicateNameConflict(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	if _, err := f.NetworkCreate(ctx, "dup-net", network.CreateOptions{Labels: map[string]string{"a": "1"}}); err != nil {
		t.Fatalf("first NetworkCreate returned unexpected error: %v", err)
	}

	_, err := f.NetworkCreate(ctx, "dup-net", network.CreateOptions{Labels: map[string]string{"a": "2"}})
	if err == nil {
		t.Fatal("second NetworkCreate with a duplicate name succeeded, want a Conflict error")
	}
	if !errdefs.IsConflict(err) {
		t.Errorf("NetworkCreate duplicate-name error = %v, want errdefs.IsConflict", err)
	}

	// The original network must be untouched: still exactly one network,
	// still carrying its ORIGINAL labels.
	out, err := f.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d networks after a rejected duplicate create, want 1", len(out))
	}
	if out[0].Labels["a"] != "1" {
		t.Errorf("Labels[\"a\"] = %q, want the original value %q", out[0].Labels["a"], "1")
	}
}

func TestFakeClientNetworkRemove(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.NetworkCreate(ctx, "removable-net", network.CreateOptions{})
	if err != nil {
		t.Fatalf("NetworkCreate returned unexpected error: %v", err)
	}

	// Removable by name too, mirroring ContainerRemove's ID-or-name lookup.
	if err := f.NetworkRemove(ctx, "removable-net"); err != nil {
		t.Fatalf("NetworkRemove(by name) returned unexpected error: %v", err)
	}

	out, err := f.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("got %d networks after remove, want 0", len(out))
	}

	// Removing an already-gone network (by the ID from the first create) is
	// tolerated by callers via errdefs.IsNotFound (ADR-004 idempotent
	// teardown), so the fake must return that shape, not a generic error.
	err = f.NetworkRemove(ctx, created.ID)
	if err == nil {
		t.Fatal("NetworkRemove of an already-removed network succeeded, want a NotFound error")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("NetworkRemove error = %v, want errdefs.IsNotFound", err)
	}
}

func TestFakeClientNetworkRemoveNotFound(t *testing.T) {
	f := NewFakeClient()

	err := f.NetworkRemove(context.Background(), "no-such-network")
	if err == nil {
		t.Fatal("NetworkRemove of a nonexistent network succeeded, want a NotFound error")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("NetworkRemove error = %v, want errdefs.IsNotFound", err)
	}
}

func TestFakeClientVolumeCreateAndList(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	created, err := f.VolumeCreate(ctx, volume.CreateOptions{
		Name:   "my-instance-workspace",
		Labels: map[string]string{"garm.docker/managed": "true", "garm.docker/resource": "workspace"},
	})
	if err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}
	if created.Name != "my-instance-workspace" {
		t.Errorf("Name = %q, want %q", created.Name, "my-instance-workspace")
	}

	if _, err := f.VolumeCreate(ctx, volume.CreateOptions{
		Name:   "other-volume",
		Labels: map[string]string{"some.other/label": "true"},
	}); err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}

	tests := []struct {
		name    string
		filters filters.Args
		wantLen int
	}{
		{name: "no filter returns everything", filters: filters.NewArgs(), wantLen: 2},
		{
			name:    "resource=workspace matches only the workspace volume",
			filters: filters.NewArgs(filters.Arg("label", "garm.docker/resource=workspace")),
			wantLen: 1,
		},
		{
			name:    "unmatched label returns nothing",
			filters: filters.NewArgs(filters.Arg("label", "garm.docker/resource=socket")),
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := f.VolumeList(ctx, volume.ListOptions{Filters: tt.filters})
			if err != nil {
				t.Fatalf("VolumeList returned unexpected error: %v", err)
			}
			if len(out.Volumes) != tt.wantLen {
				t.Fatalf("VolumeList returned %d volumes, want %d", len(out.Volumes), tt.wantLen)
			}
		})
	}
}

// TestFakeClientVolumeCreateNameCollisionIsIdempotent is the regression
// guard for the fake matching the REAL daemon's volume-create behavior
// (checked live against Docker Engine 29.6.1/API 1.55 while building this
// interface, see the doc comment on FakeClient.VolumeCreate): UNLIKE
// networks and containers, a second create reusing an in-use volume name is
// NOT a conflict — it silently succeeds and returns the ORIGINAL volume,
// discarding the second call's labels. Modeling this as a conflict (the
// naive assumption, and what networks/containers actually do) would hide a
// real divergence from daemon behavior, exactly the class of bug the M0
// tmpfs-uid/gid lesson warns against.
func TestFakeClientVolumeCreateNameCollisionIsIdempotent(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	first, err := f.VolumeCreate(ctx, volume.CreateOptions{Name: "dup-volume", Labels: map[string]string{"a": "1"}})
	if err != nil {
		t.Fatalf("first VolumeCreate returned unexpected error: %v", err)
	}

	second, err := f.VolumeCreate(ctx, volume.CreateOptions{Name: "dup-volume", Labels: map[string]string{"a": "2"}})
	if err != nil {
		t.Fatalf("second VolumeCreate with a duplicate name returned an error, want idempotent success: %v", err)
	}
	if second.Name != first.Name {
		t.Errorf("Name = %q, want %q", second.Name, first.Name)
	}
	// The ORIGINAL labels win; the second call's labels are discarded.
	if second.Labels["a"] != "1" {
		t.Errorf("Labels[\"a\"] = %q after a duplicate create, want the original value %q (real daemon keeps the original)", second.Labels["a"], "1")
	}

	out, err := f.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	if len(out.Volumes) != 1 {
		t.Fatalf("got %d volumes after a duplicate-name create, want exactly 1 (no second volume created)", len(out.Volumes))
	}
}

func TestFakeClientVolumeCreateGeneratesNameWhenOmitted(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	v1, err := f.VolumeCreate(ctx, volume.CreateOptions{})
	if err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}
	v2, err := f.VolumeCreate(ctx, volume.CreateOptions{})
	if err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}
	if v1.Name == "" || v2.Name == "" {
		t.Fatalf("VolumeCreate with no Name left it empty: v1=%q v2=%q", v1.Name, v2.Name)
	}
	if v1.Name == v2.Name {
		t.Fatalf("two anonymous VolumeCreate calls produced the same name %q", v1.Name)
	}
}

func TestFakeClientVolumeRemove(t *testing.T) {
	f := NewFakeClient()
	ctx := context.Background()

	if _, err := f.VolumeCreate(ctx, volume.CreateOptions{Name: "removable-volume"}); err != nil {
		t.Fatalf("VolumeCreate returned unexpected error: %v", err)
	}

	if err := f.VolumeRemove(ctx, "removable-volume", false); err != nil {
		t.Fatalf("VolumeRemove returned unexpected error: %v", err)
	}

	out, err := f.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	if len(out.Volumes) != 0 {
		t.Fatalf("got %d volumes after remove, want 0", len(out.Volumes))
	}

	err = f.VolumeRemove(ctx, "removable-volume", true)
	if err == nil {
		t.Fatal("VolumeRemove of an already-removed volume succeeded, want a NotFound error")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("VolumeRemove error = %v, want errdefs.IsNotFound", err)
	}
}

func TestFakeClientVolumeRemoveNotFound(t *testing.T) {
	f := NewFakeClient()

	err := f.VolumeRemove(context.Background(), "no-such-volume", false)
	if err == nil {
		t.Fatal("VolumeRemove of a nonexistent volume succeeded, want a NotFound error")
	}
	if !errdefs.IsNotFound(err) {
		t.Errorf("VolumeRemove error = %v, want errdefs.IsNotFound", err)
	}
}
