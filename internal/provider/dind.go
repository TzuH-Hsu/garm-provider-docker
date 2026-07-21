package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// resolveDindMode returns the DinD mode CreateInstance provisions for this
// allocation, and ERRORS if it falls outside the operator's
// allowed_dind_modes ceiling (ADR-001 F7). config.Load's Validate already
// enforces this for the config file's own dind_mode default at load time,
// but CreateInstance calls config.Config.EffectiveDindMode here too,
// defensively, so a misconfiguration fails closed with a clear message even
// for a Config that reached this provider without going through Load/Validate
// (e.g. a hand-built one) — this is the ceiling's single enforcement point on
// the create path.
//
// Per-pool extra_specs mode selection (ADR-005 — a pool requesting a narrower
// mode within allowed_dind_modes) is deferred to M3's extra_specs schema
// validation, exactly like flavor selection is (config.Config.Effective*
// take a flavorName WP3 does not yet thread through either). Until then every
// pool on this host uses the config default (EffectiveDindMode("")); wiring
// a pool's requested mode in later is a one-argument change here, and it
// will be bounded by the SAME ceiling check this method already performs.
func (p *Provider) resolveDindMode() (string, error) {
	mode, err := p.cfg.EffectiveDindMode("")
	if err != nil {
		return "", fmt.Errorf("cannot resolve dind_mode for this allocation: %w", err)
	}
	return mode, nil
}

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
func (p *Provider) startDindSidecar(ctx context.Context, identity spec.AllocationIdentity, nonce, dindMode string, createdAt time.Time) (string, error) {
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

	memoryBytes, err := p.cfg.EffectiveDindMemoryBytes("")
	if err != nil {
		return "", fmt.Errorf("failed to resolve DinD memory limit for %q: %w", instanceName, err)
	}

	labels := identity.DindContainerLabels(createdAt)
	labels[spec.LabelCreateNonce] = nonce

	cfg, hostCfg := spec.BuildDindContainer(spec.DindContainerSpec{
		Image:               dindImage,
		Labels:              labels,
		MemoryBytes:         memoryBytes,
		StorageDriver:       p.cfg.StorageDriver,
		Privileged:          privileged,
		Runtime:             runtime,
		NetworkName:         spec.JobNetworkName(instanceName),
		SocketVolumeName:    spec.SocketVolumeName(instanceName),
		DindStateVolumeName: spec.DindStateVolumeName(instanceName),
		// F2: share the runner's workspace volume into the daemon at the same
		// path, so nested `docker run -v "$PWD":/work` bind sources resolve to
		// the real checked-out files rather than an empty daemon-side path.
		WorkspaceVolumeName: spec.WorkspaceVolumeName(instanceName),
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
// malformed-create rejection. sysbox-runc itself has no install path on this
// project's own dev daemon (macOS/Docker Desktop) any more than on Synology
// DSM/Unraid, so this path is unit-verified only (docker.FakeClient
// simulating the daemon's rejection message) — the real-daemon shape is
// documented here from Docker's own published behavior, not reproduced live.
func runtimeUnavailableError(dindMode, runtime string, err error) error {
	if runtime == "" || err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "runtime") {
		return nil
	}
	if !strings.Contains(msg, "unknown") && !strings.Contains(msg, "not found") && !strings.Contains(msg, "no such") {
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
