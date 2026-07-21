package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// --- provider-level allocation seed/lookup helpers ---------------------------

func provIdentity(name string) spec.AllocationIdentity {
	return spec.AllocationIdentity{ControllerID: "controller-abc", PoolID: "pool-xyz", InstanceName: name}
}

func seedWorkspaceVol(t *testing.T, fake *docker.FakeClient, name string, createdAt time.Time, nonce string, extra map[string]string) {
	t.Helper()
	labels := provIdentity(name).WorkspaceVolumeLabels(createdAt)
	labels[spec.LabelCreateNonce] = nonce
	for k, v := range extra {
		labels[k] = v
	}
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: spec.WorkspaceVolumeName(name, nonce), Labels: labels}); err != nil {
		t.Fatalf("seed VolumeCreate returned unexpected error: %v", err)
	}
}

func seedNet(t *testing.T, fake *docker.FakeClient, name string, createdAt time.Time, nonce string) {
	t.Helper()
	labels := provIdentity(name).NetworkLabels(createdAt)
	labels[spec.LabelCreateNonce] = nonce
	if _, err := fake.NetworkCreate(context.Background(), spec.JobNetworkName(name), network.CreateOptions{Labels: labels}); err != nil {
		t.Fatalf("seed NetworkCreate returned unexpected error: %v", err)
	}
}

func seedAttachedRunner(t *testing.T, fake *docker.FakeClient, name string, createdAt time.Time, nonce, state string) string {
	t.Helper()
	labels := provIdentity(name).ContainerLabels(spec.RoleRunner, createdAt)
	labels[spec.LabelCreateNonce] = nonce
	labels[spec.LabelOSType] = "linux"
	labels[spec.LabelOSArch] = "amd64"
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels}, &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.JobNetworkName(name)),
	}, nil, nil, spec.RunnerContainerName(name))
	if err != nil {
		t.Fatalf("seed ContainerCreate returned unexpected error: %v", err)
	}
	fake.SetState(resp.ID, state, false)
	// The exited-runner sweep grace (F8) is measured from State.FinishedAt, so a
	// stopped seed runner models finishing when the allocation was created.
	if state == "exited" || state == "dead" {
		fake.SetFinishedAt(resp.ID, createdAt)
	}
	return resp.ID
}

func netByName(t *testing.T, fake *docker.FakeClient, name string) (network.Summary, bool) {
	t.Helper()
	out, err := fake.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	for _, n := range out {
		if n.Name == name {
			return n, true
		}
	}
	return network.Summary{}, false
}

func volByName(t *testing.T, fake *docker.FakeClient, name string) (*volume.Volume, bool) {
	t.Helper()
	out, err := fake.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	for _, v := range out.Volumes {
		if v != nil && v.Name == name {
			return v, true
		}
	}
	return nil, false
}

// volByResource finds a managed volume by its instance-name + resource labels,
// for cases where the volume's generation-unique NAME embeds a create-nonce the
// test does not know up front (F4): after CreateInstance generates a fresh
// nonce, the workspace volume can only be located by label, not by a
// reconstructed name.
func volByResource(t *testing.T, fake *docker.FakeClient, instanceName, resource string) (*volume.Volume, bool) {
	t.Helper()
	out, err := fake.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	for _, v := range out.Volumes {
		if v != nil && v.Labels[spec.LabelInstanceName] == instanceName && v.Labels[spec.LabelResource] == resource {
			return v, true
		}
	}
	return nil, false
}

// --- full none-mode allocation: create → inspect labels/attachment/mount -----

func TestCreateInstanceFullAllocationTopology(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}

	name := "Test-Instance-01"

	// (a) Job network exists with the ADR-004 claim-marker labels.
	n, ok := netByName(t, fake, spec.JobNetworkName(name))
	if !ok {
		t.Fatalf("job network %q was not created", spec.JobNetworkName(name))
	}
	if n.Labels[spec.LabelManaged] != "true" ||
		n.Labels[spec.LabelControllerID] != "controller-abc" ||
		n.Labels[spec.LabelInstanceName] != name ||
		n.Labels[spec.LabelResource] != spec.ResourceJobNetwork ||
		n.Labels[spec.LabelCreatedAt] == "" {
		t.Errorf("job network labels missing/incorrect: %v", n.Labels)
	}
	netNonce := n.Labels[spec.LabelCreateNonce]
	if netNonce == "" {
		t.Error("job network is missing the create-nonce (claim marker)")
	}

	// (b) Workspace volume exists, labeled, sharing the attempt's nonce. Its
	// name is generation-unique (F4): <instance>-<nonce>-workspace.
	v, ok := volByName(t, fake, spec.WorkspaceVolumeName(name, netNonce))
	if !ok {
		t.Fatalf("workspace volume %q was not created", spec.WorkspaceVolumeName(name, netNonce))
	}
	if v.Labels[spec.LabelResource] != spec.ResourceWorkspace ||
		v.Labels[spec.LabelInstanceName] != name {
		t.Errorf("workspace volume labels missing/incorrect: %v", v.Labels)
	}
	if v.Labels[spec.LabelCreateNonce] != netNonce {
		t.Errorf("workspace nonce %q != network nonce %q (claim marker linkage broken)", v.Labels[spec.LabelCreateNonce], netNonce)
	}

	// (c) The runner container is attached to the job network as its sole
	// network and shares the attempt's nonce.
	got := inspectRunner(t, fake, name)
	if got.HostConfig == nil || string(got.HostConfig.NetworkMode) != spec.JobNetworkName(name) {
		t.Errorf("runner NetworkMode = %v, want the job network", got.HostConfig)
	}
	if got.NetworkSettings == nil || got.NetworkSettings.Networks[spec.JobNetworkName(name)] == nil {
		t.Errorf("runner not attached to the job network endpoint: %+v", got.NetworkSettings)
	}
	if got.Config.Labels[spec.LabelCreateNonce] != netNonce {
		t.Errorf("container nonce %q != network nonce %q (claim marker linkage broken)", got.Config.Labels[spec.LabelCreateNonce], netNonce)
	}

	// (d) WP9-flagged JIT-workdir check: the workspace volume is mounted at the
	// runner image's RUNNER_WORKDIR (/actions-runner/_work).
	if len(got.Mounts) != 1 {
		t.Fatalf("runner mounts = %d, want 1 (workspace)", len(got.Mounts))
	}
	m := got.Mounts[0]
	if m.Name != spec.WorkspaceVolumeName(name, netNonce) || m.Destination != spec.RunnerWorkDir {
		t.Errorf("workspace mount = %+v, want %q at %q", m, spec.WorkspaceVolumeName(name, netNonce), spec.RunnerWorkDir)
	}
	if spec.RunnerWorkDir != "/actions-runner/_work" {
		t.Errorf("RunnerWorkDir = %q, want /actions-runner/_work", spec.RunnerWorkDir)
	}
}

// --- creation guard: claim network created, then volume create fails ---------

func TestCreateInstanceVolumeFailureRollsBackClaimNetwork(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.VolumeCreateErr = errors.New("no space left on device")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on workspace volume creation, got nil")
	}
	// The claim network was created before the volume step; the guard must roll
	// it back. No image pull or credential delivery happens before the volume.
	assertNoLeftovers(t, fake)
	if len(fake.PulledImages) != 0 {
		t.Errorf("a pre-image failure must not pull, got %v", fake.PulledImages)
	}
	if len(fake.Execs) != 0 {
		t.Errorf("a pre-container failure must not deliver credentials, got %d execs", len(fake.Execs))
	}
}

// --- stale-volume sweep before create (no cross-job residue) ------------------

func TestCreateInstanceSweepsStaleAllocationBeforeReuse(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	name := "Test-Instance-01"
	old := time.Now().Add(-time.Hour)

	// A crashed prior allocation for the SAME instance name, past the grace
	// window: a stale network, an exited runner, and a workspace volume carrying
	// a residue marker. Without the pre-create sweep, the idempotent VolumeCreate
	// would silently reuse the stale volume (cross-job residue), and the stale
	// network would 409 the claim.
	seedNet(t, fake, name, old, "stale-nonce")
	seedAttachedRunner(t, fake, name, old, "stale-nonce", "exited")
	seedWorkspaceVol(t, fake, name, old, "stale-nonce", map[string]string{"test.residue": "prior-job"})

	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error (stale sweep should have cleared the way): %v", err)
	}

	// Exactly one of each resource, all fresh. The fresh volume's name embeds a
	// new create-nonce the test does not know, so locate it by label (F4).
	v, ok := volByResource(t, fake, name, spec.ResourceWorkspace)
	if !ok {
		t.Fatal("workspace volume missing after create")
	}
	if _, residual := v.Labels["test.residue"]; residual {
		t.Error("stale workspace volume content was reused: the prior-job residue survived")
	}
	if v.Labels[spec.LabelCreateNonce] == "stale-nonce" {
		t.Error("workspace volume still carries the stale nonce; it was not replaced")
	}
	// The fresh runner is the one CreateInstance returned.
	got := inspectRunner(t, fake, inst.Name)
	if got.State == nil || !got.State.Running {
		t.Error("the freshly created runner is not running")
	}
}

// --- ListInstances runs the orphan sweep -------------------------------------

func TestListInstancesSweepsOrphansAndReturnsLiveRunners(t *testing.T) {
	p, fake := newTestProvider(t)
	old := time.Now().Add(-time.Hour)

	// A past-grace exited allocation (should be swept) and a live running one
	// (should remain and be returned).
	seedNet(t, fake, "job-stale", old, "n1")
	seedAttachedRunner(t, fake, "job-stale", old, "n1", "exited")
	seedWorkspaceVol(t, fake, "job-stale", old, "n1", nil)

	seedNet(t, fake, "job-live", time.Now(), "n2")
	seedAttachedRunner(t, fake, "job-live", time.Now(), "n2", "running")
	seedWorkspaceVol(t, fake, "job-live", time.Now(), "n2", nil)

	list, err := p.ListInstances(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInstances returned unexpected error: %v", err)
	}
	if len(list) != 1 || list[0].Name != "job-live" {
		t.Fatalf("ListInstances = %+v, want exactly job-live", list)
	}

	// The stale allocation was fully torn down by the sweep.
	if _, ok := netByName(t, fake, spec.JobNetworkName("job-stale")); ok {
		t.Error("orphan sweep left the stale job network")
	}
	if _, ok := volByName(t, fake, spec.WorkspaceVolumeName("job-stale", "n1")); ok {
		t.Error("orphan sweep left the stale workspace volume")
	}
}

// --- DeleteInstance tears down the full topology in order ---------------------

func TestDeleteInstanceTearsDownFullTopology(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}

	// Delete by GARM instance Name resolves to the instance-name and tears down
	// container → network → volume (the fake's active-endpoint rule proves the
	// container-before-network ordering held).
	if err := p.DeleteInstance(context.Background(), inst.Name); err != nil {
		t.Fatalf("DeleteInstance returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)

	// Idempotent repeat: the whole allocation is gone → exit 30.
	err = p.DeleteInstance(context.Background(), inst.Name)
	if !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("second DeleteInstance err = %v, want not-found", err)
	}
	if code := execcommon.ResolveErrorToExitCode(err); code != execcommon.ExitCodeNotFound {
		t.Errorf("exit code = %d, want %d (not found)", code, execcommon.ExitCodeNotFound)
	}
}

func TestDeleteInstanceReapsLingeringNetworkAndVolumeWhenRunnerGone(t *testing.T) {
	p, fake := newTestProvider(t)
	name := "job-partial"
	// A crashed/partial allocation: network + volume exist, but the runner
	// container is already gone (ADR-004: "runner gone but network or volumes
	// still lingering"). DeleteInstance by name must reap the leftovers.
	seedNet(t, fake, name, time.Now(), "n1")
	seedWorkspaceVol(t, fake, name, time.Now(), "n1", nil)

	if err := p.DeleteInstance(context.Background(), name); err != nil {
		t.Fatalf("DeleteInstance returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)
}

func TestDeleteInstanceReapsLingeringVolumeOnly(t *testing.T) {
	p, fake := newTestProvider(t)
	name := "job-vol-only"
	// Only a workspace volume lingers (network and container already gone):
	// DeleteInstance must still reap it via the volume-side lookup.
	seedWorkspaceVol(t, fake, name, time.Now(), "n1", nil)

	if err := p.DeleteInstance(context.Background(), name); err != nil {
		t.Fatalf("DeleteInstance returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceJobNetworkDisabledStillIsolates confirms the WP2 decision
// that enable_job_network=false is reserved and NOT honored: the isolated
// per-job network is created anyway (it is both the isolation guarantee and the
// ADR-004 claim marker).
func TestCreateInstanceJobNetworkDisabledStillIsolates(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModeNone,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		Network:          config.Network{EnableJobNetwork: false, Internal: true},
	}
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}

	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}
	if _, ok := netByName(t, fake, spec.JobNetworkName("Test-Instance-01")); !ok {
		t.Error("enable_job_network=false must still create the isolated per-job network (WP2 keeps it always on)")
	}
	if inst.Status != "running" {
		t.Errorf("Status = %q, want running", inst.Status)
	}
}

// --- RemoveAllInstances tears down networks + volumes, not just containers ----

func TestRemoveAllInstancesTearsDownAllResourceKinds(t *testing.T) {
	p, fake := newTestProvider(t)
	// Two full allocations for this controller.
	seedNet(t, fake, "job-a", time.Now(), "na")
	seedAttachedRunner(t, fake, "job-a", time.Now(), "na", "running")
	seedWorkspaceVol(t, fake, "job-a", time.Now(), "na", nil)

	seedNet(t, fake, "job-b", time.Now(), "nb")
	seedAttachedRunner(t, fake, "job-b", time.Now(), "nb", "exited")
	seedWorkspaceVol(t, fake, "job-b", time.Now(), "nb", nil)

	if err := p.RemoveAllInstances(context.Background()); err != nil {
		t.Fatalf("RemoveAllInstances returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)
}
