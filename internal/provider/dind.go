package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// effectiveDindMode returns the DinD mode CreateInstance provisions for this
// allocation. WP3: it is the operator's config default. config.Load already
// validated it is one of none/privileged-sidecar/sysbox-runc, is within
// allowed_dind_modes, and that dind_image is set when it is not "none"
// (ADR-001's operator ceiling), so this method can trust it without
// re-validating.
//
// Per-pool extra_specs mode selection (ADR-005 — a pool requesting a narrower
// mode within allowed_dind_modes) is deferred to M3's extra_specs schema
// validation, exactly like flavor selection is (config.Config.Effective*
// take a flavorName WP3 does not yet thread through either). Until then every
// pool on this host uses the config default; this is the single place that
// decision is made, so wiring extra_specs in later is a one-method change.
func (p *Provider) effectiveDindMode() string {
	return p.cfg.DindMode
}

// startDindSidecar pulls the dind image if missing, then builds, creates, and
// starts the DinD sidecar for this allocation (ADR-001), returning its
// container ID. It is called ONLY in DinD modes (dindMode != "none").
//
// The sidecar joins the same per-job network as the runner, mounts the shared
// socket volume at /var/run (where dockerd exposes its unix socket) and the
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
	})

	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.DindContainerName(instanceName))
	if err != nil {
		// We hold the claim network exclusively; a create failure (incl. an
		// ambiguous daemon leak of a nonce-tagged sidecar) is reclaimed by the
		// nonce-keyed rollback, which removes any sidecar this attempt leaked.
		return "", fmt.Errorf("failed to create DinD sidecar for %q: %w", instanceName, err)
	}
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start DinD sidecar for %q: %w", instanceName, err)
	}
	return created.ID, nil
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
