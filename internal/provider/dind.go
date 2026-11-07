package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// startDindSidecar creates and starts the allocation's DinD sidecar and returns its
// container ID. The runner must already be running and the dind image present. In
// privileged-sidecar mode the sidecar joins runnerID's network namespace, so a port the
// inner daemon publishes is reachable on the runner's 127.0.0.1; in sysbox-runc mode it
// keeps its own namespace on the job network (ADR-001). storageDriver and
// dindMemoryBytes are the values resolved from extra_specs. externalsVolume may be
// empty.
func (p *Provider) startDindSidecar(ctx context.Context, identity spec.AllocationIdentity, nonce, dindMode, storageDriver string, dindMemoryBytes int64, createdAt time.Time, runnerID, externalsVolume string) (string, error) {
	instanceName := identity.InstanceName

	privileged, runtime, err := spec.DindRuntimeSelection(dindMode)
	if err != nil {
		return "", fmt.Errorf("failed to derive DinD runtime for %q: %w", instanceName, err)
	}

	labels := identity.DindContainerLabels(createdAt)
	labels[spec.LabelCreateNonce] = nonce

	dindSpec := spec.DindContainerSpec{
		// Config-only: neither extra_specs nor a flavor can choose the sidecar's image.
		Image:         p.cfg.DindImage,
		Labels:        labels,
		MemoryBytes:   dindMemoryBytes,
		StorageDriver: storageDriver,
		// Config-only, never extra_specs.
		RegistryMirrors:     p.cfg.RegistryMirrors,
		InsecureRegistries:  p.cfg.InsecureRegistries,
		Privileged:          privileged,
		Runtime:             runtime,
		SocketVolumeName:    spec.SocketVolumeName(instanceName, nonce),
		DindStateVolumeName: spec.DindStateVolumeName(instanceName, nonce),
		// Mounted at the runner's path, so the daemon resolves a nested `docker run -v
		// "$PWD":/work` bind source to the checked-out files.
		WorkspaceVolumeName: spec.WorkspaceVolumeName(instanceName, nonce),
		// Same reason for the externals a container job binds as /__e. The runner, created
		// first, pins this volume and has revalidated it, so the sidecar needs no check of
		// its own. Keep that order in sysbox-runc mode too.
		ExternalsVolumeName: externalsVolume,
	}
	if spec.DindSharesRunnerNetns(dindMode) {
		dindSpec.NetnsContainerID = runnerID
	} else {
		dindSpec.NetworkName = spec.JobNetworkName(instanceName)
	}
	cfg, hostCfg := spec.BuildDindContainer(dindSpec)

	if len(p.cfg.RegistryMirrors) > 0 || len(p.cfg.InsecureRegistries) > 0 {
		// Mirror URLs are validated to carry no userinfo, so they are safe to log.
		slog.InfoContext(ctx, "CreateInstance: DinD sidecar registry settings",
			"instance", instanceName,
			"registry_mirrors", p.cfg.RegistryMirrors,
			"insecure_registries", p.cfg.InsecureRegistries)
	}

	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.DindContainerName(instanceName))
	if err != nil {
		// The rollback also removes a sidecar the daemon recorded despite this error.
		if clearErr := runtimeUnavailableError(dindMode, runtime, err); clearErr != nil {
			return "", clearErr
		}
		return "", fmt.Errorf("failed to create DinD sidecar for %q: %w", instanceName, err)
	}
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		// Depending on the daemon and containerd versions, an unregistered runtime can
		// be rejected at start instead of at create.
		if clearErr := runtimeUnavailableError(dindMode, runtime, err); clearErr != nil {
			return "", clearErr
		}
		return "", fmt.Errorf("failed to start DinD sidecar for %q: %w", instanceName, err)
	}
	return created.ID, nil
}

// runtimeUnavailableError returns an actionable error when err looks like the daemon
// rejecting an unregistered OCI runtime, which is the expected failure of sysbox-runc
// mode on a host that cannot install Sysbox. Otherwise it returns nil.
//
// The daemon reports this as a generic 400, "Unknown runtime specified <name>", so only
// the message can tell. It must name the configured runtime and say "unknown runtime",
// which leaves an unrelated OCI failure such as "executable file not found in $PATH" to
// the caller. The match is tested only against docker.FakeClient.
func runtimeUnavailableError(dindMode, runtime string, err error) error {
	if runtime == "" || err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, strings.ToLower(runtime)) {
		return nil
	}
	if !strings.Contains(msg, "unknown runtime") {
		return nil
	}
	return fmt.Errorf("dind_mode=%s requires the %q runtime to be registered on the Docker daemon; it is not available: %w", dindMode, runtime, err)
}

// verifyRunning returns an error unless containerID is running. role names the
// container in that error.
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
