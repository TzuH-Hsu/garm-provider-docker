package spec

import (
	"testing"
	"time"

	"github.com/docker/docker/api/types/filters"
)

var testCreatedAt = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func TestAllocationIdentityContainerLabels(t *testing.T) {
	id := AllocationIdentity{
		ControllerID: "controller-1",
		PoolID:       "pool-1",
		InstanceName: "my-instance",
	}

	tests := []struct {
		name string
		role string
	}{
		{name: "runner role", role: RoleRunner},
		{name: "dind role", role: RoleDind},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := id.ContainerLabels(tt.role, testCreatedAt)

			want := map[string]string{
				LabelManaged:      "true",
				LabelControllerID: "controller-1",
				LabelPoolID:       "pool-1",
				LabelInstanceName: "my-instance",
				LabelCreatedAt:    "2026-07-19T12:00:00Z",
				LabelRole:         tt.role,
			}
			for k, v := range want {
				if labels[k] != v {
					t.Errorf("labels[%q] = %q, want %q", k, labels[k], v)
				}
			}
			if _, ok := labels[LabelResource]; ok {
				t.Errorf("container labels must not carry %q", LabelResource)
			}
			if len(labels) != len(want) {
				t.Errorf("labels = %v, want exactly %v", labels, want)
			}
		})
	}
}

func TestAllocationIdentityResourceLabels(t *testing.T) {
	id := AllocationIdentity{
		ControllerID: "controller-1",
		PoolID:       "pool-1",
		InstanceName: "my-instance",
	}

	resources := []string{ResourceJobNetwork, ResourceWorkspace, ResourceSocket, ResourceDindState}

	for _, resource := range resources {
		t.Run(resource, func(t *testing.T) {
			labels := id.ResourceLabels(resource, testCreatedAt)

			if labels[LabelResource] != resource {
				t.Errorf("labels[%q] = %q, want %q", LabelResource, labels[LabelResource], resource)
			}
			if _, ok := labels[LabelRole]; ok {
				t.Errorf("resource labels must not carry %q", LabelRole)
			}
			if labels[LabelManaged] != "true" {
				t.Errorf("labels[%q] = %q, want \"true\"", LabelManaged, labels[LabelManaged])
			}
			if labels[LabelInstanceName] != "my-instance" {
				t.Errorf("labels[%q] = %q, want %q", LabelInstanceName, labels[LabelInstanceName], "my-instance")
			}
		})
	}
}

func TestMatchesPredicate(t *testing.T) {
	const controllerID = "controller-1"

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "job-scoped resource matches",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: controllerID,
				LabelInstanceName: "my-instance",
			},
			want: true,
		},
		{
			name: "not managed",
			labels: map[string]string{
				LabelControllerID: controllerID,
				LabelInstanceName: "my-instance",
			},
			want: false,
		},
		{
			name: "different controller",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: "other-controller",
				LabelInstanceName: "my-instance",
			},
			want: false,
		},
		{
			name: "no instance-name (e.g. a cache volume)",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: controllerID,
				LabelCache:        "true",
			},
			want: false,
		},
		{
			name: "cache=true even if instance-name were somehow present",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: controllerID,
				LabelInstanceName: "my-instance",
				LabelCache:        "true",
			},
			want: false,
		},
		{
			name:   "empty labels",
			labels: map[string]string{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchesPredicate(tt.labels, controllerID); got != tt.want {
				t.Errorf("MatchesPredicate(%v, %q) = %v, want %v", tt.labels, controllerID, got, tt.want)
			}
		})
	}
}

func TestMatchPredicateFiltersAgreesWithMatchesPredicate(t *testing.T) {
	// The Docker-expressible part of MatchPredicateFilters must accept
	// exactly the labels sets that MatchesPredicate's first three
	// conjuncts (managed, controller-id, has-instance-name) accept; the
	// fourth conjunct (NOT cache=true) is deliberately not encoded in the
	// filter (see the doc comment on MatchPredicateFilters), so it is
	// exercised separately in TestMatchesPredicate above.
	const controllerID = "controller-1"
	f := MatchPredicateFilters(controllerID)

	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "job-scoped resource matches",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: controllerID,
				LabelInstanceName: "my-instance",
			},
			want: true,
		},
		{
			name: "not managed",
			labels: map[string]string{
				LabelControllerID: controllerID,
				LabelInstanceName: "my-instance",
			},
			want: false,
		},
		{
			name: "different controller",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: "other-controller",
				LabelInstanceName: "my-instance",
			},
			want: false,
		},
		{
			name: "no instance-name",
			labels: map[string]string{
				LabelManaged:      "true",
				LabelControllerID: controllerID,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := f.MatchKVList("label", tt.labels); got != tt.want {
				t.Errorf("filters.Args.MatchKVList(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestResourceLabelBuilders(t *testing.T) {
	id := AllocationIdentity{
		ControllerID: "controller-1",
		PoolID:       "pool-1",
		InstanceName: "my-instance",
	}

	tests := []struct {
		name         string
		fn           func(time.Time) map[string]string
		wantResource string
	}{
		{name: "NetworkLabels", fn: id.NetworkLabels, wantResource: ResourceJobNetwork},
		{name: "WorkspaceVolumeLabels", fn: id.WorkspaceVolumeLabels, wantResource: ResourceWorkspace},
		{name: "SocketVolumeLabels", fn: id.SocketVolumeLabels, wantResource: ResourceSocket},
		{name: "DindStateVolumeLabels", fn: id.DindStateVolumeLabels, wantResource: ResourceDindState},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := tt.fn(testCreatedAt)
			if labels[LabelResource] != tt.wantResource {
				t.Errorf("labels[%q] = %q, want %q", LabelResource, labels[LabelResource], tt.wantResource)
			}
			if labels[LabelInstanceName] != "my-instance" {
				t.Errorf("labels[%q] = %q, want %q", LabelInstanceName, labels[LabelInstanceName], "my-instance")
			}
			if labels[LabelCreatedAt] != "2026-07-19T12:00:00Z" {
				t.Errorf("labels[%q] = %q, want RFC3339 %q", LabelCreatedAt, labels[LabelCreatedAt], "2026-07-19T12:00:00Z")
			}
			if labels[LabelManaged] != "true" {
				t.Errorf("labels[%q] = %q, want \"true\"", LabelManaged, labels[LabelManaged])
			}
			if _, ok := labels[LabelRole]; ok {
				t.Errorf("%s must not carry %q (containers only)", tt.name, LabelRole)
			}
			// Every builder must agree with the general-purpose
			// ResourceLabels it wraps, not just superficially resemble it.
			want := id.ResourceLabels(tt.wantResource, testCreatedAt)
			if len(labels) != len(want) {
				t.Errorf("%s = %v, want exactly %v", tt.name, labels, want)
			}
			for k, v := range want {
				if labels[k] != v {
					t.Errorf("%s[%q] = %q, want %q", tt.name, k, labels[k], v)
				}
			}
		})
	}
}

func TestNetworkLabelsIsTheClaimMarker(t *testing.T) {
	// ADR-004: the job network doubles as the claim marker, so its labels
	// must carry both instance-name and created-at — the two fields the
	// concurrency-safe orphan sweep and duplicate-detection logic (WP2/WP3)
	// key off.
	id := AllocationIdentity{ControllerID: "c", PoolID: "p", InstanceName: "claim-me"}
	labels := id.NetworkLabels(testCreatedAt)

	if _, ok := labels[LabelInstanceName]; !ok {
		t.Error("NetworkLabels missing garm.docker/instance-name: cannot serve as ADR-004's claim marker without it")
	}
	if _, ok := labels[LabelCreatedAt]; !ok {
		t.Error("NetworkLabels missing garm.docker/created-at: cannot serve as ADR-004's claim marker without it")
	}
}

func TestDindContainerLabels(t *testing.T) {
	id := AllocationIdentity{
		ControllerID: "controller-1",
		PoolID:       "pool-1",
		InstanceName: "my-instance",
	}

	labels := id.DindContainerLabels(testCreatedAt)

	if labels[LabelRole] != RoleDind {
		t.Errorf("labels[%q] = %q, want %q", LabelRole, labels[LabelRole], RoleDind)
	}
	if _, ok := labels[LabelResource]; ok {
		t.Error("DindContainerLabels must not carry garm.docker/resource (containers only carry role)")
	}
	// Must agree with the general-purpose ContainerLabels it wraps.
	want := id.ContainerLabels(RoleDind, testCreatedAt)
	if len(labels) != len(want) {
		t.Fatalf("DindContainerLabels = %v, want exactly %v", labels, want)
	}
	for k, v := range want {
		if labels[k] != v {
			t.Errorf("DindContainerLabels[%q] = %q, want %q", k, labels[k], v)
		}
	}
}

func TestMatchPredicateFiltersUsesLabelKey(t *testing.T) {
	// Sanity-check that MatchPredicateFilters builds "label" filters (the
	// only filter key Docker's ContainerList/NetworkList/VolumeList
	// actually evaluate label predicates under), not some other key.
	f := MatchPredicateFilters("controller-1")
	empty := filters.NewArgs()
	if f.Len() == empty.Len() {
		t.Fatal("MatchPredicateFilters returned an empty filter set")
	}
	values := f.Get("label")
	if len(values) != 3 {
		t.Fatalf("filters.Args.Get(\"label\") = %v, want 3 entries", values)
	}
}
