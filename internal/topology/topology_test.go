package topology

import (
	"context"
	"errors"
	"testing"
	"time"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const testControllerID = "controller-abc"

func newManager(t *testing.T) (*Manager, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	return New(fake, testControllerID), fake
}

func identityFor(name string) spec.AllocationIdentity {
	return spec.AllocationIdentity{ControllerID: testControllerID, PoolID: "pool-1", InstanceName: name}
}

// --- seed helpers ------------------------------------------------------------

func seedNetworkFor(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce string) {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: controllerID, PoolID: "pool-1", InstanceName: name}
	labels := id.NetworkLabels(createdAt)
	if nonce != "" {
		labels[spec.LabelCreateNonce] = nonce
	}
	if _, err := fake.NetworkCreate(context.Background(), spec.JobNetworkName(name), network.CreateOptions{Labels: labels}); err != nil {
		t.Fatalf("seed NetworkCreate for %q returned unexpected error: %v", name, err)
	}
}

func seedWorkspaceVolumeFor(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce string) {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: controllerID, PoolID: "pool-1", InstanceName: name}
	labels := id.WorkspaceVolumeLabels(createdAt)
	if nonce != "" {
		labels[spec.LabelCreateNonce] = nonce
	}
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: spec.WorkspaceVolumeName(name), Labels: labels}); err != nil {
		t.Fatalf("seed VolumeCreate for %q returned unexpected error: %v", name, err)
	}
}

func seedRunnerFor(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce, state string) string {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: controllerID, PoolID: "pool-1", InstanceName: name}
	labels := id.ContainerLabels(spec.RoleRunner, createdAt)
	if nonce != "" {
		labels[spec.LabelCreateNonce] = nonce
	}
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels}, &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.JobNetworkName(name)),
	}, nil, nil, spec.RunnerContainerName(name))
	if err != nil {
		t.Fatalf("seed ContainerCreate for %q returned unexpected error: %v", name, err)
	}
	fake.SetState(resp.ID, state, false)
	return resp.ID
}

// seedAllocation seeds a full allocation (network + workspace volume + runner
// attached to the network) for THIS controller.
func seedAllocation(t *testing.T, fake *docker.FakeClient, name string, createdAt time.Time, nonce, runnerState string) {
	t.Helper()
	seedNetworkFor(t, fake, testControllerID, name, createdAt, nonce)
	seedWorkspaceVolumeFor(t, fake, testControllerID, name, createdAt, nonce)
	seedRunnerFor(t, fake, testControllerID, name, createdAt, nonce, runnerState)
}

func seedAllocationForController(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce, runnerState string) {
	t.Helper()
	seedNetworkFor(t, fake, controllerID, name, createdAt, nonce)
	seedWorkspaceVolumeFor(t, fake, controllerID, name, createdAt, nonce)
	seedRunnerFor(t, fake, controllerID, name, createdAt, nonce, runnerState)
}

// seedDindSidecarFor seeds a DinD sidecar container (role=dind) for name,
// attached to the job network — used to exercise the runner-before-DinD
// teardown ordering (ADR-004), even though WP2 itself never creates one.
func seedDindSidecarFor(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce string) string {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: controllerID, PoolID: "pool-1", InstanceName: name}
	labels := id.DindContainerLabels(createdAt)
	if nonce != "" {
		labels[spec.LabelCreateNonce] = nonce
	}
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels}, &container.HostConfig{
		NetworkMode: container.NetworkMode(spec.JobNetworkName(name)),
	}, nil, nil, spec.DindContainerName(name))
	if err != nil {
		t.Fatalf("seed DinD ContainerCreate for %q returned unexpected error: %v", name, err)
	}
	fake.SetState(resp.ID, "running", false)
	return resp.ID
}

// seedCacheVolume seeds an ADR-003 cache volume (managed, this controller, but
// cache=true and NO instance-name) that the ADR-004 predicate must exclude.
func seedCacheVolume(t *testing.T, fake *docker.FakeClient, name string) {
	t.Helper()
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: name, Labels: map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: testControllerID,
		spec.LabelCache:        "true",
	}}); err != nil {
		t.Fatalf("seed cache VolumeCreate returned unexpected error: %v", err)
	}
}

// --- count / lookup helpers --------------------------------------------------

func countContainers(t *testing.T, fake *docker.FakeClient) int {
	t.Helper()
	out, err := fake.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		t.Fatalf("ContainerList returned unexpected error: %v", err)
	}
	return len(out)
}

func countNetworks(t *testing.T, fake *docker.FakeClient) int {
	t.Helper()
	out, err := fake.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	return len(out)
}

func countVolumes(t *testing.T, fake *docker.FakeClient) int {
	t.Helper()
	out, err := fake.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	return len(out.Volumes)
}

func findNetwork(t *testing.T, fake *docker.FakeClient, name string) network.Summary {
	t.Helper()
	out, err := fake.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		t.Fatalf("NetworkList returned unexpected error: %v", err)
	}
	for _, n := range out {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("network %q not found", name)
	return network.Summary{}
}

func findVolume(t *testing.T, fake *docker.FakeClient, name string) *volume.Volume {
	t.Helper()
	out, err := fake.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	for _, v := range out.Volumes {
		if v != nil && v.Name == name {
			return v
		}
	}
	t.Fatalf("volume %q not found", name)
	return nil
}

// --- CreateClaimNetwork ------------------------------------------------------

func TestCreateClaimNetworkSuccessStampsClaimLabels(t *testing.T) {
	m, fake := newManager(t)

	netID, dupErr, err := m.CreateClaimNetwork(context.Background(), identityFor("job-1"), "nonce-1", true)
	if err != nil || dupErr != nil {
		t.Fatalf("CreateClaimNetwork = (%q, dup=%v, err=%v), want success", netID, dupErr, err)
	}
	if netID == "" {
		t.Fatal("CreateClaimNetwork returned an empty network ID")
	}

	n := findNetwork(t, fake, spec.JobNetworkName("job-1"))
	if n.Labels[spec.LabelManaged] != "true" ||
		n.Labels[spec.LabelControllerID] != testControllerID ||
		n.Labels[spec.LabelInstanceName] != "job-1" ||
		n.Labels[spec.LabelResource] != spec.ResourceJobNetwork ||
		n.Labels[spec.LabelCreateNonce] != "nonce-1" {
		t.Errorf("claim-marker labels missing/incorrect: %v", n.Labels)
	}
	if n.Labels[spec.LabelCreatedAt] == "" {
		t.Errorf("claim marker missing created-at: %v", n.Labels)
	}
}

func TestCreateClaimNetworkDuplicateReturnsExit31(t *testing.T) {
	m, fake := newManager(t)
	// An existing managed claim marker for the same instance name.
	seedNetworkFor(t, fake, testControllerID, "job-1", time.Now(), "other-nonce")

	netID, dupErr, err := m.CreateClaimNetwork(context.Background(), identityFor("job-1"), "nonce-1", true)
	if err != nil {
		t.Fatalf("CreateClaimNetwork returned a hard error, want a duplicate: %v", err)
	}
	if netID != "" {
		t.Errorf("netID = %q, want empty on duplicate", netID)
	}
	if dupErr == nil || !errors.Is(dupErr, gErrors.ErrDuplicateEntity) {
		t.Errorf("dupErr = %v, want a duplicate error (exit 31)", dupErr)
	}
	// The original network is untouched: still one network, original nonce.
	if countNetworks(t, fake) != 1 {
		t.Errorf("network count = %d, want 1 (peer untouched)", countNetworks(t, fake))
	}
	if findNetwork(t, fake, spec.JobNetworkName("job-1")).Labels[spec.LabelCreateNonce] != "other-nonce" {
		t.Error("the existing claim marker's nonce was overwritten by the duplicate attempt")
	}
}

func TestCreateClaimNetworkForeignNameCollisionIsNotADuplicate(t *testing.T) {
	m, fake := newManager(t)
	// A FOREIGN network occupies the deterministic job-network name but carries
	// none of our managed labels.
	if _, err := fake.NetworkCreate(context.Background(), spec.JobNetworkName("job-1"), network.CreateOptions{
		Labels: map[string]string{"some.other/label": "x"},
	}); err != nil {
		t.Fatalf("seed foreign NetworkCreate returned unexpected error: %v", err)
	}

	netID, dupErr, err := m.CreateClaimNetwork(context.Background(), identityFor("job-1"), "nonce-1", true)
	if dupErr != nil {
		t.Errorf("dupErr = %v, want nil: a foreign name collision is NOT our duplicate", dupErr)
	}
	if err == nil {
		t.Fatal("expected a hard error for a foreign name collision, got nil")
	}
	if errors.Is(err, gErrors.ErrDuplicateEntity) {
		t.Errorf("a foreign collision must not map to exit 31: %v", err)
	}
	if netID != "" {
		t.Errorf("netID = %q, want empty", netID)
	}
	// The foreign network is untouched.
	if countNetworks(t, fake) != 1 {
		t.Errorf("network count = %d, want 1 (foreign untouched)", countNetworks(t, fake))
	}
}

func TestCreateClaimNetworkCleanFailureLeavesNothing(t *testing.T) {
	m, fake := newManager(t)
	fake.NetworkCreateErr = errors.New("address pool exhausted")

	netID, dupErr, err := m.CreateClaimNetwork(context.Background(), identityFor("job-1"), "nonce-1", true)
	if err == nil || dupErr != nil || netID != "" {
		t.Fatalf("CreateClaimNetwork = (%q, dup=%v, err=%v), want a clean hard error", netID, dupErr, err)
	}
	if countNetworks(t, fake) != 0 {
		t.Errorf("clean create failure left %d networks, want 0", countNetworks(t, fake))
	}
}

func TestCreateClaimNetworkAmbiguousCreateCleansUpLeakedMarker(t *testing.T) {
	m, fake := newManager(t)
	// The daemon created the network before erroring (ambiguous): the leaked
	// marker carries our nonce and must be cleaned up.
	fake.NetworkCreateErr = errors.New("transient daemon error")
	fake.NetworkCreateErrLeaks = true

	_, _, err := m.CreateClaimNetwork(context.Background(), identityFor("job-1"), "nonce-1", true)
	if err == nil {
		t.Fatal("expected a hard error from the ambiguous create, got nil")
	}
	if countNetworks(t, fake) != 0 {
		t.Errorf("ambiguous create leaked %d networks, want 0 (best-effort cleanup)", countNetworks(t, fake))
	}
}

// --- CreateWorkspaceVolume ---------------------------------------------------

func TestCreateWorkspaceVolumeFresh(t *testing.T) {
	m, fake := newManager(t)
	if err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "nonce-1"); err != nil {
		t.Fatalf("CreateWorkspaceVolume returned unexpected error: %v", err)
	}
	v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1"))
	if v.Labels[spec.LabelResource] != spec.ResourceWorkspace ||
		v.Labels[spec.LabelInstanceName] != "job-1" ||
		v.Labels[spec.LabelCreateNonce] != "nonce-1" {
		t.Errorf("workspace volume labels missing/incorrect: %v", v.Labels)
	}
}

func TestCreateWorkspaceVolumeReplacesStaleContent(t *testing.T) {
	m, fake := newManager(t)
	// A stale workspace volume from a crashed prior allocation of the same
	// instance name, carrying a DIFFERENT nonce and a residue marker. Because
	// VolumeCreate is idempotent, a naive create would silently reuse it.
	staleLabels := identityFor("job-1").WorkspaceVolumeLabels(time.Now().Add(-time.Hour))
	staleLabels[spec.LabelCreateNonce] = "stale-nonce"
	staleLabels["test.residue"] = "prior-job"
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1"),
		Labels: staleLabels,
	}); err != nil {
		t.Fatalf("seed stale VolumeCreate returned unexpected error: %v", err)
	}

	if err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "fresh-nonce"); err != nil {
		t.Fatalf("CreateWorkspaceVolume returned unexpected error: %v", err)
	}

	// Still exactly one volume, but now fresh: our nonce, no prior-job residue.
	if countVolumes(t, fake) != 1 {
		t.Errorf("volume count = %d, want 1", countVolumes(t, fake))
	}
	v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1"))
	if v.Labels[spec.LabelCreateNonce] != "fresh-nonce" {
		t.Errorf("workspace nonce = %q, want fresh-nonce (stale volume not replaced)", v.Labels[spec.LabelCreateNonce])
	}
	if _, residual := v.Labels["test.residue"]; residual {
		t.Error("stale volume content was reused: the prior-job residue label survived")
	}
}

func TestCreateWorkspaceVolumeCreateErrorSurfaces(t *testing.T) {
	m, fake := newManager(t)
	fake.VolumeCreateErr = errors.New("no space left on device")
	if err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "nonce-1"); err == nil {
		t.Fatal("expected CreateWorkspaceVolume to surface the create error, got nil")
	}
}

// --- TeardownAllocation ------------------------------------------------------

func TestTeardownAllocationRemovesAllKinds(t *testing.T) {
	m, fake := newManager(t)
	seedAllocation(t, fake, "job-1", time.Now(), "nonce-1", "running")

	found, err := m.TeardownAllocation(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("TeardownAllocation returned unexpected error: %v", err)
	}
	if !found {
		t.Error("TeardownAllocation found=false, want true (resources existed)")
	}
	// The container was attached to the network; the fake rejects removing an
	// in-use network, so all three being gone proves the container-before-
	// network ordering held (ADR-004).
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("after teardown: %d containers, %d networks, %d volumes; want 0/0/0",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
	}
}

func TestTeardownAllocationIdempotentWhenGone(t *testing.T) {
	m, fake := newManager(t)
	found, err := m.TeardownAllocation(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("TeardownAllocation of a missing instance returned an error: %v", err)
	}
	if found {
		t.Error("TeardownAllocation found=true for a missing instance, want false (exit 30)")
	}
	_ = fake
}

func TestTeardownAllocationLeavesForeignCacheAndOtherController(t *testing.T) {
	m, fake := newManager(t)
	seedAllocation(t, fake, "job-1", time.Now(), "n1", "exited")
	seedAllocationForController(t, fake, "other-controller", "job-2", time.Now(), "n2", "running")
	seedCacheVolume(t, fake, "toolcache-generation-1")

	found, err := m.TeardownAllocation(context.Background(), "job-1")
	if err != nil || !found {
		t.Fatalf("TeardownAllocation(job-1) = (found=%v, err=%v), want (true, nil)", found, err)
	}

	// job-1 gone; the other controller's allocation and the cache volume survive.
	if countNetworks(t, fake) != 1 {
		t.Errorf("network count = %d, want 1 (other-controller's job-2 net)", countNetworks(t, fake))
	}
	if countContainers(t, fake) != 1 {
		t.Errorf("container count = %d, want 1 (other-controller's runner)", countContainers(t, fake))
	}
	// other-controller's workspace volume (1) + cache volume (1) = 2 remain.
	if countVolumes(t, fake) != 2 {
		t.Errorf("volume count = %d, want 2 (other-controller workspace + cache)", countVolumes(t, fake))
	}
	findVolume(t, fake, "toolcache-generation-1") // must still exist
}

func TestTeardownAllocationRemovesRunnerBeforeDind(t *testing.T) {
	m, fake := newManager(t)
	// A runner AND a DinD sidecar (WP3 shape) for the same instance, both
	// attached to the job network. Teardown must remove both containers (runner
	// first) before the network, or the fake's active-endpoint check trips.
	seedAllocation(t, fake, "job-1", time.Now(), "n1", "running")
	seedDindSidecarFor(t, fake, testControllerID, "job-1", time.Now(), "n1")

	found, err := m.TeardownAllocation(context.Background(), "job-1")
	if err != nil || !found {
		t.Fatalf("TeardownAllocation = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("after teardown: %d/%d/%d containers/networks/volumes, want 0/0/0",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
	}
}

func TestTeardownAllocationJoinsVolumeRemovalError(t *testing.T) {
	m, fake := newManager(t)
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", time.Now(), "n1")
	fake.VolumeRemoveErr = errors.New("volume busy")

	if _, err := m.TeardownAllocation(context.Background(), "job-1"); err == nil {
		t.Fatal("expected TeardownAllocation to surface the volume removal error, got nil")
	}
}

func TestTeardownAllocationJoinsContainerRemovalError(t *testing.T) {
	m, fake := newManager(t)
	seedRunnerFor(t, fake, testControllerID, "job-1", time.Now(), "n1", "exited")
	fake.RemoveErr = errors.New("container busy")

	if _, err := m.TeardownAllocation(context.Background(), "job-1"); err == nil {
		t.Fatal("expected TeardownAllocation to surface the container removal error, got nil")
	}
}

func TestTeardownAllocationJoinsNetworkRemovalError(t *testing.T) {
	m, fake := newManager(t)
	seedNetworkFor(t, fake, testControllerID, "job-1", time.Now(), "n1")
	fake.NetworkRemoveErr = errors.New("network busy")

	if _, err := m.TeardownAllocation(context.Background(), "job-1"); err == nil {
		t.Fatal("expected TeardownAllocation to surface the network removal error, got nil")
	}
}

func TestCreateWorkspaceVolumeStaleReplaceRemoveErrorSurfaces(t *testing.T) {
	m, fake := newManager(t)
	// A stale volume with a different nonce forces the remove-and-recreate path;
	// a VolumeRemove failure there must surface.
	staleLabels := identityFor("job-1").WorkspaceVolumeLabels(time.Now().Add(-time.Hour))
	staleLabels[spec.LabelCreateNonce] = "stale-nonce"
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1"),
		Labels: staleLabels,
	}); err != nil {
		t.Fatalf("seed stale VolumeCreate returned unexpected error: %v", err)
	}
	fake.VolumeRemoveErr = errors.New("volume in use")

	if err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "fresh-nonce"); err == nil {
		t.Fatal("expected CreateWorkspaceVolume to surface the stale-replacement removal error, got nil")
	}
}

// --- TeardownAll (RemoveAllInstances) ----------------------------------------

func TestTeardownAllRemovesEveryControllerAllocationButNotCacheOrForeign(t *testing.T) {
	m, fake := newManager(t)
	seedAllocation(t, fake, "job-a", time.Now(), "na", "running")
	seedAllocation(t, fake, "job-b", time.Now(), "nb", "exited")
	// Out of scope: another controller's allocation and a cache volume.
	seedAllocationForController(t, fake, "other-controller", "job-c", time.Now(), "nc", "running")
	seedCacheVolume(t, fake, "toolcache-generation-1")

	if err := m.TeardownAll(context.Background()); err != nil {
		t.Fatalf("TeardownAll returned unexpected error: %v", err)
	}

	// job-a and job-b are gone; the other controller's allocation + cache remain.
	if countContainers(t, fake) != 1 {
		t.Errorf("container count = %d, want 1 (other-controller runner)", countContainers(t, fake))
	}
	if countNetworks(t, fake) != 1 {
		t.Errorf("network count = %d, want 1 (other-controller net)", countNetworks(t, fake))
	}
	if countVolumes(t, fake) != 2 {
		t.Errorf("volume count = %d, want 2 (other-controller workspace + cache)", countVolumes(t, fake))
	}
	findVolume(t, fake, "toolcache-generation-1")
}

// --- Rollback (creation guard) -----------------------------------------------

func TestRollbackIsNonceKeyed(t *testing.T) {
	m, fake := newManager(t)
	seedAllocation(t, fake, "job-1", time.Now(), "mine", "running")

	// Rollback with a DIFFERENT nonce (as if a concurrent peer owned it) must
	// touch nothing.
	if err := m.Rollback(context.Background(), "job-1", "peer-nonce"); err != nil {
		t.Fatalf("Rollback(wrong nonce) returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 1 || countNetworks(t, fake) != 1 || countVolumes(t, fake) != 1 {
		t.Fatalf("Rollback with a mismatched nonce removed resources it did not own: %d/%d/%d",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
	}

	// Rollback with the correct nonce removes exactly this attempt's resources.
	if err := m.Rollback(context.Background(), "job-1", "mine"); err != nil {
		t.Fatalf("Rollback(correct nonce) returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("after Rollback: %d/%d/%d containers/networks/volumes, want 0/0/0",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
	}
}

// --- Orphan sweep ------------------------------------------------------------

func TestSweepSkipsRunningRunnerRegardlessOfAge(t *testing.T) {
	m, fake := newManager(t)
	// Old created-at, but the runner is running: an active long job.
	seedAllocation(t, fake, "job-1", time.Now().Add(-time.Hour), "n1", "running")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 1 || countNetworks(t, fake) != 1 {
		t.Error("sweep tore down an active (running) allocation")
	}
}

func TestSweepSkipsYoungAllocationWithinGrace(t *testing.T) {
	m, fake := newManager(t)
	// Exited, but created just now: inside the grace window (an in-flight peer,
	// or a just-finished job GARM may be about to delete).
	seedAllocation(t, fake, "job-1", time.Now(), "n1", "exited")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 1 || countNetworks(t, fake) != 1 {
		t.Error("sweep tore down an allocation still within the grace window")
	}
}

func TestSweepRemovesExitedRunnerPastGrace(t *testing.T) {
	m, fake := newManager(t)
	seedAllocation(t, fake, "job-1", time.Now().Add(-time.Hour), "n1", "exited")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("sweep did not fully tear down a past-grace exited allocation: %d/%d/%d",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
	}
}

func TestSweepRemovesDanglingNetworkWithNoRunnerPastGrace(t *testing.T) {
	m, fake := newManager(t)
	// A claim-marker network + workspace volume, no runner container, past grace:
	// an abandoned, never-completed create.
	old := time.Now().Add(-time.Hour)
	seedNetworkFor(t, fake, testControllerID, "job-1", old, "n1")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", old, "n1")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("sweep left a dangling network/volume: %d/%d", countNetworks(t, fake), countVolumes(t, fake))
	}
}

func TestSweepSkipsAllocationWithUnparseableCreatedAt(t *testing.T) {
	m, fake := newManager(t)
	// A managed network whose created-at label is malformed: the sweep cannot
	// age it, so it must be conservatively skipped rather than deleted.
	labels := identityFor("job-1").NetworkLabels(time.Now().Add(-time.Hour))
	labels[spec.LabelCreatedAt] = "not-a-timestamp"
	labels[spec.LabelCreateNonce] = "n1"
	if _, err := fake.NetworkCreate(context.Background(), spec.JobNetworkName("job-1"), network.CreateOptions{Labels: labels}); err != nil {
		t.Fatalf("seed NetworkCreate returned unexpected error: %v", err)
	}

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countNetworks(t, fake) != 1 {
		t.Errorf("sweep removed an allocation it could not age (malformed created-at); network count = %d, want 1", countNetworks(t, fake))
	}
}

func TestSweepStaleTargetsOnlyOneInstance(t *testing.T) {
	m, fake := newManager(t)
	seedAllocation(t, fake, "job-stale", time.Now().Add(-time.Hour), "n1", "exited")
	seedAllocation(t, fake, "job-live", time.Now(), "n2", "running")

	if err := m.SweepStale(context.Background(), "job-stale"); err != nil {
		t.Fatalf("SweepStale returned unexpected error: %v", err)
	}
	// job-stale gone; job-live intact.
	if countContainers(t, fake) != 1 || countNetworks(t, fake) != 1 || countVolumes(t, fake) != 1 {
		t.Errorf("SweepStale should have removed only job-stale: %d/%d/%d",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
	}
	findNetwork(t, fake, spec.JobNetworkName("job-live")) // must still exist
}

func TestSweepNeverTouchesForeignOrCacheOrOtherController(t *testing.T) {
	m, fake := newManager(t)
	// Our own abandoned allocation (will be swept).
	seedAllocation(t, fake, "job-1", time.Now().Add(-time.Hour), "n1", "exited")
	// Another controller's abandoned allocation (must be untouched).
	seedAllocationForController(t, fake, "other-controller", "job-2", time.Now().Add(-time.Hour), "n2", "exited")
	// A cache volume (no instance-name; must be untouched).
	seedCacheVolume(t, fake, "toolcache-generation-1")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}

	// Only our job-1 is gone. other-controller's net+runner and the cache volume
	// remain.
	if countNetworks(t, fake) != 1 {
		t.Errorf("network count = %d, want 1 (other-controller's)", countNetworks(t, fake))
	}
	if countContainers(t, fake) != 1 {
		t.Errorf("container count = %d, want 1 (other-controller's)", countContainers(t, fake))
	}
	if countVolumes(t, fake) != 2 {
		t.Errorf("volume count = %d, want 2 (other-controller workspace + cache)", countVolumes(t, fake))
	}
	findVolume(t, fake, "toolcache-generation-1") // must still exist
}
