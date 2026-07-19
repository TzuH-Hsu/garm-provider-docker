package provider

import (
	"context"
	"errors"
	"testing"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// seedRunner creates a managed runner container in the fake with the given
// instance name, pool, controller, and Docker state, and returns its ID.
func seedRunner(t *testing.T, fake *docker.FakeClient, instanceName, poolID, controllerID, state string) string {
	t.Helper()
	labels := map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: controllerID,
		spec.LabelInstanceName: instanceName,
		spec.LabelPoolID:       poolID,
		spec.LabelRole:         spec.RoleRunner,
		spec.LabelOSType:       "linux",
		spec.LabelOSArch:       "amd64",
	}
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels}, nil, nil, nil, spec.RunnerContainerName(instanceName))
	if err != nil {
		t.Fatalf("seed ContainerCreate returned unexpected error: %v", err)
	}
	fake.SetState(resp.ID, state, false)
	return resp.ID
}

func TestMapContainerStatus(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		oomKilled bool
		dead      bool
		want      params.InstanceStatus
	}{
		{"running", "running", false, false, params.InstanceRunning},
		{"exited", "exited", false, false, params.InstanceStopped},
		{"created", "created", false, false, params.InstancePendingCreate},
		{"dead", "dead", false, true, params.InstanceError},
		{"oom killed while exited", "exited", true, false, params.InstanceError},
		{"paused is unknown", "paused", false, false, params.InstanceStatusUnknown},
		{"restarting is unknown", "restarting", false, false, params.InstanceStatusUnknown},
		{"empty is unknown", "", false, false, params.InstanceStatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapContainerStatus(tt.state, tt.oomKilled, tt.dead); got != tt.want {
				t.Errorf("mapContainerStatus(%q, %v, %v) = %q, want %q", tt.state, tt.oomKilled, tt.dead, got, tt.want)
			}
		})
	}
}

func TestResolveByID(t *testing.T) {
	p, fake := newTestProvider(t)
	id := seedRunner(t, fake, "runner-a", "p1", "controller-abc", "running")

	c, found, err := p.resolve(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve returned unexpected error: %v", err)
	}
	if !found || c.ID != id {
		t.Errorf("resolve(id) = %q found=%v, want %q true", c.ID, found, id)
	}
}

func TestResolveByName(t *testing.T) {
	p, fake := newTestProvider(t)
	// The fake's ContainerInspect matches by ID only, so passing the GARM
	// instance Name exercises the label-filter fallback path.
	id := seedRunner(t, fake, "Runner-B", "p1", "controller-abc", "running")

	c, found, err := p.resolve(context.Background(), "Runner-B")
	if err != nil {
		t.Fatalf("resolve returned unexpected error: %v", err)
	}
	if !found || c.ID != id {
		t.Errorf("resolve(name) = %q found=%v, want %q true", c.ID, found, id)
	}
}

func TestResolveNotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, found, err := p.resolve(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("resolve returned unexpected error: %v", err)
	}
	if found {
		t.Error("resolve found a container that does not exist")
	}
}

// assertForeignUntouched asserts that every lifecycle method treats ref as
// not-found and that the container foreignID is never removed.
func assertForeignUntouched(t *testing.T, p *Provider, fake *docker.FakeClient, ref, foreignID string) {
	t.Helper()
	if _, err := p.GetInstance(context.Background(), ref); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("GetInstance(%q) err = %v, want not-found (foreign container)", ref, err)
	}
	if err := p.Stop(context.Background(), ref, false); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("Stop(%q) err = %v, want not-found (foreign container)", ref, err)
	}
	if err := p.Start(context.Background(), ref); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("Start(%q) err = %v, want not-found (foreign container)", ref, err)
	}
	if err := p.DeleteInstance(context.Background(), ref); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("DeleteInstance(%q) err = %v, want not-found (foreign container)", ref, err)
	}
	if _, err := fake.ContainerInspect(context.Background(), foreignID); err != nil {
		t.Errorf("foreign container %s was touched/removed: %v", foreignID, err)
	}
}

func TestResolveRejectsForeignController(t *testing.T) {
	p, fake := newTestProvider(t)
	// A managed runner owned by a DIFFERENT controller, whose Docker name and
	// container ID both collide with what a lookup might resolve (ADR-004 F3).
	foreignID := seedRunner(t, fake, "runner-foreign", "p1", "different-controller", "running")

	// By GARM Name and by container ID: both must be not-found and untouched.
	assertForeignUntouched(t, p, fake, "runner-foreign", foreignID)
	assertForeignUntouched(t, p, fake, foreignID, foreignID)
}

func TestResolveRejectsWrongRole(t *testing.T) {
	p, fake := newTestProvider(t)
	// A container this controller owns, but with a non-runner role (e.g. a
	// future DinD sidecar): it must not be resolvable as a runner instance.
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{
		Labels: map[string]string{
			spec.LabelManaged:      "true",
			spec.LabelControllerID: "controller-abc",
			spec.LabelInstanceName: "sidecar-1",
			spec.LabelRole:         spec.RoleDind,
		},
	}, nil, nil, nil, spec.RunnerContainerName("sidecar-1"))
	if err != nil {
		t.Fatalf("seed sidecar ContainerCreate returned unexpected error: %v", err)
	}
	fake.SetState(resp.ID, "running", false)

	// By ID and by name.
	assertForeignUntouched(t, p, fake, resp.ID, resp.ID)
	assertForeignUntouched(t, p, fake, "sidecar-1", resp.ID)
}

func TestResolveRejectsUnmanaged(t *testing.T) {
	p, fake := newTestProvider(t)
	// A completely unmanaged container whose Docker name collides.
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{
		Labels: map[string]string{"some.other/label": "true"},
	}, nil, nil, nil, "runner-x")
	if err != nil {
		t.Fatalf("seed unmanaged ContainerCreate returned unexpected error: %v", err)
	}
	fake.SetState(resp.ID, "running", false)

	assertForeignUntouched(t, p, fake, "runner-x", resp.ID)
	assertForeignUntouched(t, p, fake, resp.ID, resp.ID)
}

func TestGetInstanceStatusMapping(t *testing.T) {
	tests := []struct {
		state     string
		oomKilled bool
		want      params.InstanceStatus
	}{
		{"running", false, params.InstanceRunning},
		{"exited", false, params.InstanceStopped},
		{"created", false, params.InstancePendingCreate},
		{"dead", false, params.InstanceError},
		{"exited", true, params.InstanceError}, // OOM-killed
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			p, fake := newTestProvider(t)
			id := seedRunner(t, fake, "runner-x", "p1", "controller-abc", tt.state)
			fake.SetState(id, tt.state, tt.oomKilled)

			inst, err := p.GetInstance(context.Background(), id)
			if err != nil {
				t.Fatalf("GetInstance returned unexpected error: %v", err)
			}
			if inst.Status != tt.want {
				t.Errorf("Status = %q, want %q", inst.Status, tt.want)
			}
			if inst.ProviderID != id || inst.Name != "runner-x" {
				t.Errorf("ProviderID/Name = %q/%q, want %q/runner-x", inst.ProviderID, inst.Name, id)
			}
			if inst.OSType != params.Linux || inst.OSArch != params.Amd64 {
				t.Errorf("os fields = %q/%q, want linux/amd64", inst.OSType, inst.OSArch)
			}
		})
	}
}

func TestGetInstanceNotFound(t *testing.T) {
	p, _ := newTestProvider(t)
	_, err := p.GetInstance(context.Background(), "ghost")
	if !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("GetInstance(missing) err = %v, want a not-found error", err)
	}
}

func TestDeleteInstanceIdempotentAndExit30(t *testing.T) {
	p, fake := newTestProvider(t)
	seedRunner(t, fake, "runner-del", "p1", "controller-abc", "running")

	// First delete removes the container.
	if err := p.DeleteInstance(context.Background(), "runner-del"); err != nil {
		t.Fatalf("first DeleteInstance returned unexpected error: %v", err)
	}
	if n := listAll(t, p); n != 0 {
		t.Errorf("after delete, %d containers remain, want 0", n)
	}

	// Second delete (already gone) is idempotent -> not-found -> exit 30.
	err := p.DeleteInstance(context.Background(), "runner-del")
	if !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("second DeleteInstance err = %v, want a not-found error", err)
	}
	if code := execcommon.ResolveErrorToExitCode(err); code != execcommon.ExitCodeNotFound {
		t.Errorf("exit code = %d, want %d (not found)", code, execcommon.ExitCodeNotFound)
	}
}

func TestDeleteInstanceByContainerID(t *testing.T) {
	p, fake := newTestProvider(t)
	id := seedRunner(t, fake, "runner-byid", "p1", "controller-abc", "running")

	if err := p.DeleteInstance(context.Background(), id); err != nil {
		t.Fatalf("DeleteInstance(id) returned unexpected error: %v", err)
	}
	if n := listAll(t, p); n != 0 {
		t.Errorf("after delete by id, %d containers remain, want 0", n)
	}
}

func TestListInstancesFiltering(t *testing.T) {
	p, fake := newTestProvider(t)
	seedRunner(t, fake, "a1", "p1", "controller-abc", "running")
	seedRunner(t, fake, "a2", "p1", "controller-abc", "exited")
	seedRunner(t, fake, "b1", "p2", "controller-abc", "running")
	// Foreign controller: must never appear.
	seedRunner(t, fake, "other", "p1", "different-controller", "running")

	// No pool filter: all three for this controller (not the foreign one).
	all, err := p.ListInstances(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInstances returned unexpected error: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListInstances(\"\") = %d, want 3 (excludes foreign controller)", len(all))
	}
	for _, inst := range all {
		if inst.Name == "other" {
			t.Error("ListInstances leaked a foreign-controller instance")
		}
	}

	// Pool filter: only p1.
	p1, err := p.ListInstances(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListInstances(p1) returned unexpected error: %v", err)
	}
	if len(p1) != 2 {
		t.Errorf("ListInstances(p1) = %d, want 2", len(p1))
	}
}

func TestListInstancesStatusFromSummary(t *testing.T) {
	p, fake := newTestProvider(t)
	seedRunner(t, fake, "run", "p1", "controller-abc", "running")
	seedRunner(t, fake, "stop", "p1", "controller-abc", "exited")

	list, err := p.ListInstances(context.Background(), "p1")
	if err != nil {
		t.Fatalf("ListInstances returned unexpected error: %v", err)
	}
	byName := map[string]params.InstanceStatus{}
	for _, inst := range list {
		byName[inst.Name] = inst.Status
	}
	if byName["run"] != params.InstanceRunning {
		t.Errorf("run status = %q, want running", byName["run"])
	}
	if byName["stop"] != params.InstanceStopped {
		t.Errorf("stop status = %q, want stopped", byName["stop"])
	}
}

func TestStopAndStartViaResolver(t *testing.T) {
	p, fake := newTestProvider(t)
	id := seedRunner(t, fake, "runner-ss", "p1", "controller-abc", "running")

	// Stop by name.
	if err := p.Stop(context.Background(), "runner-ss", true); err != nil {
		t.Fatalf("Stop returned unexpected error: %v", err)
	}
	got, _ := fake.ContainerInspect(context.Background(), id)
	if got.State.Running {
		t.Error("container still running after Stop")
	}

	// Start by name.
	if err := p.Start(context.Background(), "runner-ss"); err != nil {
		t.Fatalf("Start returned unexpected error: %v", err)
	}
	got, _ = fake.ContainerInspect(context.Background(), id)
	if !got.State.Running {
		t.Error("container not running after Start")
	}

	// Stop/Start on a missing instance -> not found.
	if err := p.Stop(context.Background(), "ghost", false); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("Stop(missing) err = %v, want not-found", err)
	}
	if err := p.Start(context.Background(), "ghost"); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("Start(missing) err = %v, want not-found", err)
	}
}

func TestResolveInspectErrorIsPropagated(t *testing.T) {
	p, fake := newTestProvider(t)
	fake.InspectErr = errors.New("docker daemon exploded")

	// A non-NotFound inspect error must surface as an error, not be treated
	// as "gone" (which would be a not-found error / exit 30).
	_, gotErr := p.GetInstance(context.Background(), "anything")
	if gotErr == nil {
		t.Fatal("expected the inspect error to surface, got nil")
	}
	if errors.Is(gotErr, gErrors.ErrNotFound) {
		t.Error("a daemon error must not be mistaken for not-found")
	}
}

func TestDeleteInstanceRemoveErrorSurfaces(t *testing.T) {
	p, fake := newTestProvider(t)
	seedRunner(t, fake, "runner-rmfail", "p1", "controller-abc", "running")
	fake.RemoveErr = errors.New("remove failed")

	if err := p.DeleteInstance(context.Background(), "runner-rmfail"); err == nil {
		t.Fatal("expected DeleteInstance to surface the removal error, got nil")
	}
}

func TestRemoveAllInstancesToleratesRemoveError(t *testing.T) {
	p, fake := newTestProvider(t)
	seedRunner(t, fake, "job-a", "p1", "controller-abc", "running")
	fake.RemoveErr = errors.New("remove failed")

	// Best-effort: a removal failure is logged, not returned.
	if err := p.RemoveAllInstances(context.Background()); err != nil {
		t.Errorf("RemoveAllInstances should not fail fast on a removal error, got %v", err)
	}
	if n := listAll(t, p); n != 1 {
		t.Errorf("container count = %d, want 1 (removal failed so it stays)", n)
	}
}

func TestAddressesFromInspect(t *testing.T) {
	c := types.ContainerJSON{
		NetworkSettings: &types.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {IPAddress: "172.17.0.2"},
				"empty":  {IPAddress: ""},
				"nilep":  nil,
			},
		},
	}
	addrs := addressesFromInspect(c)
	if len(addrs) != 1 {
		t.Fatalf("addresses = %v, want exactly one (the non-empty IP)", addrs)
	}
	if addrs[0].Address != "172.17.0.2" || addrs[0].Type != params.PrivateAddress {
		t.Errorf("address = %+v, want 172.17.0.2/private", addrs[0])
	}

	// No network settings -> no addresses.
	if got := addressesFromInspect(types.ContainerJSON{}); got != nil {
		t.Errorf("addresses = %v, want nil for a container with no network settings", got)
	}
}

func TestRemoveAllInstancesRemovesJobScopedOnly(t *testing.T) {
	p, fake := newTestProvider(t)
	seedRunner(t, fake, "job1", "p1", "controller-abc", "running")
	seedRunner(t, fake, "job2", "p2", "controller-abc", "exited")
	// Foreign controller instance: out of scope, must survive.
	foreignCtrlID := seedRunner(t, fake, "foreign-ctrl", "p1", "different-controller", "running")

	// A completely unmanaged container (no garm.docker/* labels): must survive.
	unmanaged, err := fake.ContainerCreate(context.Background(), &container.Config{
		Labels: map[string]string{"some.other/label": "true"},
	}, nil, nil, nil, "unmanaged")
	if err != nil {
		t.Fatalf("seed unmanaged container: %v", err)
	}

	// A managed container WITHOUT an instance-name label (not job-scoped):
	// the ADR-004 predicate must exclude it.
	noInstance, err := fake.ContainerCreate(context.Background(), &container.Config{
		Labels: map[string]string{
			spec.LabelManaged:      "true",
			spec.LabelControllerID: "controller-abc",
		},
	}, nil, nil, nil, "no-instance-name")
	if err != nil {
		t.Fatalf("seed no-instance-name container: %v", err)
	}

	if err := p.RemoveAllInstances(context.Background()); err != nil {
		t.Fatalf("RemoveAllInstances returned unexpected error: %v", err)
	}

	// job1 and job2 are gone; the three out-of-scope containers survive.
	if _, err := fake.ContainerInspect(context.Background(), foreignCtrlID); err != nil {
		t.Errorf("foreign-controller instance was removed: %v", err)
	}
	if _, err := fake.ContainerInspect(context.Background(), unmanaged.ID); err != nil {
		t.Errorf("unmanaged container was removed: %v", err)
	}
	if _, err := fake.ContainerInspect(context.Background(), noInstance.ID); err != nil {
		t.Errorf("managed-but-not-job-scoped container was removed: %v", err)
	}

	remaining := listAll(t, p)
	if remaining != 3 {
		t.Errorf("after RemoveAllInstances, %d containers remain, want 3 (the out-of-scope ones)", remaining)
	}
}
