package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// newDindTestProvider builds a Provider configured for privileged-sidecar DinD
// (ADR-001), backed by a FakeClient. config.Load's validation is not run on a
// hand-built Config, so the image refs need only be plausible.
func newDindTestProvider(t *testing.T) (*Provider, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:       "unix:///var/run/docker.sock",
		RunnerImage:      "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:         config.DindModePrivilegedSidecar,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar, config.DindModeSysboxRunc},
		DindImage:        "docker:dind@sha256:beefdead",
		StorageDriver:    "overlay2",
		Resources:        config.Resources{DindMemory: "4GiB"},
		Network:          config.Network{EnableJobNetwork: true, Internal: false},
	}
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}
	return p, fake
}

func TestCreateInstanceDinDFullTopology(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance (DinD) returned unexpected error: %v", err)
	}

	name := "Test-Instance-01"

	// Both images are pulled (runner and dind, both missing in the fake).
	if len(fake.PulledImages) != 2 ||
		!slices.Contains(fake.PulledImages, "ghcr.io/example/runner@sha256:deadbeef") ||
		!slices.Contains(fake.PulledImages, "docker:dind@sha256:beefdead") {
		t.Errorf("PulledImages = %v, want the runner and dind images", fake.PulledImages)
	}

	// --- job network (claim marker) ---
	n, ok := netByName(t, fake, spec.JobNetworkName(name))
	if !ok {
		t.Fatalf("job network %q was not created", spec.JobNetworkName(name))
	}
	nonce := n.Labels[spec.LabelCreateNonce]
	if nonce == "" {
		t.Fatal("job network is missing the create-nonce (claim marker)")
	}

	// --- three job-scoped volumes, all sharing the attempt's nonce ---
	for _, tc := range []struct {
		volName  string
		resource string
	}{
		{spec.WorkspaceVolumeName(name), spec.ResourceWorkspace},
		{spec.SocketVolumeName(name), spec.ResourceSocket},
		{spec.DindStateVolumeName(name), spec.ResourceDindState},
	} {
		v, ok := volByName(t, fake, tc.volName)
		if !ok {
			t.Errorf("volume %q was not created", tc.volName)
			continue
		}
		if v.Labels[spec.LabelResource] != tc.resource ||
			v.Labels[spec.LabelInstanceName] != name ||
			v.Labels[spec.LabelCreateNonce] != nonce {
			t.Errorf("volume %q labels missing/incorrect: %v", tc.volName, v.Labels)
		}
	}

	// --- DinD sidecar container ---
	dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName(name))
	if err != nil {
		t.Fatalf("DinD sidecar %q was not created: %v", spec.DindContainerName(name), err)
	}
	if dind.Config.Labels[spec.LabelRole] != spec.RoleDind {
		t.Errorf("sidecar role = %q, want dind", dind.Config.Labels[spec.LabelRole])
	}
	if dind.Config.Labels[spec.LabelInstanceName] != name || dind.Config.Labels[spec.LabelCreateNonce] != nonce {
		t.Errorf("sidecar ownership labels missing/incorrect: %v", dind.Config.Labels)
	}
	// Privileged=true, default runtime (privileged-sidecar via DindRuntimeSelection).
	if dind.HostConfig == nil || !dind.HostConfig.Privileged {
		t.Errorf("sidecar Privileged = %v, want true", dind.HostConfig)
	}
	// The RAW create request set no explicit runtime (privileged-sidecar uses
	// the daemon default); inspect normalizes that to the daemon default (F13),
	// so the meaningful assertion is that no explicit sysbox runtime was set.
	if raw := fake.RawRuntime(spec.DindContainerName(name)); raw != "" {
		t.Errorf("sidecar raw Runtime create request = %q, want empty (default runtime)", raw)
	}
	// dockerd argv carries the explicit storage driver from config and the
	// explicit socket --group (F1).
	wantCmd := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2", "--group=" + spec.DindSocketGID}
	if !slices.Equal([]string(dind.Config.Cmd), wantCmd) {
		t.Errorf("sidecar Cmd = %v, want %v", dind.Config.Cmd, wantCmd)
	}
	// TLS off.
	if !hasEnv(dind.Config.Env, "DOCKER_TLS_CERTDIR=") {
		t.Errorf("sidecar env = %v, want DOCKER_TLS_CERTDIR= (TLS off)", dind.Config.Env)
	}
	// On the SAME job network as the runner.
	if string(dind.HostConfig.NetworkMode) != spec.JobNetworkName(name) {
		t.Errorf("sidecar NetworkMode = %q, want the job network", dind.HostConfig.NetworkMode)
	}
	// Socket at /run and dind-state at /var/lib/docker; no host docker.sock.
	assertContainerMount(t, dind.Mounts, spec.SocketVolumeName(name), spec.DindSocketDir)
	assertContainerMount(t, dind.Mounts, spec.DindStateVolumeName(name), spec.DindStateDir)
	// F2: the runner's workspace volume is ALSO mounted into the sidecar at the
	// runner workdir, so a nested `docker run -v "$PWD":/work` the job issues
	// resolves its bind source (daemon-side) to the real checked-out files.
	assertContainerMount(t, dind.Mounts, spec.WorkspaceVolumeName(name), spec.RunnerWorkDir)
	assertNoHostSocketMount(t, dind.Mounts)

	// --- runner container ---
	runner := inspectRunner(t, fake, name)
	if runner.Config.Labels[spec.LabelRole] != spec.RoleRunner {
		t.Errorf("provider_id container role = %q, want runner", runner.Config.Labels[spec.LabelRole])
	}
	// DOCKER_HOST points at the sidecar socket.
	if !hasEnv(runner.Config.Env, "DOCKER_HOST=unix:///run/docker.sock") {
		t.Errorf("runner env = %v, want DOCKER_HOST=unix:///run/docker.sock", runner.Config.Env)
	}
	// No credential leak (M0 invariant preserved in DinD mode).
	for _, e := range runner.Config.Env {
		if strings.Contains(e, testInstanceToken) || strings.Contains(e, srv.URL) {
			t.Errorf("runner env leaks the token or metadata URL: %q", e)
		}
	}
	// Runner shares the SAME socket volume (at /run) + its workspace; no host socket.
	assertContainerMount(t, runner.Mounts, spec.SocketVolumeName(name), spec.DindSocketDir)
	assertContainerMount(t, runner.Mounts, spec.WorkspaceVolumeName(name), spec.RunnerWorkDir)
	assertNoHostSocketMount(t, runner.Mounts)
	// F1: the runner carries the DinD socket GID as a supplementary group so
	// its unprivileged user can reach the shared dockerd socket.
	if !slices.Contains(runner.HostConfig.GroupAdd, spec.DindSocketGID) {
		t.Errorf("runner GroupAdd = %v, want it to include the DinD socket GID %q", runner.HostConfig.GroupAdd, spec.DindSocketGID)
	}
	// Runner on the job network, with the credential tmpfs.
	if string(runner.HostConfig.NetworkMode) != spec.JobNetworkName(name) {
		t.Errorf("runner NetworkMode = %q, want the job network", runner.HostConfig.NetworkMode)
	}
	if _, ok := fake.TmpfsMounts(spec.RunnerContainerName(name))[spec.CredentialDir]; !ok {
		t.Error("runner is missing the credential tmpfs")
	}

	// Both containers are running; the provider reported running.
	if inst.Status != "running" {
		t.Errorf("Status = %q, want running", inst.Status)
	}
	if dind.State == nil || !dind.State.Running {
		t.Error("DinD sidecar is not running after create")
	}
}

// TestCreateInstanceDinDDeliverFailureRollsBackBothContainers proves the
// nonce-keyed creation guard reclaims BOTH the runner AND the DinD sidecar (plus
// the network and all three volumes) when credential delivery fails — zero
// leftovers of any kind.
func TestCreateInstanceDinDDeliverFailureRollsBackBothContainers(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.ExecErr = errors.New("exec into runner failed")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on credential delivery, got nil")
	}
	// The sidecar was created+started before delivery; the guard must remove it
	// along with the runner, network, and all three volumes.
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceDinDSidecarStartFailureCleansUp exercises the dind-start
// failure branch: the sidecar is started FIRST, so a ContainerStart failure
// trips there, before the runner is ever built. The guard must leave nothing.
func TestCreateInstanceDinDSidecarStartFailureCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.StartErr = errors.New("start boom")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on sidecar start, got nil")
	}
	assertNoLeftovers(t, fake)
	if len(fake.Execs) != 0 {
		t.Errorf("a sidecar-start failure must not deliver credentials, got %d execs", len(fake.Execs))
	}
}

// TestCreateInstanceDinDImagePullFailureCleansUp exercises the dind-image-pull
// failure branch inside startDindSidecar: the runner image is present (so its
// pull is skipped), but the dind image pull fails. The guard must roll back the
// network and all three volumes (no container was created yet).
func TestCreateInstanceDinDImagePullFailureCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.PresentImages["ghcr.io/example/runner@sha256:deadbeef"] = true // runner won't pull
	fake.PullErr = errors.New("dind registry unreachable")              // dind pull fails

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on the dind image pull, got nil")
	}
	assertNoLeftovers(t, fake)
	if n := listAll(t, p); n != 0 {
		t.Errorf("a dind-pull failure created %d containers, want 0", n)
	}
}

// TestCreateInstanceDinDSidecarCreateAmbiguousLeakCleansUp exercises the
// ambiguous dind-create branch: ContainerCreate errors but the daemon still
// recorded the sidecar (ADR-004 F6). Since the leaked sidecar carries this
// attempt's nonce, the nonce-keyed rollback must remove it along with the
// network and volumes. In DinD mode the sidecar is the FIRST container created,
// so CreateErr trips there.
func TestCreateInstanceDinDSidecarCreateAmbiguousLeakCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.CreateErr = errors.New("transient daemon error")
	fake.CreateErrLeaksContainer = true

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on the ambiguous sidecar create, got nil")
	}
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceDinDRunnerCreateFailsAfterSidecarCleansUpSidecar exercises
// the runner-create failure branch AFTER the sidecar is already up: a foreign
// container occupies the runner's Docker name (409 on runner create), while the
// sidecar's distinct name creates fine. The guard must reclaim the sidecar,
// network, and volumes but never touch the foreign container.
func TestCreateInstanceDinDRunnerCreateFailsAfterSidecarCleansUpSidecar(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	// Foreign container occupies the runner name "test-instance-01"; the dind
	// name "test-instance-01-dind" is free, so the sidecar starts, then the
	// runner create hits a 409.
	foreignID := seedRunner(t, fake, "Test-Instance-01", "p1", "different-controller", "running")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on runner create (name conflict), got nil")
	}

	// Foreign container survives.
	if _, err := fake.ContainerInspect(context.Background(), foreignID); err != nil {
		t.Errorf("rollback removed the foreign container: %v", err)
	}
	// The sidecar, network, and all three volumes are rolled back.
	if n := listAll(t, p); n != 1 {
		t.Errorf("container count = %d, want 1 (only the foreign container; sidecar rolled back)", n)
	}
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 0 {
		t.Errorf("rollback left %d networks, want 0", len(nets))
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("rollback left %d volumes, want 0", len(vols.Volumes))
	}
}

// TestCreateInstanceDinDDuplicateReturnsExit31 confirms duplicate detection
// still keys off the claim-marker network in DinD mode: the loser stops before
// pulling the dind image or creating any DinD volume.
func TestCreateInstanceDinDDuplicateReturnsExit31(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	seedClaimNetwork(t, fake, "Test-Instance-01", "peer-nonce")

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected a duplicate error, got nil")
	}
	if !errors.Is(err, gErrors.ErrDuplicateEntity) {
		t.Errorf("error is not a duplicate error: %v", err)
	}
	if code := execcommon.ResolveErrorToExitCode(err); code != execcommon.ExitCodeDuplicate {
		t.Errorf("exit code = %d, want %d (duplicate)", code, execcommon.ExitCodeDuplicate)
	}
	// The loser pulled nothing and created no DinD volumes or containers.
	if len(fake.PulledImages) != 0 {
		t.Errorf("duplicate must not pull an image, got %v", fake.PulledImages)
	}
	if n := listAll(t, p); n != 0 {
		t.Errorf("container count = %d, want 0", n)
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("the losing create left %d volumes, want 0", len(vols.Volumes))
	}
}

// TestDeleteInstanceDinDTearsDownFullTopology drives a real DinD create then
// DeleteInstance, asserting every resource kind (runner + sidecar + network +
// three volumes) is gone in order (the fake's active-endpoint rule enforces
// container-before-network).
func TestDeleteInstanceDinDTearsDownFullTopology(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance (DinD) returned unexpected error: %v", err)
	}
	// Two containers (runner + sidecar) exist.
	if n := listAll(t, p); n != 2 {
		t.Fatalf("container count after create = %d, want 2 (runner + sidecar)", n)
	}

	if err := p.DeleteInstance(context.Background(), inst.Name); err != nil {
		t.Fatalf("DeleteInstance returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)

	// Idempotent repeat → exit 30.
	err = p.DeleteInstance(context.Background(), inst.Name)
	if !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("second DeleteInstance err = %v, want not-found (exit 30)", err)
	}
}

// TestDeleteInstanceReapsSidecarWhenRunnerRemovedOutOfBand is the F6 red-line
// guard: after the runner container is removed out of band (a crash, or an
// external `docker rm`), DeleteInstance by the ORIGINAL provider_id must still
// reap the privileged DinD sidecar, the network, and every volume — not return
// exit 30 and leak them. This is exactly the leak the old provider_id=container-id
// scheme caused: the sidecar/network/volumes are labeled with the instance
// NAME, so a stable name-keyed provider_id (F6) is what makes the label-scoped
// teardown always resolvable, even with the runner gone.
func TestDeleteInstanceReapsSidecarWhenRunnerRemovedOutOfBand(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance (DinD) returned unexpected error: %v", err)
	}
	// Runner + sidecar exist.
	if n := listAll(t, p); n != 2 {
		t.Fatalf("container count after create = %d, want 2 (runner + sidecar)", n)
	}

	// Remove ONLY the runner container out of band, leaving the privileged
	// sidecar, the network, and all volumes behind.
	runnerID := runnerContainerID(t, fake, inst.Name)
	if err := fake.ContainerRemove(context.Background(), runnerID, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("out-of-band runner removal returned unexpected error: %v", err)
	}
	// The sidecar is still present (this is the resource that used to leak).
	if _, err := fake.ContainerInspect(context.Background(), spec.DindContainerName(inst.Name)); err != nil {
		t.Fatalf("precondition: DinD sidecar should still exist after removing only the runner: %v", err)
	}

	// DeleteInstance by the ORIGINAL provider_id (the instance name, F6).
	if err := p.DeleteInstance(context.Background(), inst.ProviderID); err != nil {
		t.Fatalf("DeleteInstance(%q) returned unexpected error: %v", inst.ProviderID, err)
	}

	// The privileged sidecar + network + all volumes must be gone — no leak.
	assertNoLeftovers(t, fake)
}

// TestVerifyRunning covers the shared BOTH-containers-running check's branches
// directly: a running container passes; an exited one and a missing one fail.
func TestVerifyRunning(t *testing.T) {
	p, fake := newDindTestProvider(t)

	runningID := seedRunner(t, fake, "job-run", "p", "controller-abc", "running")
	if err := p.verifyRunning(context.Background(), runningID, "runner", "job-run"); err != nil {
		t.Errorf("verifyRunning(running) = %v, want nil", err)
	}

	exitedID := seedRunner(t, fake, "job-exit", "p", "controller-abc", "exited")
	if err := p.verifyRunning(context.Background(), exitedID, "runner", "job-exit"); err == nil {
		t.Error("verifyRunning(exited) = nil, want an error")
	}

	if err := p.verifyRunning(context.Background(), "no-such-id", "DinD sidecar", "ghost"); err == nil {
		t.Error("verifyRunning(missing) = nil, want an error")
	}
}

// --- helpers -----------------------------------------------------------------

// assertContainerMount asserts a named-volume mount with the given source
// (volume name) and destination exists among an inspected container's mounts.
func assertContainerMount(t *testing.T, mounts []types.MountPoint, source, dest string) {
	t.Helper()
	for _, m := range mounts {
		if m.Name == source && m.Destination == dest {
			return
		}
	}
	t.Errorf("no mount with name %q at %q found in %+v", source, dest, mounts)
}

// assertNoHostSocketMount asserts nothing binds the host Docker socket into the
// container (ADR-001 red line): no mount references /var/run/docker.sock as its
// source/name or destination.
func assertNoHostSocketMount(t *testing.T, mounts []types.MountPoint) {
	t.Helper()
	for _, m := range mounts {
		if strings.Contains(m.Name, "docker.sock") || strings.Contains(m.Destination, "docker.sock") || strings.Contains(string(m.Source), "docker.sock") {
			t.Errorf("a host docker.sock is mounted: %+v", m)
		}
	}
}
