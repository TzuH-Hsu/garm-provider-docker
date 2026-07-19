package provider

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"time"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/metadata"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// CreateInstance provisions a runner container in "none" mode (ADR-001) and
// delivers its credentials without ever exposing them to the container's
// environment or to host disk (ADR-002). The sequence is exactly ADR-002's
// create → start → poll → docker cp:
//
//  1. duplicate check (exit 31 on collision)
//  2. pull the runner image if missing
//  3. create the container with a credential tmpfs and workspace volume
//  4. start it (its entrypoint blocks polling the tmpfs for credentials)
//  5. fetch the credentials provider-side (WP5)
//  6. stream them into the running container's tmpfs via an in-memory tar
//
// Any failure after the container exists triggers a best-effort teardown of
// what was created for this instance (ADR-004 creation guard), so a failed
// create never leaves a partial allocation behind.
func (p *Provider) CreateInstance(ctx context.Context, bootstrap params.BootstrapInstance) (params.ProviderInstance, error) {
	instanceName := bootstrap.Name
	if instanceName == "" {
		return params.ProviderInstance{}, fmt.Errorf("bootstrap instance name is empty")
	}

	// TODO(M1): run the opportunistic orphan sweep here (ADR-004). M0 "none"
	// mode has no job networks/volumes to sweep beyond the container itself.

	// Duplicate detection (exit 31). M0 keys off an existing managed runner
	// container with this instance-name.
	// TODO(M1): key off ADR-004's atomic claim marker (the job network)
	// instead, so a concurrent, still-in-flight CreateInstance for the same
	// name — which may not have a runner container yet — is also detected.
	existing, err := p.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: p.managedByInstanceNameFilter(instanceName),
	})
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to check for an existing instance %q: %w", instanceName, err)
	}
	if len(existing) > 0 {
		// gErrors.ErrDuplicateEntity maps to exit 31 via
		// execution.ResolveErrorToExitCode.
		return params.ProviderInstance{}, gErrors.NewDuplicateUserError(fmt.Sprintf("instance %q already exists", instanceName))
	}

	// Pull the image if missing. No resources exist yet, so a failure here
	// leaves nothing to clean up.
	if err := p.ensureImage(ctx, p.cfg.RunnerImage); err != nil {
		return params.ProviderInstance{}, err
	}

	// Build the container config: ownership labels (ADR-004) + informational
	// os labels, the per-mode env (ADR-002), and the credential tmpfs +
	// workspace mounts.
	createdAt := time.Now()
	identity := spec.AllocationIdentity{
		ControllerID: p.controllerID,
		PoolID:       bootstrap.PoolID,
		InstanceName: instanceName,
	}
	labels := identity.ContainerLabels(spec.RoleRunner, createdAt)
	labels[spec.LabelOSType] = string(bootstrap.OSType)
	labels[spec.LabelOSArch] = string(bootstrap.OSArch)

	env, err := buildRunnerEnv(bootstrap)
	if err != nil {
		return params.ProviderInstance{}, err
	}

	cfg, hostCfg := spec.BuildRunnerContainer(spec.RunnerContainerSpec{
		Image:  p.cfg.RunnerImage,
		Env:    env,
		Labels: labels,
	})

	// Create (status created, not yet started).
	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.RunnerContainerName(instanceName))
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to create container for %q: %w", instanceName, err)
	}

	// Creation guard: from here, any error best-effort removes the container
	// and its anonymous volumes before returning (ADR-004).
	guarded := func(retErr error) (params.ProviderInstance, error) {
		p.bestEffortRemoveContainer(ctx, created.ID)
		return params.ProviderInstance{}, retErr
	}

	// Start it; the entrypoint now blocks polling the credential tmpfs.
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return guarded(fmt.Errorf("failed to start container for %q: %w", instanceName, err))
	}

	// Fetch credentials provider-side. They never enter the container env.
	creds, err := p.fetchCredentials(ctx, bootstrap)
	if err != nil {
		return guarded(fmt.Errorf("failed to fetch credentials for %q: %w", instanceName, err))
	}

	// Stream them into the running container's tmpfs via an in-memory tar.
	// This is the only moment the credential files exist anywhere, and the
	// path is memory → container, never touching host disk (ADR-002).
	archive, err := metadata.TarArchive(creds)
	if err != nil {
		return guarded(fmt.Errorf("failed to build credential archive for %q: %w", instanceName, err))
	}
	if err := p.cli.CopyToContainer(ctx, created.ID, spec.CredentialDir, archive, container.CopyToContainerOptions{}); err != nil {
		return guarded(fmt.Errorf("failed to deliver credentials to %q: %w", instanceName, err))
	}

	// CreateInstance reports running immediately, as the reference providers
	// do (research.md §2.A): the container is up and the runner will register
	// once its entrypoint consumes the delivered credentials.
	return params.ProviderInstance{
		ProviderID: created.ID,
		Name:       instanceName,
		OSType:     bootstrap.OSType,
		OSArch:     bootstrap.OSArch,
		Status:     params.InstanceRunning,
	}, nil
}

// ensureImage pulls imageRef if it is not already present locally (ADR-002:
// image comes from provider config, pulled only when missing). The bootstrap
// image field is informational and deliberately ignored — image provenance
// stays an operator config choice.
func (p *Provider) ensureImage(ctx context.Context, imageRef string) error {
	if _, _, err := p.cli.ImageInspectWithRaw(ctx, imageRef); err == nil {
		return nil // already present
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("failed to inspect image %q: %w", imageRef, err)
	}

	rc, err := p.cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %q: %w", imageRef, err)
	}
	defer rc.Close()
	// The pull only finishes once its progress stream is fully drained.
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("failed to complete pull of image %q: %w", imageRef, err)
	}
	return nil
}

// fetchCredentials fetches the runner's credentials from GARM's metadata
// service (WP5): the three JIT files, or the non-JIT registration token.
func (p *Provider) fetchCredentials(ctx context.Context, b params.BootstrapInstance) ([]metadata.CredentialFileContent, error) {
	mc, err := metadata.NewClient(b.MetadataURL, b.InstanceToken, b.CACertBundle)
	if err != nil {
		return nil, err
	}
	if b.JitConfigEnabled {
		return mc.FetchJITCredentials(ctx)
	}
	tok, err := mc.FetchRegistrationToken(ctx)
	if err != nil {
		return nil, err
	}
	return []metadata.CredentialFileContent{tok}, nil
}

// bestEffortRemoveContainer force-removes a container and its anonymous
// volumes, swallowing every error. It is only called from the creation
// guard, where the goal is to leave nothing behind, not to surface a
// secondary cleanup failure over the primary error being returned.
func (p *Provider) bestEffortRemoveContainer(ctx context.Context, id string) {
	_ = p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true})
}

// buildRunnerEnv computes the runner container's environment per ADR-002's
// per-delivery-mode contract. The entity scope is only needed (and only
// parsed) in non-JIT mode.
func buildRunnerEnv(b params.BootstrapInstance) ([]string, error) {
	opts := spec.RunnerEnvOptions{
		JITConfigEnabled: b.JitConfigEnabled,
		GitHubURL:        githubBaseURL(b.RepoURL),
		RunnerWorkDir:    spec.RunnerWorkDir,
		RunnerName:       b.Name,
		RunnerGroup:      b.GitHubRunnerGroup,
		Labels:           b.Labels,
	}
	if !b.JitConfigEnabled {
		entity, err := spec.ParseEntity(b.RepoURL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse repo_url for non-JIT runner env: %w", err)
		}
		opts.Entity = entity
	}
	return spec.BuildRunnerEnv(opts), nil
}

// githubBaseURL derives the GitHub server base (scheme://host) from a
// repo/org/enterprise URL, for the informational GITHUB_URL env var. It is
// best-effort: a malformed repo_url yields an empty base rather than an
// error, since GITHUB_URL is only a connectivity-check hint (ADR-002).
func githubBaseURL(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
