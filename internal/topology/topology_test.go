package topology

import (
	"context"
	"errors"
	"strings"
	"sync"
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
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: spec.WorkspaceVolumeName(name, nonce), Labels: labels}); err != nil {
		t.Fatalf("seed VolumeCreate for %q returned unexpected error: %v", name, err)
	}
}

func seedSocketVolumeFor(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce string) {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: controllerID, PoolID: "pool-1", InstanceName: name}
	labels := id.SocketVolumeLabels(createdAt)
	if nonce != "" {
		labels[spec.LabelCreateNonce] = nonce
	}
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: spec.SocketVolumeName(name, nonce), Labels: labels}); err != nil {
		t.Fatalf("seed socket VolumeCreate for %q returned unexpected error: %v", name, err)
	}
}

func seedDindStateVolumeFor(t *testing.T, fake *docker.FakeClient, controllerID, name string, createdAt time.Time, nonce string) {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: controllerID, PoolID: "pool-1", InstanceName: name}
	labels := id.DindStateVolumeLabels(createdAt)
	if nonce != "" {
		labels[spec.LabelCreateNonce] = nonce
	}
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{Name: spec.DindStateVolumeName(name, nonce), Labels: labels}); err != nil {
		t.Fatalf("seed dind-state VolumeCreate for %q returned unexpected error: %v", name, err)
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
	// The exited-runner sweep grace (F8) is measured from State.FinishedAt, so a
	// stopped seed runner models finishing when the allocation was created — old
	// created-at ⇒ old FinishedAt ⇒ past grace, now ⇒ within grace.
	if state == "exited" || state == "dead" {
		fake.SetFinishedAt(resp.ID, createdAt)
	}
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
	// The volume name is generation-unique (F4): <instance>-<nonce>-workspace.
	v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "nonce-1"))
	if v.Labels[spec.LabelResource] != spec.ResourceWorkspace ||
		v.Labels[spec.LabelInstanceName] != "job-1" ||
		v.Labels[spec.LabelCreateNonce] != "nonce-1" {
		t.Errorf("workspace volume labels missing/incorrect: %v", v.Labels)
	}
}

// TestCreateWorkspaceVolumePriorGenerationNameDiffers is the F4 create-level
// structural guard: a stale workspace volume left by a crashed PRIOR generation
// of the same instance name carries that generation's nonce, so its name
// (<instance>-<staleNonce>-workspace) DIFFERS from the fresh generation's name
// (<instance>-<freshNonce>-workspace). The fresh create therefore never
// idempotent-hits it and never reuses its content — the two volumes are
// distinct by construction. (The stale one is reaped by the pre-create sweep /
// teardown, not by create.)
func TestCreateWorkspaceVolumePriorGenerationNameDiffers(t *testing.T) {
	m, fake := newManager(t)
	// A stale prior-generation workspace volume with residue, at ITS OWN
	// generation-unique name (nonce "stale-gen").
	staleLabels := identityFor("job-1").WorkspaceVolumeLabels(time.Now().Add(-time.Hour))
	staleLabels[spec.LabelCreateNonce] = "stale-gen"
	staleLabels["test.residue"] = "prior-job"
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1", "stale-gen"),
		Labels: staleLabels,
	}); err != nil {
		t.Fatalf("seed stale VolumeCreate returned unexpected error: %v", err)
	}

	if err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "fresh-gen"); err != nil {
		t.Fatalf("CreateWorkspaceVolume returned unexpected error: %v", err)
	}

	// The fresh volume is a DISTINCT, empty resource under a different name.
	fresh := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "fresh-gen"))
	if fresh.Labels[spec.LabelCreateNonce] != "fresh-gen" {
		t.Errorf("fresh workspace nonce = %q, want fresh-gen", fresh.Labels[spec.LabelCreateNonce])
	}
	if _, residual := fresh.Labels["test.residue"]; residual {
		t.Error("[F4] fresh generation reused stale content: residue label present on the fresh volume")
	}
	// Both volumes coexist under distinct names — the stale one was NOT
	// name-collided/replaced by create (it is the sweep's job, not create's).
	if countVolumes(t, fake) != 2 {
		t.Errorf("volume count = %d, want 2 (stale + fresh under distinct generation-unique names)", countVolumes(t, fake))
	}
}

// TestCreateWorkspaceVolumeReplacesStaleContent exercises createFreshVolume's
// ownership-validated replace path — now defense-in-depth behind the F4
// generation-unique names. Since a fresh generation's name embeds its own
// nonce, the ONLY way VolumeCreate can still idempotent-hit an existing volume
// is at THIS generation's own name (e.g. a retried create step within one
// attempt). We model that adversarial case: a volume already sitting at the
// generation-unique name but carrying a DIFFERENT (stale) nonce label and
// residue. createFreshVolume must detect the nonce mismatch, confirm the
// ownership tuple, and replace it with a fresh, empty volume.
func TestCreateWorkspaceVolumeReplacesStaleContent(t *testing.T) {
	m, fake := newManager(t)
	// A stale-labeled volume occupying THIS generation's own name (nonce
	// "fresh-nonce" in the name) but tagged with a stale nonce + residue.
	staleLabels := identityFor("job-1").WorkspaceVolumeLabels(time.Now().Add(-time.Hour))
	staleLabels[spec.LabelCreateNonce] = "stale-nonce"
	staleLabels["test.residue"] = "prior-job"
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1", "fresh-nonce"),
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
	v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "fresh-nonce"))
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

// TestCreateWorkspaceVolumeRefusesToReplaceForeignVolume is the F3 red-line
// guard: a volume occupying the deterministic workspace name that does NOT
// carry this controller's full ownership tuple (here, a completely foreign
// volume) must NEVER be force-removed. The idempotent VolumeCreate returns it,
// but createFreshVolume must fail closed rather than destroy someone else's
// data.
func TestCreateWorkspaceVolumeRefusesToReplaceForeignVolume(t *testing.T) {
	m, fake := newManager(t)
	// A foreign volume occupying THIS generation's own name (nonce "nonce-1")
	// so the idempotent VolumeCreate hits it and the refuse path is exercised.
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1", "nonce-1"),
		Labels: map[string]string{"some.other/label": "x"},
	}); err != nil {
		t.Fatalf("seed foreign VolumeCreate returned unexpected error: %v", err)
	}

	err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "nonce-1")
	if err == nil {
		t.Fatal("expected CreateWorkspaceVolume to fail closed on a foreign name collision, got nil")
	}
	// The foreign volume is untouched: still exactly one, still its own labels.
	if countVolumes(t, fake) != 1 {
		t.Errorf("volume count = %d, want 1 (foreign untouched)", countVolumes(t, fake))
	}
	if v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "nonce-1")); v.Labels["some.other/label"] != "x" {
		t.Errorf("foreign volume labels = %v, want the original foreign label untouched", v.Labels)
	}
}

// TestCreateWorkspaceVolumeRefusesToReplaceOtherControllerVolume proves the F3
// ownership tuple is checked in FULL: a managed volume with the right resource
// and instance-name but a DIFFERENT controller-id is not ours, so it must not
// be replaced.
func TestCreateWorkspaceVolumeRefusesToReplaceOtherControllerVolume(t *testing.T) {
	m, fake := newManager(t)
	other := spec.AllocationIdentity{ControllerID: "other-controller", PoolID: "pool-1", InstanceName: "job-1"}
	labels := other.WorkspaceVolumeLabels(time.Now())
	labels[spec.LabelCreateNonce] = "theirs"
	// Occupy THIS generation's own name so the idempotent VolumeCreate hits it.
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1", "nonce-1"),
		Labels: labels,
	}); err != nil {
		t.Fatalf("seed other-controller VolumeCreate returned unexpected error: %v", err)
	}

	if err := m.CreateWorkspaceVolume(context.Background(), identityFor("job-1"), "nonce-1"); err == nil {
		t.Fatal("expected CreateWorkspaceVolume to fail closed on an other-controller volume, got nil")
	}
	if countVolumes(t, fake) != 1 {
		t.Errorf("volume count = %d, want 1 (other-controller volume untouched)", countVolumes(t, fake))
	}
	if v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "nonce-1")); v.Labels[spec.LabelCreateNonce] != "theirs" {
		t.Error("an other-controller volume was replaced (nonce no longer theirs)")
	}
}

// --- CreateSocketVolume / CreateDindStateVolume (WP3 DinD) --------------------

func TestCreateSocketVolumeFresh(t *testing.T) {
	m, fake := newManager(t)
	if err := m.CreateSocketVolume(context.Background(), identityFor("job-1"), "nonce-1"); err != nil {
		t.Fatalf("CreateSocketVolume returned unexpected error: %v", err)
	}
	v := findVolume(t, fake, spec.SocketVolumeName("job-1", "nonce-1"))
	if v.Labels[spec.LabelResource] != spec.ResourceSocket ||
		v.Labels[spec.LabelInstanceName] != "job-1" ||
		v.Labels[spec.LabelCreateNonce] != "nonce-1" {
		t.Errorf("socket volume labels missing/incorrect: %v", v.Labels)
	}
}

func TestCreateDindStateVolumeFresh(t *testing.T) {
	m, fake := newManager(t)
	if err := m.CreateDindStateVolume(context.Background(), identityFor("job-1"), "nonce-1"); err != nil {
		t.Fatalf("CreateDindStateVolume returned unexpected error: %v", err)
	}
	v := findVolume(t, fake, spec.DindStateVolumeName("job-1", "nonce-1"))
	if v.Labels[spec.LabelResource] != spec.ResourceDindState ||
		v.Labels[spec.LabelInstanceName] != "job-1" ||
		v.Labels[spec.LabelCreateNonce] != "nonce-1" {
		t.Errorf("dind-state volume labels missing/incorrect: %v", v.Labels)
	}
}

// TestCreateSocketVolumeReplacesStaleContent proves the shared
// createFreshVolume stale-replacement guarantee holds for the socket volume
// too (not just workspace): a crashed prior allocation's socket volume must
// never be silently reused (the idempotent VolumeCreate hazard).
func TestCreateSocketVolumeReplacesStaleContent(t *testing.T) {
	m, fake := newManager(t)
	staleLabels := identityFor("job-1").SocketVolumeLabels(time.Now().Add(-time.Hour))
	staleLabels[spec.LabelCreateNonce] = "stale-nonce"
	staleLabels["test.residue"] = "prior-socket"
	// Occupy THIS generation's own name (nonce "fresh-nonce") with a stale nonce
	// label, so the idempotent VolumeCreate hits it and the replace path runs.
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.SocketVolumeName("job-1", "fresh-nonce"),
		Labels: staleLabels,
	}); err != nil {
		t.Fatalf("seed stale socket VolumeCreate returned unexpected error: %v", err)
	}

	if err := m.CreateSocketVolume(context.Background(), identityFor("job-1"), "fresh-nonce"); err != nil {
		t.Fatalf("CreateSocketVolume returned unexpected error: %v", err)
	}
	if countVolumes(t, fake) != 1 {
		t.Errorf("volume count = %d, want 1", countVolumes(t, fake))
	}
	v := findVolume(t, fake, spec.SocketVolumeName("job-1", "fresh-nonce"))
	if v.Labels[spec.LabelCreateNonce] != "fresh-nonce" {
		t.Errorf("socket nonce = %q, want fresh-nonce (stale volume not replaced)", v.Labels[spec.LabelCreateNonce])
	}
	if _, residual := v.Labels["test.residue"]; residual {
		t.Error("stale socket volume was reused: the prior residue label survived")
	}
}

func TestCreateDindStateVolumeCreateErrorSurfaces(t *testing.T) {
	m, fake := newManager(t)
	fake.VolumeCreateErr = errors.New("no space left on device")
	if err := m.CreateDindStateVolume(context.Background(), identityFor("job-1"), "nonce-1"); err == nil {
		t.Fatal("expected CreateDindStateVolume to surface the create error, got nil")
	}
}

// TestTeardownAllocationRemovesFullDindTopology seeds every WP3 resource kind
// (runner + DinD sidecar + job network + workspace/socket/dind-state volumes)
// and asserts teardown removes ALL of them in the ADR-004 order — both
// containers before the network (the fake's active-endpoint check enforces
// that), and all three volumes gone last.
func TestTeardownAllocationRemovesFullDindTopology(t *testing.T) {
	m, fake := newManager(t)
	now := time.Now()
	seedNetworkFor(t, fake, testControllerID, "job-1", now, "n1")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", now, "n1")
	seedSocketVolumeFor(t, fake, testControllerID, "job-1", now, "n1")
	seedDindStateVolumeFor(t, fake, testControllerID, "job-1", now, "n1")
	seedRunnerFor(t, fake, testControllerID, "job-1", now, "n1", "running")
	seedDindSidecarFor(t, fake, testControllerID, "job-1", now, "n1")

	if countVolumes(t, fake) != 3 {
		t.Fatalf("seeded volume count = %d, want 3 (workspace/socket/dind-state)", countVolumes(t, fake))
	}

	found, err := m.TeardownAllocation(context.Background(), "job-1")
	if err != nil || !found {
		t.Fatalf("TeardownAllocation = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("after full-DinD teardown: %d/%d/%d containers/networks/volumes, want 0/0/0",
			countContainers(t, fake), countNetworks(t, fake), countVolumes(t, fake))
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

// TestTeardownRemovesNetworkLast is the F4 ordering guard: teardown removes all
// containers first, then all volumes, then the job network (the claim marker)
// LAST — so the network is held until every volume is gone and a concurrent
// same-name create cannot claim a new generation mid-teardown (ADR-004
// amendment 2026-07-21).
func TestTeardownRemovesNetworkLast(t *testing.T) {
	m, fake := newManager(t)
	now := time.Now()
	seedNetworkFor(t, fake, testControllerID, "job-1", now, "n1")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", now, "n1")
	seedSocketVolumeFor(t, fake, testControllerID, "job-1", now, "n1")
	seedDindStateVolumeFor(t, fake, testControllerID, "job-1", now, "n1")
	seedRunnerFor(t, fake, testControllerID, "job-1", now, "n1", "running")
	seedDindSidecarFor(t, fake, testControllerID, "job-1", now, "n1")

	found, err := m.TeardownAllocation(context.Background(), "job-1")
	if err != nil || !found {
		t.Fatalf("TeardownAllocation = (found=%v, err=%v), want (true, nil)", found, err)
	}

	// Walk the recorded removal order: once a volume has been removed, no
	// container may follow; once the network is removed, nothing may follow.
	seenVolume, seenNetwork := false, false
	for _, ev := range fake.RemoveOrder {
		switch {
		case strings.HasPrefix(ev, "container:"):
			if seenVolume || seenNetwork {
				t.Errorf("container removed after a volume/network: order=%v", fake.RemoveOrder)
			}
		case strings.HasPrefix(ev, "volume:"):
			seenVolume = true
			if seenNetwork {
				t.Errorf("volume removed after the network (network must be LAST): order=%v", fake.RemoveOrder)
			}
		case strings.HasPrefix(ev, "network:"):
			seenNetwork = true
		}
	}
	// The network must be the very last removal (F4).
	if n := len(fake.RemoveOrder); n == 0 || !strings.HasPrefix(fake.RemoveOrder[n-1], "network:") {
		t.Errorf("last removal = %q, want the network last: order=%v", fake.RemoveOrder, fake.RemoveOrder)
	}
}

// TestTeardownAllocationGenerationScopedProtectsConcurrentCreate reproduces the
// F4 T1/T2/gen-B interleave: a teardown of generation A captures A's nonce, then
// — in the window between that capture and its per-kind volume removal — a peer
// teardown finishes removing A and a concurrent CreateInstance claims generation
// B, creating B's fresh CLAIM NETWORK under the SAME stable name (the network is
// the dedup primitive, so its name never changes) and B's workspace volume under
// a generation-UNIQUE name embedding B's nonce. Before the fix, the still-running
// teardown enumerated volumes purely by instance-name and deleted B's brand-new
// volume. This test exercises the generation-nonce GATE (now defense-in-depth
// behind the unique names) — it still protects the stable-named claim network,
// and it also protects B's volume even though the VolumeListHook makes B's volume
// appear in the teardown's own list snapshot.
//
// The interleave is driven deterministically via the fake's VolumeListHook,
// which fires exactly when the teardown lists volumes for removal (T2's
// per-kind volume list) — the precise instant the original bug struck.
func TestTeardownAllocationGenerationScopedProtectsConcurrentCreate(t *testing.T) {
	m, fake := newManager(t)
	now := time.Now()
	// Generation A: claim network + workspace volume + a running runner.
	seedNetworkFor(t, fake, testControllerID, "job-1", now, "gen-a")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", now, "gen-a")
	seedRunnerFor(t, fake, testControllerID, "job-1", now, "gen-a", "running")

	var once sync.Once
	fake.VolumeListHook = func() {
		once.Do(func() {
			ctx := context.Background()
			// T1 finishes tearing down generation A (its runner is already gone —
			// removeContainers ran before this volume list — so its volume and
			// claim network can be removed). Gen-A's volume name embeds gen-a.
			if err := fake.VolumeRemove(ctx, spec.WorkspaceVolumeName("job-1", "gen-a"), true); err != nil {
				t.Errorf("hook: removing gen-A volume: %v", err)
			}
			nets, err := fake.NetworkList(ctx, network.ListOptions{})
			if err != nil {
				t.Errorf("hook: listing networks: %v", err)
			}
			for _, n := range nets {
				if n.Name == spec.JobNetworkName("job-1") {
					if err := fake.NetworkRemove(ctx, n.ID); err != nil {
						t.Errorf("hook: removing gen-A network: %v", err)
					}
				}
			}
			// A concurrent CreateInstance claims generation B: fresh claim network
			// (SAME stable name) and workspace volume (DIFFERENT, gen-b-embedding
			// name), a DIFFERENT nonce.
			seedNetworkFor(t, fake, testControllerID, "job-1", time.Now(), "gen-b")
			seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", time.Now(), "gen-b")
		})
	}

	found, err := m.TeardownAllocation(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("TeardownAllocation returned unexpected error: %v", err)
	}
	if !found {
		t.Error("TeardownAllocation found=false, want true (generation A's runner existed)")
	}

	// Generation B's volume AND network survive: exactly one of each, both nonce
	// gen-b. Generation A's runner is gone.
	if countContainers(t, fake) != 0 {
		t.Errorf("container count = %d, want 0 (generation A's runner removed)", countContainers(t, fake))
	}
	if countVolumes(t, fake) != 1 {
		t.Fatalf("volume count = %d, want 1 (generation B's fresh volume survives)", countVolumes(t, fake))
	}
	if v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "gen-b")); v.Labels[spec.LabelCreateNonce] != "gen-b" {
		t.Errorf("[F4] surviving volume nonce = %q, want gen-b — a stale teardown deleted the newer generation's volume", v.Labels[spec.LabelCreateNonce])
	}
	if countNetworks(t, fake) != 1 {
		t.Fatalf("network count = %d, want 1 (generation B's fresh network survives)", countNetworks(t, fake))
	}
	if n := findNetwork(t, fake, spec.JobNetworkName("job-1")); n.Labels[spec.LabelCreateNonce] != "gen-b" {
		t.Errorf("[F4] surviving network nonce = %q, want gen-b — a stale teardown deleted the newer generation's claim marker", n.Labels[spec.LabelCreateNonce])
	}
}

// TestTeardownAllocationFallbackProtectsLiveGenerationWhenClaimGone is the F4
// fallback guard: when the claim network is already GONE at teardown start
// (nothing to key a generation to), removal falls back to instance-scoped — but
// if a generation then claims the name mid-teardown (a live claim network with a
// fresh nonce appears before the per-kind volume removal), the teardown must NOT
// delete that newer generation's resources.
func TestTeardownAllocationFallbackProtectsLiveGenerationWhenClaimGone(t *testing.T) {
	m, fake := newManager(t)
	now := time.Now()
	// Only a leftover workspace volume, NO claim network at start -> fallback.
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", now, "orphan")

	var once sync.Once
	fake.VolumeListHook = func() {
		once.Do(func() {
			ctx := context.Background()
			// Remove the orphan leftover (as a peer teardown would) and let a fresh
			// generation B claim the name: a live claim network + volume appear.
			if err := fake.VolumeRemove(ctx, spec.WorkspaceVolumeName("job-1", "orphan"), true); err != nil {
				t.Errorf("hook: removing orphan volume: %v", err)
			}
			seedNetworkFor(t, fake, testControllerID, "job-1", time.Now(), "gen-b")
			seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", time.Now(), "gen-b")
		})
	}

	if _, err := m.TeardownAllocation(context.Background(), "job-1"); err != nil {
		t.Fatalf("TeardownAllocation returned unexpected error: %v", err)
	}

	// Generation B's volume and network survive: the fallback protected the nonce
	// the live claim network holds (refreshLive re-read it before removing).
	if v := findVolume(t, fake, spec.WorkspaceVolumeName("job-1", "gen-b")); v.Labels[spec.LabelCreateNonce] != "gen-b" {
		t.Errorf("[F4] surviving volume nonce = %q, want gen-b — the fallback deleted a newer generation's volume", v.Labels[spec.LabelCreateNonce])
	}
	if countNetworks(t, fake) != 1 {
		t.Errorf("network count = %d, want 1 (generation B's claim network survives the fallback)", countNetworks(t, fake))
	}
}

// TestTeardownGenerationUniqueVolumeNamesSurviveRemoveBoundaryInterleave is the
// F4 STRUCTURAL guard the older VolumeListHook tests did not exercise (the
// interleave codex flagged): generation B's volume appears AFTER the teardown's
// volume-list snapshot but strictly BEFORE the teardown physically removes the
// (generation A) volume it read from that snapshot. Because volume names are now
// generation-UNIQUE (F4), B's volume has a DIFFERENT name than anything in the
// teardown's snapshot, so the teardown — which removes only the exact names it
// enumerated — can NEVER name-collide with B's fresh volume. This is the TOCTOU
// removed by construction: the name protection here is independent of the
// generation-nonce gate (B's volume is never even enumerated by this teardown,
// so mayRemove is never consulted for it).
//
// The boundary is driven deterministically by the fake's VolumeRemoveHook, which
// fires immediately before each VolumeRemove — i.e. after the list snapshot,
// exactly where the old name-based bug struck.
func TestTeardownGenerationUniqueVolumeNamesSurviveRemoveBoundaryInterleave(t *testing.T) {
	m, fake := newManager(t)
	now := time.Now()
	// Generation A: claim network + workspace volume + a running runner.
	seedNetworkFor(t, fake, testControllerID, "job-1", now, "gen-a")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", now, "gen-a")
	seedRunnerFor(t, fake, testControllerID, "job-1", now, "gen-a", "running")

	genAVol := spec.WorkspaceVolumeName("job-1", "gen-a")
	genBVol := spec.WorkspaceVolumeName("job-1", "gen-b")

	var once sync.Once
	hookFired := false
	fake.VolumeRemoveHook = func(name string) {
		// Fire exactly at the boundary: the teardown is about to remove gen-A's
		// volume (the name it snapshotted). A concurrent CreateInstance claims
		// generation B and creates B's workspace volume under B's OWN, DIFFERENT
		// generation-unique name — after the snapshot, before this remove.
		if name != genAVol {
			return
		}
		once.Do(func() {
			hookFired = true
			seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", time.Now(), "gen-b")
		})
	}

	if _, err := m.TeardownAllocation(context.Background(), "job-1"); err != nil {
		t.Fatalf("TeardownAllocation returned unexpected error: %v", err)
	}
	if !hookFired {
		t.Fatal("VolumeRemoveHook never fired at the gen-A volume-remove boundary; the interleave was not exercised")
	}

	// Generation A's volume is gone; generation B's DIFFERENTLY-NAMED volume is
	// untouched — the teardown physically could not have named it.
	if _, err := fake.VolumeList(context.Background(), volume.ListOptions{}); err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	if countVolumes(t, fake) != 1 {
		t.Fatalf("volume count = %d, want 1 (only generation B's fresh volume survives)", countVolumes(t, fake))
	}
	surviving := findVolume(t, fake, genBVol)
	if surviving.Labels[spec.LabelCreateNonce] != "gen-b" {
		t.Errorf("[F4] surviving volume = %q nonce=%q, want %q (gen-b) — a stale teardown removed the newer generation's volume", surviving.Name, surviving.Labels[spec.LabelCreateNonce], genBVol)
	}
	// And gen-A's volume really was removed by name.
	for _, v := range volumesSnapshot(t, fake) {
		if v.Name == genAVol {
			t.Errorf("[F4] generation A's volume %q survived teardown, want removed", genAVol)
		}
	}
}

// volumesSnapshot returns all volumes in the fake, for name assertions.
func volumesSnapshot(t *testing.T, fake *docker.FakeClient) []*volume.Volume {
	t.Helper()
	out, err := fake.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		t.Fatalf("VolumeList returned unexpected error: %v", err)
	}
	return out.Volumes
}

// TestCreateClaimNetworkAmbiguousCleanupErrorIsJoined is the F7 guard: when the
// ambiguous-create cleanup (removing a leaked, nonce-tagged network) itself
// fails, that failure is joined onto — not swallowed by — the returned error,
// and the original create error is preserved for diagnostics.
func TestCreateClaimNetworkAmbiguousCleanupErrorIsJoined(t *testing.T) {
	m, fake := newManager(t)
	fake.NetworkCreateErr = errors.New("transient daemon error")
	fake.NetworkCreateErrLeaks = true
	fake.NetworkRemoveErr = errors.New("cleanup remove boom")

	_, dupErr, err := m.CreateClaimNetwork(context.Background(), identityFor("job-1"), "nonce-1", true)
	if dupErr != nil {
		t.Fatalf("dupErr = %v, want nil (a non-conflict failure is not a duplicate)", dupErr)
	}
	if err == nil {
		t.Fatal("expected a hard error from the ambiguous create, got nil")
	}
	if !strings.Contains(err.Error(), "transient daemon error") {
		t.Errorf("error = %q, want the original create error preserved", err.Error())
	}
	if !strings.Contains(err.Error(), "cleanup") {
		t.Errorf("error = %q, want the joined cleanup failure, not a swallowed one", err.Error())
	}
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
	// A stale-labeled volume occupying THIS generation's own name (nonce
	// "fresh-nonce") but tagged with a different nonce forces the idempotent-hit
	// remove-and-recreate path; a VolumeRemove failure there must surface.
	staleLabels := identityFor("job-1").WorkspaceVolumeLabels(time.Now().Add(-time.Hour))
	staleLabels[spec.LabelCreateNonce] = "stale-nonce"
	if _, err := fake.VolumeCreate(context.Background(), volume.CreateOptions{
		Name:   spec.WorkspaceVolumeName("job-1", "fresh-nonce"),
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

// TestTeardownAllRemovesOlderGenerationResidueAlongsideLiveGeneration is the
// MEDIUM rescue-path fix: unlike TeardownAllocation/the orphan sweep,
// TeardownAll (RemoveAllInstances) must NOT be generation-nonce-scoped, or an
// older generation's residue — left behind by, e.g., a volume removal that
// failed after its claim network had already been removed — is invisible to a
// rescue pass that only ever sees "whichever generation currently holds the
// claim network." This reproduces exactly that: generation A's workspace
// volume survives with NO claim network at all (modeling that prior partial
// failure directly), while generation B is a fully live allocation (network +
// volume + running runner) under a DIFFERENT nonce. A single
// RemoveAllInstances/TeardownAll pass must remove BOTH: A's residue (it still
// satisfies the ADR-004 ownership predicate; scoping is now
// generation-agnostic for this path) AND B's live allocation
// (RemoveAllInstances's documented contract is "everything this controller
// owns," ADR-004) — leaving zero managed resources for the controller.
func TestTeardownAllRemovesOlderGenerationResidueAlongsideLiveGeneration(t *testing.T) {
	m, fake := newManager(t)
	now := time.Now()

	// Generation A residue: only a stale workspace volume survives — its claim
	// network is already gone (a previously-failed volume removal after a
	// successful network removal). Its name embeds gen-a's own nonce.
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", now, "gen-a")

	// Generation B: a fully live allocation of the SAME instance name, under a
	// DIFFERENT nonce — the current generation a naively generation-scoped
	// rescue would see, while never noticing A's residue above.
	seedAllocation(t, fake, "job-1", now, "gen-b", "running")

	if countVolumes(t, fake) != 2 {
		t.Fatalf("seeded volume count = %d, want 2 (gen-A residue + gen-B workspace)", countVolumes(t, fake))
	}

	if err := m.TeardownAll(context.Background()); err != nil {
		t.Fatalf("TeardownAll returned unexpected error: %v", err)
	}

	if countContainers(t, fake) != 0 {
		t.Errorf("container count = %d, want 0 (gen-B's live runner must also be removed by the rescue)", countContainers(t, fake))
	}
	if countNetworks(t, fake) != 0 {
		t.Errorf("network count = %d, want 0 (gen-B's claim network must also be removed)", countNetworks(t, fake))
	}
	if countVolumes(t, fake) != 0 {
		t.Errorf("volume count = %d, want 0 (both gen-A's residue AND gen-B's live volume must be removed — no residue left behind)", countVolumes(t, fake))
	}
}

// TestTeardownAllJoinsRemovalError guards the teardownModeAll path the same
// way the other teardown-error tests guard teardownModeGeneration/Exact: a
// per-resource removal failure must be surfaced, not swallowed.
func TestTeardownAllJoinsRemovalError(t *testing.T) {
	m, fake := newManager(t)
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", time.Now(), "n1")
	fake.VolumeRemoveErr = errors.New("volume busy")

	if err := m.TeardownAll(context.Background()); err == nil {
		t.Fatal("expected TeardownAll to surface the volume removal error, got nil")
	}
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

// TestSweepUsesFinishedAtNotCreatedAtForExitedRunner is the F8 guard: the
// exited-runner grace is measured from State.FinishedAt, NOT allocation
// creation. A long-running job whose allocation was created long ago but which
// only JUST exited must not be swept — it is inside the (short) exited grace
// despite its old created-at.
func TestSweepUsesFinishedAtNotCreatedAtForExitedRunner(t *testing.T) {
	m, fake := newManager(t)
	old := time.Now().Add(-time.Hour)
	seedNetworkFor(t, fake, testControllerID, "job-1", old, "n1")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", old, "n1")
	id := seedRunnerFor(t, fake, testControllerID, "job-1", old, "n1", "exited")
	// Override: the runner ran for an hour and only just finished.
	fake.SetFinishedAt(id, time.Now())

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 1 || countNetworks(t, fake) != 1 {
		t.Errorf("sweep tore down a just-finished job (FinishedAt recent) with an old created-at: %d/%d containers/networks",
			countContainers(t, fake), countNetworks(t, fake))
	}
}

// TestSweepSkipsInFlightCreatePastExitedGraceButWithinInflightGrace is the F5
// guard: an allocation with NO runner yet (an in-flight create), older than the
// short exited-runner grace (2m) but younger than the long in-flight-create
// deadline (20m), must NOT be swept — a cold, emulated image pull plus
// credential fetch can legitimately take this long.
func TestSweepSkipsInFlightCreatePastExitedGraceButWithinInflightGrace(t *testing.T) {
	m, fake := newManager(t)
	fiveMinAgo := time.Now().Add(-5 * time.Minute)
	seedNetworkFor(t, fake, testControllerID, "job-1", fiveMinAgo, "n1")
	seedWorkspaceVolumeFor(t, fake, testControllerID, "job-1", fiveMinAgo, "n1")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countNetworks(t, fake) != 1 || countVolumes(t, fake) != 1 {
		t.Errorf("sweep tore down a valid in-flight create (5m old, no runner yet): %d/%d networks/volumes",
			countNetworks(t, fake), countVolumes(t, fake))
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
