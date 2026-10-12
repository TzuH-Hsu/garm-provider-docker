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

// TestCreateInstanceRejectsDindModeWithoutDindImage covers the pre-Docker
// dind_image gate. config.Load already requires dind_image whenever the
// CONFIG's own dind_mode is non-none, but that check cannot see a pool's
// extra_specs: an operator running dind_mode="none" (so Load never demanded a
// dind_image of them) who has widened allowed_dind_modes can still be handed a
// pool that selects privileged-sidecar. That combination clears the ceiling
// and would otherwise run the sweep, the claim network, the volumes and the
// credential fetch before failing at the sidecar pull.
//
// The assertions are all "nothing happened yet": no container, no image pull,
// no credential exec — i.e. the rejection really is ahead of every Docker op.
func TestCreateInstanceRejectsDindModeWithoutDindImage(t *testing.T) {
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:  "unix:///var/run/docker.sock",
		RunnerImage: "ghcr.io/example/runner@sha256:deadbeef",
		// The operator's own default is none — so config validation never
		// required a dind_image — but the ceiling has been widened.
		DindMode:         config.DindModeNone,
		AllowedDindModes: []string{config.DindModeNone, config.DindModePrivilegedSidecar},
		DindImage:        "",
		StorageDriver:    "overlay2",
		Network:          config.Network{EnableJobNetwork: true},
	}
	p, err := New(fake, cfg, "controller-abc")
	if err != nil {
		t.Fatalf("New returned unexpected error: %v", err)
	}

	// The pool escalates within the (widened) ceiling to a DinD mode.
	b := jitBootstrap("https://metadata.invalid/")
	b.ExtraSpecs = []byte(`{"dind_mode": "privileged-sidecar"}`)

	_, err = p.CreateInstance(context.Background(), b)
	if err == nil {
		t.Fatal("expected a missing-dind_image error, got nil")
	}
	if !strings.Contains(err.Error(), "dind_image") {
		t.Errorf("error %q does not mention dind_image", err)
	}

	if n := listAll(t, p); n != 0 {
		t.Errorf("rejection left %d containers behind, want 0", n)
	}
	if len(fake.PulledImages) != 0 {
		t.Errorf("rejection must not pull an image, got %v", fake.PulledImages)
	}
	if len(fake.Execs) != 0 {
		t.Errorf("rejection must not deliver credentials, got %d execs", len(fake.Execs))
	}
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 0 {
		t.Errorf("rejection created %d networks, want 0 (it must precede the claim marker)", len(nets))
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("rejection created %d volumes, want 0", len(vols.Volumes))
	}
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
		{spec.WorkspaceVolumeName(name, nonce), spec.ResourceWorkspace},
		{spec.SocketVolumeName(name, nonce), spec.ResourceSocket},
		{spec.DindStateVolumeName(name, nonce), spec.ResourceDindState},
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
	// Joins the RUNNER's network namespace (shared netns amendment): no
	// network attachment of its own, and no sysctl of its own.
	runnerForNetns := inspectRunner(t, fake, name)
	if dind.HostConfig.NetworkMode != spec.ContainerNetworkMode(runnerForNetns.ID) {
		t.Errorf("sidecar NetworkMode = %q, want %q (the runner's network namespace)", dind.HostConfig.NetworkMode, spec.ContainerNetworkMode(runnerForNetns.ID))
	}
	if dind.NetworkSettings != nil && len(dind.NetworkSettings.Networks) != 0 {
		t.Errorf("sidecar network attachments = %v, want none of its own", dind.NetworkSettings.Networks)
	}
	if !slices.ContainsFunc(fake.Created, func(c docker.CreatedContainer) bool {
		return c.Name == spec.DindContainerName(name) && len(c.Sysctls) == 0
	}) {
		t.Error("sidecar was created with a sysctl; the runner owns the namespace and its sysctl")
	}
	// The runner is created before the sidecar: the daemon refuses to join
	// the namespace of a container that is not running.
	if got := createdNames(fake); !slices.Equal(got, []string{spec.RunnerContainerName(name), spec.DindContainerName(name)}) {
		t.Errorf("container create order = %v, want [runner sidecar]", got)
	}
	// Socket at /run and dind-state at /var/lib/docker; no host docker.sock.
	assertContainerMount(t, dind.Mounts, spec.SocketVolumeName(name, nonce), spec.DindSocketDir)
	assertContainerMount(t, dind.Mounts, spec.DindStateVolumeName(name, nonce), spec.DindStateDir)
	// F2: the runner's workspace volume is ALSO mounted into the sidecar at the
	// runner workdir, so a nested `docker run -v "$PWD":/work` the job issues
	// resolves its bind source (daemon-side) to the real checked-out files.
	assertContainerMount(t, dind.Mounts, spec.WorkspaceVolumeName(name, nonce), spec.RunnerWorkDir)
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
	assertContainerMount(t, runner.Mounts, spec.SocketVolumeName(name, nonce), spec.DindSocketDir)
	assertContainerMount(t, runner.Mounts, spec.WorkspaceVolumeName(name, nonce), spec.RunnerWorkDir)
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
// failure branch: the runner is created and started first (the sidecar joins
// its network namespace), then the sidecar's ContainerStart fails. The guard
// must remove the started runner, the created sidecar, the network and all
// three volumes, and no credentials may be delivered.
func TestCreateInstanceDinDSidecarStartFailureCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.StartErr = errors.New("start boom")
	fake.StartErrContainerName = spec.DindContainerName("Test-Instance-01")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on sidecar start, got nil")
	} else if !strings.Contains(err.Error(), "failed to start DinD sidecar") {
		t.Errorf("error = %q, want the sidecar start failure", err)
	}
	if got := createdNames(fake); !slices.Equal(got, []string{spec.RunnerContainerName("Test-Instance-01"), spec.DindContainerName("Test-Instance-01")}) {
		t.Errorf("container create order = %v, want [runner sidecar]", got)
	}
	assertNoLeftovers(t, fake)
	if len(fake.Execs) != 0 {
		t.Errorf("a sidecar-start failure must not deliver credentials, got %d execs", len(fake.Execs))
	}
}

// TestCreateInstanceDinDSidecarCannotJoinStoppedRunner pins the start-order
// contract the shared network namespace depends on: the daemon refuses to start
// a container that joins the namespace of a container that is not running. A
// runner that exits between its own start and the sidecar's start therefore
// fails the allocation cleanly, with nothing left behind.
func TestCreateInstanceDinDSidecarCannotJoinStoppedRunner(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	runnerName := spec.RunnerContainerName("Test-Instance-01")
	fake.CreateHook = func() {
		// Fires at the start of every ContainerCreate; the runner already
		// exists and is running when the sidecar's create begins.
		if c, err := fake.ContainerInspect(context.Background(), runnerName); err == nil && c.State != nil && c.State.Running {
			_ = fake.ContainerStop(context.Background(), c.ID, container.StopOptions{})
		}
	}

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected CreateInstance to fail when the runner is not running at sidecar start, got nil")
	}
	if !strings.Contains(err.Error(), "cannot join network namespace of a non running container") {
		t.Errorf("error = %q, want the daemon's refusal to join a stopped runner's namespace", err)
	}
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceDinDImagePullFailureCleansUp exercises the dind-image-pull
// failure branch: the runner image is present (so its pull is skipped), but the
// dind image pull fails. The pull runs before the runner is created, so the
// guard must roll back the network and all three volumes and no container may
// have been created.
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
	if n := len(fake.Created); n != 0 {
		t.Errorf("a dind-pull failure created %d containers, want 0", n)
	}
}

// TestCreateInstanceDinDSidecarCreateAmbiguousLeakCleansUp exercises the
// ambiguous dind-create branch: ContainerCreate errors but the daemon still
// recorded the sidecar (ADR-004 F6). Since the leaked sidecar carries this
// attempt's nonce, the nonce-keyed rollback must remove it along with the
// already-running runner, the network and the volumes.
func TestCreateInstanceDinDSidecarCreateAmbiguousLeakCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.CreateErr = errors.New("transient daemon error")
	fake.CreateErrLeaksContainer = true
	fake.CreateErrContainerName = spec.DindContainerName("Test-Instance-01")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on the ambiguous sidecar create, got nil")
	}
	if got := createdNames(fake); !slices.Contains(got, spec.DindContainerName("Test-Instance-01")) {
		t.Errorf("container creates = %v, want the leaked sidecar among them", got)
	}
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceDinDRunnerCreateFailureNeverCreatesSidecar exercises the
// runner-create failure branch: a foreign container occupies the runner's
// Docker name (409 on runner create). The runner is created before the sidecar,
// so no sidecar may ever be created; the guard must reclaim the network and
// volumes but never touch the foreign container.
func TestCreateInstanceDinDRunnerCreateFailureNeverCreatesSidecar(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	// Foreign container occupies the runner name "test-instance-01".
	foreignID := seedRunner(t, fake, "Test-Instance-01", "p1", "different-controller", "running")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on runner create (name conflict), got nil")
	}

	if slices.Contains(createdNames(fake), spec.DindContainerName("Test-Instance-01")) {
		t.Error("a runner-create failure must not create the sidecar")
	}
	// Foreign container survives.
	if _, err := fake.ContainerInspect(context.Background(), foreignID); err != nil {
		t.Errorf("rollback removed the foreign container: %v", err)
	}
	if n := listAll(t, p); n != 1 {
		t.Errorf("container count = %d, want 1 (only the foreign container)", n)
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

// TestCreateInstanceDinDSidecarJoinsRetriedRunner covers the IPv6 sysctl retry
// in privileged-sidecar mode. The retry recreates the runner before the sidecar
// exists, so the sidecar joins the namespace of the runner that actually
// serves the job (the recreated one), the namespace owner is the only
// container that ever carried the sysctl, and the sidecar carries none.
func TestCreateInstanceDinDSidecarJoinsRetriedRunner(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newDindTestProvider(t)
	fake.HostLacksIPv6 = true

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
		t.Fatalf("CreateInstance (DinD, host without IPv6) returned unexpected error: %v", err)
	}
	name := "Test-Instance-01"
	runner := inspectRunner(t, fake, name)
	dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName(name))
	if err != nil {
		t.Fatalf("DinD sidecar was not created: %v", err)
	}
	if dind.HostConfig.NetworkMode != spec.ContainerNetworkMode(runner.ID) {
		t.Errorf("sidecar NetworkMode = %q, want %q (the recreated runner)", dind.HostConfig.NetworkMode, spec.ContainerNetworkMode(runner.ID))
	}
	if dind.State == nil || !dind.State.Running {
		t.Error("DinD sidecar is not running after create")
	}
	want := []string{spec.RunnerContainerName(name), spec.RunnerContainerName(name), spec.DindContainerName(name)}
	if got := createdNames(fake); !slices.Equal(got, want) {
		t.Errorf("container create order = %v, want %v (runner, retried runner, then sidecar)", got, want)
	}
	for _, c := range fake.Created {
		if c.Name == spec.DindContainerName(name) && len(c.Sysctls) != 0 {
			t.Errorf("sidecar created with sysctls %v, want none", c.Sysctls)
		}
	}
}

// createdNames returns the names of every container the fake was asked to
// create, in call order.
func createdNames(fake *docker.FakeClient) []string {
	out := make([]string, 0, len(fake.Created))
	for _, c := range fake.Created {
		out = append(out, c.Name)
	}
	return out
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

	runner := inspectRunner(t, fake, inst.Name)
	dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName(inst.Name))
	if err != nil {
		t.Fatalf("DinD sidecar was not created: %v", err)
	}
	if dind.HostConfig.NetworkMode != spec.ContainerNetworkMode(runner.ID) {
		t.Fatalf("sidecar NetworkMode = %q, want the runner's namespace", dind.HostConfig.NetworkMode)
	}

	if err := p.DeleteInstance(context.Background(), inst.Name); err != nil {
		t.Fatalf("DeleteInstance returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)
	// ADR-004 order is unchanged by the shared namespace: the runner (the
	// namespace owner) goes first, then the sidecar that joined it, then the
	// volumes, and the network last. The sidecar holds no endpoint on the
	// network, so the network removal does not depend on it.
	if len(fake.RemoveOrder) < 3 ||
		fake.RemoveOrder[0] != "container:"+runner.ID ||
		fake.RemoveOrder[1] != "container:"+dind.ID ||
		!strings.HasPrefix(fake.RemoveOrder[len(fake.RemoveOrder)-1], "network:") {
		t.Errorf("RemoveOrder = %v, want runner, sidecar, ..., network last", fake.RemoveOrder)
	}

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
