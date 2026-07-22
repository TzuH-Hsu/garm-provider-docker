package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// startDindSidecar pulls the dind image if missing, then builds, creates, and
// starts the DinD sidecar for this allocation (ADR-001), returning its
// container ID. It is called ONLY in DinD modes (dindMode != "none").
//
// The sidecar joins the same per-job network as the runner, mounts the shared
// socket volume at /run (where dockerd exposes its unix socket) and the
// dind-state volume at /var/lib/docker, runs `dockerd --host=unix://...
// --storage-driver=<config>` with TLS off, and carries this attempt's
// create-nonce so the creation-guard rollback (nonce-keyed) reclaims it on any
// later failure. Its Privileged/Runtime pair comes from
// spec.DindRuntimeSelection — Privileged=true for privileged-sidecar — so
// WP4's sysbox-runc support is a thin flip with no change here.
//
// storageDriver and dindMemoryBytes are the extra_specs-resolved values
// (resolved.StorageDriver / resolved.DindMemoryBytes): the pool's overrides
// bounded against the operator's config (storage_driver enum, dind_memory
// ceiling), or the config defaults when extra_specs sets neither.
func (p *Provider) startDindSidecar(ctx context.Context, identity spec.AllocationIdentity, nonce, dindMode, storageDriver string, dindMemoryBytes int64, createdAt time.Time) (string, error) {
	instanceName := identity.InstanceName

	privileged, runtime, err := spec.DindRuntimeSelection(dindMode)
	if err != nil {
		return "", fmt.Errorf("failed to derive DinD runtime for %q: %w", instanceName, err)
	}

	// dind_image is config-only (ADR-002: no flavor/extra_specs image channel
	// for the sidecar) and validated digest-pinned + non-empty for any non-none
	// mode at config load.
	dindImage := p.cfg.DindImage
	if err := p.ensureImage(ctx, dindImage); err != nil {
		return "", err
	}

	labels := identity.DindContainerLabels(createdAt)
	labels[spec.LabelCreateNonce] = nonce

	cfg, hostCfg := spec.BuildDindContainer(spec.DindContainerSpec{
		Image:               dindImage,
		Labels:              labels,
		MemoryBytes:         dindMemoryBytes,
		StorageDriver:       storageDriver,
		Privileged:          privileged,
		Runtime:             runtime,
		NetworkName:         spec.JobNetworkName(instanceName),
		SocketVolumeName:    spec.SocketVolumeName(instanceName, nonce),
		DindStateVolumeName: spec.DindStateVolumeName(instanceName, nonce),
		// F2: share the runner's workspace volume into the daemon at the same
		// path, so nested `docker run -v "$PWD":/work` bind sources resolve to
		// the real checked-out files rather than an empty daemon-side path.
		WorkspaceVolumeName: spec.WorkspaceVolumeName(instanceName, nonce),
	})

	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.DindContainerName(instanceName))
	if err != nil {
		// We hold the claim network exclusively; a create failure (incl. an
		// ambiguous daemon leak of a nonce-tagged sidecar) is reclaimed by the
		// nonce-keyed rollback, which removes any sidecar this attempt leaked.
		if clearErr := runtimeUnavailableError(dindMode, runtime, err); clearErr != nil {
			return "", clearErr
		}
		return "", fmt.Errorf("failed to create DinD sidecar for %q: %w", instanceName, err)
	}
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// A daemon that accepts an unregistered Runtime at create time (the
		// commonly observed shape — "Unknown runtime specified <name>",
		// rejected before a container is ever recorded) can still, depending
		// on daemon/containerd version, defer that rejection to start instead;
		// classify here too so either shape gets the same clear message.
		if clearErr := runtimeUnavailableError(dindMode, runtime, err); clearErr != nil {
			return "", clearErr
		}
		return "", fmt.Errorf("failed to start DinD sidecar for %q: %w", instanceName, err)
	}
	return created.ID, nil
}

// runtimeUnavailableError returns a clear, actionable provider error when err
// looks like the daemon rejecting an unregistered OCI runtime (observed real
// -daemon shape: "Unknown runtime specified <name>", returned e.g. for
// Runtime="sysbox-runc" when the daemon's `runtimes` config — daemon.json —
// has no such entry; see docs/research.md §3.C: Sysbox has no supported
// install path on Synology DSM or Unraid, so this is the EXPECTED failure
// mode there). It returns nil when runtime is empty (privileged-sidecar sets
// no Runtime, so this never applies to it) or err does not look
// runtime-shaped, in which case the caller falls through to its own generic
// wrapping.
//
// This is a message-content heuristic, not an errdefs classification: the
// real daemon reports an unknown runtime as a generic 400 (invalid
// parameter), indistinguishable by status/error-kind alone from any other
// malformed-create rejection. To avoid MISdiagnosing an unrelated OCI failure
// as a missing runtime (F12) — e.g. `OCI runtime create failed: … executable
// file not found in $PATH`, which is the container entrypoint missing, not the
// runtime — it requires BOTH signals: the message must name the CONFIGURED
// runtime (e.g. "sysbox-runc") AND carry the canonical daemon missing-runtime
// phrase "unknown runtime". An "executable file not found" error names neither,
// so it is left to the caller's generic wrapping.
//
// sysbox-runc itself has no install path on this project's own dev daemon
// (macOS/Docker Desktop) any more than on Synology DSM/Unraid, so this path is
// unit-verified only (docker.FakeClient simulating the daemon's rejection
// message) — the real-daemon shape is documented here from Docker's own
// published behavior, not reproduced live.
func runtimeUnavailableError(dindMode, runtime string, err error) error {
	if runtime == "" || err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	// Must name the CONFIGURED runtime — an OCI error about some other thing
	// (a missing entrypoint binary, say) does not, so it is not misdiagnosed.
	if !strings.Contains(msg, strings.ToLower(runtime)) {
		return nil
	}
	// AND carry the canonical daemon phrase for an unregistered runtime.
	if !strings.Contains(msg, "unknown runtime") {
		return nil
	}
	return fmt.Errorf("dind_mode=%s requires the %q runtime to be registered on the Docker daemon; it is not available: %w", dindMode, runtime, err)
}

// verifyRunning inspects containerID and errors unless it is running — used to
// confirm both the runner AND the DinD sidecar are healthy before reporting
// success to GARM (ADR-001: "verify BOTH containers running"). role names the
// container for the error message ("runner" / "DinD sidecar").
func (p *Provider) verifyRunning(ctx context.Context, containerID, role, instanceName string) error {
	inspected, err := p.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("failed to verify %s state for %q: %w", role, instanceName, err)
	}
	if inspected.State == nil || !inspected.State.Running {
		return fmt.Errorf("%s for %q is not running after credential delivery", role, instanceName)
	}
	return nil
}
