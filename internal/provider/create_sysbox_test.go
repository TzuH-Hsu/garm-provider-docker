package provider

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	gErrors "github.com/cloudbase/garm-provider-common/errors"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// newSysboxTestProvider builds a Provider configured for sysbox-runc DinD
// (ADR-001), backed by a FakeClient. It mirrors newDindTestProvider
// (create_dind_test.go) exactly except for DindMode, so the two modes'
// allocations can be compared directly: the topology built for sysbox-runc
// should differ from privileged-sidecar's in ONLY the two HostConfig fields
// DindRuntimeSelection derives (Privileged, Runtime) — everything else
// (network, three volumes, dockerd Cmd, TLS-off env, mounts, storage driver)
// is the same shared code path (ADR-001: "identical topology to
// privileged-sidecar").
func newSysboxTestProvider(t *testing.T) (*Provider, *docker.FakeClient) {
	t.Helper()
	fake := docker.NewFakeClient()
	cfg := config.Config{
		DockerHost:    "unix:///var/run/docker.sock",
		RunnerImage:   "ghcr.io/example/runner@sha256:deadbeef",
		DindMode:      config.DindModeSysboxRunc,
		DindImage:     "docker:dind@sha256:beefdead",
		StorageDriver: "overlay2",
		Resources:     config.Resources{DindMemory: "4GiB"},
		Network:       config.Network{EnableJobNetwork: true, Internal: false},
	}
	return New(fake, cfg, "controller-abc"), fake
}

// TestCreateInstanceSysboxRuncFullTopology proves WP4's "thin flip" claim
// end to end through the real provider allocation path: a full sysbox-runc
// CreateInstance builds the identical topology TestCreateInstanceDinDFullTopology
// (create_dind_test.go) asserts for privileged-sidecar — job network, three
// job-scoped volumes, dockerd Cmd/TLS-off, socket+dind-state mounts, runner
// DOCKER_HOST/socket mount, both containers running — differing ONLY in the
// sidecar's Privileged/Runtime pair (ADR-001).
func TestCreateInstanceSysboxRuncFullTopology(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newSysboxTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance (sysbox-runc) returned unexpected error: %v", err)
	}

	name := "Test-Instance-01"

	// --- job network (claim marker) ---
	n, ok := netByName(t, fake, spec.JobNetworkName(name))
	if !ok {
		t.Fatalf("job network %q was not created", spec.JobNetworkName(name))
	}
	nonce := n.Labels[spec.LabelCreateNonce]
	if nonce == "" {
		t.Fatal("job network is missing the create-nonce (claim marker)")
	}

	// --- three job-scoped volumes, identical to privileged-sidecar ---
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
		if v.Labels[spec.LabelResource] != tc.resource || v.Labels[spec.LabelCreateNonce] != nonce {
			t.Errorf("volume %q labels missing/incorrect: %v", tc.volName, v.Labels)
		}
	}

	// --- DinD sidecar container: ONLY Privileged/Runtime differ from privileged-sidecar ---
	dind, err := fake.ContainerInspect(context.Background(), spec.DindContainerName(name))
	if err != nil {
		t.Fatalf("DinD sidecar %q was not created: %v", spec.DindContainerName(name), err)
	}
	if dind.HostConfig == nil || dind.HostConfig.Privileged {
		t.Errorf("sidecar Privileged = %v, want false for sysbox-runc", dind.HostConfig)
	}
	if dind.HostConfig.Runtime != "sysbox-runc" {
		t.Errorf("sidecar Runtime = %q, want sysbox-runc", dind.HostConfig.Runtime)
	}
	// Everything else about the sidecar is the SAME shared-code shape as
	// privileged-sidecar: dockerd Cmd (with the config storage driver), TLS
	// off, same job network, same two named-volume mounts, no host socket.
	wantCmd := []string{"dockerd", "--host=unix:///run/docker.sock", "--storage-driver=overlay2"}
	if !slices.Equal([]string(dind.Config.Cmd), wantCmd) {
		t.Errorf("sidecar Cmd = %v, want %v", dind.Config.Cmd, wantCmd)
	}
	if !hasEnv(dind.Config.Env, "DOCKER_TLS_CERTDIR=") {
		t.Errorf("sidecar env = %v, want DOCKER_TLS_CERTDIR= (TLS off)", dind.Config.Env)
	}
	if string(dind.HostConfig.NetworkMode) != spec.JobNetworkName(name) {
		t.Errorf("sidecar NetworkMode = %q, want the job network", dind.HostConfig.NetworkMode)
	}
	assertContainerMount(t, dind.Mounts, spec.SocketVolumeName(name), spec.DindSocketDir)
	assertContainerMount(t, dind.Mounts, spec.DindStateVolumeName(name), spec.DindStateDir)
	assertNoHostSocketMount(t, dind.Mounts)

	// --- runner container: unchanged from privileged-sidecar's shape ---
	runner, err := fake.ContainerInspect(context.Background(), inst.ProviderID)
	if err != nil {
		t.Fatalf("ContainerInspect(runner) returned unexpected error: %v", err)
	}
	if !hasEnv(runner.Config.Env, "DOCKER_HOST=unix:///run/docker.sock") {
		t.Errorf("runner env = %v, want DOCKER_HOST=unix:///run/docker.sock", runner.Config.Env)
	}
	assertContainerMount(t, runner.Mounts, spec.SocketVolumeName(name), spec.DindSocketDir)
	assertContainerMount(t, runner.Mounts, spec.WorkspaceVolumeName(name), spec.RunnerWorkDir)
	assertNoHostSocketMount(t, runner.Mounts)

	if inst.Status != "running" {
		t.Errorf("Status = %q, want running", inst.Status)
	}
	if dind.State == nil || !dind.State.Running {
		t.Error("DinD sidecar is not running after create")
	}
}

// TestCreateInstanceSysboxRuncRuntimeUnavailableCleansUp exercises the
// graceful-failure path this WP adds: a daemon with no sysbox-runc runtime
// registered (the expected state on any host without Sysbox installed —
// this project's own dev daemon included, mirroring the Synology DSM/Unraid
// gap ADR-001 documents) rejects the sidecar's ContainerCreate. The provider
// must surface a CLEAR, actionable error naming dind_mode and the missing
// runtime rather than the raw daemon message, and the creation guard must
// still roll back the network and volumes created before the sidecar —
// zero leftovers of any kind.
func TestCreateInstanceSysboxRuncRuntimeUnavailableCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newSysboxTestProvider(t)
	// The real daemon's observed rejection shape for an unregistered OCI
	// runtime (see internal/provider/dind.go's runtimeUnavailableError).
	fake.CreateErr = errors.New("Unknown runtime specified sysbox-runc")

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected CreateInstance to fail when sysbox-runc is not registered, got nil")
	}
	if !strings.Contains(err.Error(), "dind_mode=sysbox-runc") ||
		!strings.Contains(err.Error(), "sysbox-runc") ||
		!strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %q, want a clear message naming dind_mode=sysbox-runc and the missing runtime", err.Error())
	}
	// The underlying daemon error is still available for diagnostics (wrapped,
	// not discarded).
	if !strings.Contains(err.Error(), "Unknown runtime specified sysbox-runc") {
		t.Errorf("error = %q, want the original daemon error wrapped in, not discarded", err.Error())
	}
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceSysboxRuncRuntimeUnavailableAtStartCleansUp covers the
// same graceful-failure classification when the daemon instead defers the
// unknown-runtime rejection to ContainerStart (a shape that can vary by
// daemon/containerd version) rather than ContainerCreate: the sidecar record
// exists (created, not yet running) when the failure trips, so the guard's
// removal must reclaim it too.
func TestCreateInstanceSysboxRuncRuntimeUnavailableAtStartCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newSysboxTestProvider(t)
	fake.StartErr = errors.New("failed to start container: unknown runtime specified sysbox-runc")

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected CreateInstance to fail when sysbox-runc is not registered, got nil")
	}
	if !strings.Contains(err.Error(), "dind_mode=sysbox-runc") || !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %q, want a clear message naming dind_mode=sysbox-runc and the missing runtime", err.Error())
	}
	assertNoLeftovers(t, fake)
}

// TestCreateInstanceSysboxRuncOtherCreateFailureKeepsGenericMessage confirms
// runtimeUnavailableError does not over-match: a create failure unrelated to
// the runtime (e.g. a transient daemon error) keeps the ordinary wrapped
// message instead of being mislabeled as a missing-runtime failure.
func TestCreateInstanceSysboxRuncOtherCreateFailureKeepsGenericMessage(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newSysboxTestProvider(t)
	fake.CreateErr = errors.New("transient daemon error")

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected CreateInstance to fail, got nil")
	}
	if strings.Contains(err.Error(), "requires the") && strings.Contains(err.Error(), "runtime to be registered") {
		t.Errorf("error = %q, an unrelated create failure must not be mislabeled as a missing runtime", err.Error())
	}
	assertNoLeftovers(t, fake)
}

// TestDeleteInstanceSysboxRuncTearsDownFullTopology drives a real
// sysbox-runc create then DeleteInstance: the teardown/rollback machinery is
// generic (mode-agnostic, ADR-004) and already covered end to end for
// privileged-sidecar (TestDeleteInstanceDinDTearsDownFullTopology); this
// confirms the same guarantee — every resource kind gone, idempotent repeat
// → exit 30 — holds for sysbox-runc's allocation too.
func TestDeleteInstanceSysboxRuncTearsDownFullTopology(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newSysboxTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance (sysbox-runc) returned unexpected error: %v", err)
	}
	if n := listAll(t, p); n != 2 {
		t.Fatalf("container count after create = %d, want 2 (runner + sidecar)", n)
	}

	if err := p.DeleteInstance(context.Background(), inst.Name); err != nil {
		t.Fatalf("DeleteInstance returned unexpected error: %v", err)
	}
	assertNoLeftovers(t, fake)

	err = p.DeleteInstance(context.Background(), inst.Name)
	if !errors.Is(err, gErrors.ErrNotFound) {
		t.Errorf("second DeleteInstance err = %v, want not-found (exit 30)", err)
	}
}
