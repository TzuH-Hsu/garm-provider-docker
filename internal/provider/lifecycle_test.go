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

// seedDindSidecar creates a managed DinD sidecar container (role=dind) for the
// given instance name, so tests can exercise the runner-vs-sidecar resolution
// (F6/N1) with both present under the same instance-name label.
func seedDindSidecar(t *testing.T, fake *docker.FakeClient, instanceName, poolID, controllerID, state string) string {
	t.Helper()
	labels := map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: controllerID,
		spec.LabelInstanceName: instanceName,
		spec.LabelPoolID:       poolID,
		spec.LabelRole:         spec.RoleDind,
	}
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels}, nil, nil, nil, spec.DindContainerName(instanceName))
	if err != nil {
		t.Fatalf("seed sidecar ContainerCreate returned unexpected error: %v", err)
	}
	fake.SetState(resp.ID, state, false)
	return resp.ID
}

// TestResolveSelectsRunnerNotSidecar is the F6/N1 guard: with a DinD sidecar
// present under the same instance name, a by-name Get resolves the RUNNER
// (never the sidecar, whatever order the daemon lists them in), and Delete
// reaps the whole allocation — runner AND the lingering privileged sidecar.
func TestResolveSelectsRunnerNotSidecar(t *testing.T) {
	p, fake := newTestProvider(t)
	// Sidecar exited, runner running: if resolution picked the sidecar, the
	// reported status would be Stopped instead of Running.
	sidecarID := seedDindSidecar(t, fake, "job-1", "p1", "controller-abc", "exited")
	runnerID := seedRunner(t, fake, "job-1", "p1", "controller-abc", "running")

	inst, err := p.GetInstance(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetInstance(job-1) returned unexpected error: %v", err)
	}
	if inst.Status != params.InstanceRunning {
		t.Errorf("[F6/N1] GetInstance resolved status=%q, want running — it selected the DinD sidecar, not the runner", inst.Status)
	}
	if inst.ProviderID != "job-1" {
		t.Errorf("ProviderID=%q, want the instance name job-1", inst.ProviderID)
	}

	// Delete reaps the whole allocation: runner AND the lingering sidecar.
	if err := p.DeleteInstance(context.Background(), "job-1"); err != nil {
		t.Fatalf("DeleteInstance(job-1) returned unexpected error: %v", err)
	}
	if _, err := fake.ContainerInspect(context.Background(), runnerID); err == nil {
		t.Error("runner survived DeleteInstance")
	}
	if _, err := fake.ContainerInspect(context.Background(), sidecarID); err == nil {
		t.Error("[F6] DinD sidecar leaked past DeleteInstance")
	}
}

// TestResolveByInstanceNameCollidingWithAnotherRunnerID is the N2 guard: an
// instance name that equals ANOTHER runner's container ID must resolve to the
// runner that OWNS that instance name, not the id-colliding one — the exact
// mis-resolution an inspect-by-ID-first resolver would produce.
func TestResolveByInstanceNameCollidingWithAnotherRunnerID(t *testing.T) {
	p, fake := newTestProvider(t)
	// Runner X owns instance "alpha"; it is EXITED.
	xID := seedRunner(t, fake, "alpha", "p1", "controller-abc", "exited")
	// Runner Y's instance NAME is exactly X's container ID; it is RUNNING.
	yID := seedRunner(t, fake, xID, "p1", "controller-abc", "running")

	// Resolving by that colliding id/name must select Y (owner of the label),
	// not X (whose container ID it happens to equal).
	inst, err := p.GetInstance(context.Background(), xID)
	if err != nil {
		t.Fatalf("GetInstance(%q) returned unexpected error: %v", xID, err)
	}
	if inst.ProviderID != xID {
		t.Errorf("[N2] GetInstance(%q) resolved ProviderID=%q, want %q (runner Y owns that instance-name)", xID, inst.ProviderID, xID)
	}
	if inst.Status != params.InstanceRunning {
		t.Errorf("[N2] resolved status=%q, want running — it mis-resolved to runner X via an id match", inst.Status)
	}

	// Delete by the colliding id/name removes Y (the label owner); X survives.
	if err := p.DeleteInstance(context.Background(), xID); err != nil {
		t.Fatalf("DeleteInstance(%q) returned unexpected error: %v", xID, err)
	}
	if _, err := fake.ContainerInspect(context.Background(), yID); err == nil {
		t.Error("[N2] runner Y (the instance-name owner) survived delete")
	}
	if _, err := fake.ContainerInspect(context.Background(), xID); err != nil {
		t.Errorf("[N2] runner X (id-colliding, different instance) was wrongly deleted: %v", err)
	}
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

// TestResolveRawContainerIDNoLongerResolves is the NEW-H1 regression guard: the
// raw inspect-by-ID fallback was DROPPED (2026-07-21). A GARM_INSTANCE_ID that
// is a bare container ID (never a legitimate value — provider_id has always been
// the instance name in this pre-release provider) must NOT resolve the runner by
// that id; only the instance-name label resolves. This closes the residual
// ambiguity where a foreign/other-generation container could be reached by a raw
// id inspect.
func TestResolveRawContainerIDNoLongerResolves(t *testing.T) {
	p, fake := newTestProvider(t)
	id := seedRunner(t, fake, "runner-a", "p1", "controller-abc", "running")

	// The runner's own instance-name label is "runner-a", NOT its container id.
	_, found, err := p.resolve(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve returned unexpected error: %v", err)
	}
	if found {
		t.Errorf("resolve(rawContainerID=%q) found a container; the raw inspect-by-ID fallback must be gone (resolution is instance-name-label only)", id)
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
	assertGetStopStartNotFound(t, p, ref)
	if err := p.DeleteInstance(context.Background(), ref); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("DeleteInstance(%q) err = %v, want not-found (foreign container)", ref, err)
	}
	if _, err := fake.ContainerInspect(context.Background(), foreignID); err != nil {
		t.Errorf("foreign container %s was touched/removed: %v", foreignID, err)
	}
}

// assertGetStopStartNotFound asserts the three RUNNER-scoped lifecycle methods
// (which resolve the runner container) treat ref as not-found — used both for
// genuinely foreign containers and for our own wrong-role (DinD sidecar)
// containers, which are not runners and so must never be operated on as one.
func assertGetStopStartNotFound(t *testing.T, p *Provider, ref string) {
	t.Helper()
	if _, err := p.GetInstance(context.Background(), ref); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("GetInstance(%q) err = %v, want not-found", ref, err)
	}
	if err := p.Stop(context.Background(), ref, false); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("Stop(%q) err = %v, want not-found", ref, err)
	}
	if err := p.Start(context.Background(), ref); !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("Start(%q) err = %v, want not-found", ref, err)
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
	// A container this controller owns, but with a non-runner role (a DinD
	// sidecar). It must not be resolvable as a RUNNER — Get/Stop/Start operate on
	// the runner, so they treat it as not-found and never touch it.
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

	// By the sidecar's OWN container ID, nothing resolves (its ID is not the
	// instance-name label), so every op is not-found and it is untouched.
	assertForeignUntouched(t, p, fake, resp.ID, resp.ID)

	// By the allocation's instance NAME, the runner-scoped ops still reject the
	// wrong-role container...
	assertGetStopStartNotFound(t, p, "sidecar-1")
	// ...but DeleteInstance MUST reap the lingering sidecar (F6): a managed DinD
	// sidecar under this instance-name is part of an allocation whose runner is
	// gone, and the privileged sidecar must never be left behind.
	if err := p.DeleteInstance(context.Background(), "sidecar-1"); err != nil {
		t.Errorf("DeleteInstance(sidecar-1) should reap the lingering managed sidecar (F6), got %v", err)
	}
	if _, err := fake.ContainerInspect(context.Background(), resp.ID); err == nil {
		t.Error("F6: DeleteInstance must reap the lingering privileged sidecar, but it survived")
	}
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

			// GARM_INSTANCE_ID is the instance NAME (F6), resolved by the
			// instance-name label — never the raw container id (NEW-H1).
			inst, err := p.GetInstance(context.Background(), "runner-x")
			if err != nil {
				t.Fatalf("GetInstance returned unexpected error: %v", err)
			}
			if inst.Status != tt.want {
				t.Errorf("Status = %q, want %q", inst.Status, tt.want)
			}
			// provider_id is the stable instance NAME (F6), not the container id.
			if inst.ProviderID != "runner-x" || inst.Name != "runner-x" {
				t.Errorf("ProviderID/Name = %q/%q, want runner-x/runner-x (F6)", inst.ProviderID, inst.Name)
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

// TestDeleteInstanceByRawContainerIDNoLongerResolves is the NEW-H1 guard for the
// delete path: DeleteInstance with a raw container ID (never a legitimate
// GARM_INSTANCE_ID — provider_id is always the instance name) must NOT resolve
// the runner by that id and must leave the allocation untouched, returning
// not-found (exit 30). The raw inspect-by-ID fallback was dropped, so a stray
// container-id value can no longer trigger a delete of a container it happens to
// id-match. Deleting by the instance NAME still works (covered elsewhere).
func TestDeleteInstanceByRawContainerIDNoLongerResolves(t *testing.T) {
	p, fake := newTestProvider(t)
	id := seedRunner(t, fake, "runner-byid", "p1", "controller-abc", "running")

	err := p.DeleteInstance(context.Background(), id)
	if !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("DeleteInstance(rawContainerID) err = %v, want not-found (raw-ID resolution is dropped)", err)
	}
	// The runner is untouched — a raw container id never resolved it.
	if n := listAll(t, p); n != 1 {
		t.Errorf("after delete-by-raw-id, %d runners remain, want 1 (untouched)", n)
	}
	// And deleting by the instance NAME does reap it.
	if err := p.DeleteInstance(context.Background(), "runner-byid"); err != nil {
		t.Fatalf("DeleteInstance(name) returned unexpected error: %v", err)
	}
	if n := listAll(t, p); n != 0 {
		t.Errorf("after delete-by-name, %d runners remain, want 0", n)
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
	// A matching runner exists, so the label lookup finds a candidate to inspect;
	// the inspect (of that candidate) then fails with a non-NotFound daemon error.
	seedRunner(t, fake, "runner-x", "p1", "controller-abc", "running")
	fake.InspectErr = errors.New("docker daemon exploded")

	// A non-NotFound inspect error must surface as an error, not be treated
	// as "gone" (which would be a not-found error / exit 30).
	_, gotErr := p.GetInstance(context.Background(), "runner-x")
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
