package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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

// credentialFetchDeadline is the aggregate bound on fetching all credential
// files from the metadata service (ADR-002 F7). JIT config is valid ~60 min,
// so this is comfortable headroom; the metadata client keeps its bounded
// per-request retries within this deadline.
const credentialFetchDeadline = 60 * time.Second

// cleanupTimeout bounds the creation guard's best-effort teardown. Cleanup
// runs under a context detached from the caller's (context.WithoutCancel), so
// a caller cancellation cannot abort a partial-allocation cleanup; this
// timeout keeps that detached cleanup from hanging (ADR-004 F6).
const cleanupTimeout = 30 * time.Second

// credentialDeliverCmd is the command run inside the started container to
// extract the streamed credential tar into the credential tmpfs. `-p`
// preserves the archive's 0600 file modes; `-C` targets the tmpfs mount.
var credentialDeliverCmd = []string{"tar", "-x", "-p", "-C", spec.CredentialDir}

// CreateInstance provisions a runner container in "none" mode (ADR-001) and
// delivers its credentials without ever exposing them to the container's
// environment or to host disk (ADR-002). The sequence is fetch-first
// (ADR-002 F7) with exec-based delivery (ADR-002 F1):
//
//  1. duplicate check (exit 31 on collision)
//  2. fetch the credentials provider-side, before the container exists, so a
//     slow fetch can never outlive the entrypoint's credential wait
//  3. pull the runner image if missing
//  4. create the container with a credential tmpfs and workspace volume
//  5. start it (its entrypoint blocks waiting for the delivery marker)
//  6. deliver the credentials by streaming an in-memory tar into a
//     `docker exec`-run `tar -x` (docker cp cannot write a running
//     container's user tmpfs — see docker.Client.ExecStream), with an atomic
//     .delivered marker as the tar's last entry
//  7. verify the container is still running before reporting success
//
// Any failure after the container exists triggers a best-effort teardown of
// what was created for this instance (ADR-004 creation guard), so a failed
// create never leaves a partial allocation behind.
func (p *Provider) CreateInstance(ctx context.Context, bootstrap params.BootstrapInstance) (params.ProviderInstance, error) {
	instanceName := bootstrap.Name
	if instanceName == "" {
		return params.ProviderInstance{}, fmt.Errorf("bootstrap instance name is empty")
	}

	// Reject unsupported platforms before ANY Docker operation (ADR F8), so
	// an out-of-scope OS/arch surfaces as a clean provider_fault rather than
	// a partially-created container.
	if err := validatePlatform(bootstrap); err != nil {
		return params.ProviderInstance{}, err
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

	// Fetch credentials FIRST, before the container exists (ADR-002 F7).
	// They never enter the container env, and nothing has been created yet,
	// so a fetch failure leaves nothing to clean up. The in-memory tar is
	// built here too, with the atomic ready marker as its last entry.
	creds, err := p.fetchCredentials(ctx, bootstrap)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to fetch credentials for %q: %w", instanceName, err)
	}
	archive, err := metadata.TarArchive(withReadyMarker(creds))
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to build credential archive for %q: %w", instanceName, err)
	}

	// Pull the image if missing. Still nothing created, so a failure here
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
		// A ContainerCreate error is ambiguous: the daemon may have created
		// the container before failing. Resolve it by its deterministic
		// name, validate it is ours, and remove it so a failed create leaves
		// nothing behind (ADR-004 F6).
		if cerr := p.cleanupAmbiguousCreate(ctx, instanceName); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return params.ProviderInstance{}, fmt.Errorf("failed to create container for %q: %w", instanceName, err)
	}

	// Creation guard: from here, any error best-effort removes the container
	// and its anonymous volumes before returning. Cleanup runs under a
	// context detached from the caller's (context.WithoutCancel) with its own
	// timeout, so a caller cancellation cannot abort it; a cleanup failure is
	// logged and joined onto the primary error rather than swallowed
	// (ADR-004 F6).
	guarded := func(retErr error) (params.ProviderInstance, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := p.bestEffortRemoveContainer(cleanupCtx, created.ID); cerr != nil {
			log.Printf("garm-provider-docker: CreateInstance: cleanup of container %s for %q failed: %v", created.ID, instanceName, cerr)
			retErr = errors.Join(retErr, fmt.Errorf("cleanup of container %s failed: %w", created.ID, cerr))
		}
		return params.ProviderInstance{}, retErr
	}

	// Start it; the entrypoint now blocks waiting for the delivery marker.
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return guarded(fmt.Errorf("failed to start container for %q: %w", instanceName, err))
	}

	// Deliver the credentials into the running container's tmpfs by
	// streaming the in-memory tar into a `docker exec`-run `tar -x`. This is
	// the only moment the credential files exist inside the container, and
	// the path is memory → exec stdin → tmpfs, never touching host disk
	// (ADR-002 F1). docker cp is deliberately not used: it cannot write into
	// a running container's user tmpfs (see docker.Client.ExecStream).
	code, err := p.cli.ExecStream(ctx, created.ID, credentialDeliverCmd, archive)
	if err != nil {
		return guarded(fmt.Errorf("failed to deliver credentials to %q: %w", instanceName, err))
	}
	if code != 0 {
		return guarded(fmt.Errorf("credential delivery exec for %q exited with code %d", instanceName, code))
	}

	// Verify the container is still running before reporting success
	// (ADR-002 F7): a container that exited during or right after delivery
	// must not be reported to GARM as a healthy running instance.
	inspected, err := p.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return guarded(fmt.Errorf("failed to verify container state for %q: %w", instanceName, err))
	}
	if inspected.State == nil || !inspected.State.Running {
		return guarded(fmt.Errorf("container for %q is not running after credential delivery", instanceName))
	}

	// CreateInstance reports running, as the reference providers do
	// (research.md §2.A): the container is up and the runner will register
	// once its entrypoint consumes the delivered credentials.
	return params.ProviderInstance{
		ProviderID: created.ID,
		Name:       instanceName,
		OSType:     bootstrap.OSType,
		OSArch:     bootstrap.OSArch,
		Status:     params.InstanceRunning,
	}, nil
}

// withReadyMarker appends the atomic ready marker (spec.ReadyMarker) as the
// LAST credential entry, so `tar -x` creates it only after every real
// credential file is fully written. The entrypoint waits for exactly this
// marker instead of polling the individual files, closing the window in
// which it could observe a partial credential set (ADR-002 F1).
func withReadyMarker(files []metadata.CredentialFileContent) []metadata.CredentialFileContent {
	out := make([]metadata.CredentialFileContent, 0, len(files)+1)
	out = append(out, files...)
	out = append(out, metadata.CredentialFileContent{Name: spec.ReadyMarker, Bytes: nil})
	return out
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
// service (WP5): the three JIT files, or the non-JIT registration token. The
// whole fetch is bounded by an aggregate deadline (ADR-002 F7) so a stalled
// metadata service fails the create cleanly rather than delaying delivery
// past the entrypoint's own credential wait.
func (p *Provider) fetchCredentials(ctx context.Context, b params.BootstrapInstance) ([]metadata.CredentialFileContent, error) {
	mc, err := metadata.NewClient(b.MetadataURL, b.InstanceToken, b.CACertBundle)
	if err != nil {
		return nil, err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, credentialFetchDeadline)
	defer cancel()

	if b.JitConfigEnabled {
		return mc.FetchJITCredentials(fetchCtx)
	}
	tok, err := mc.FetchRegistrationToken(fetchCtx)
	if err != nil {
		return nil, err
	}
	return []metadata.CredentialFileContent{tok}, nil
}

// bestEffortRemoveContainer force-removes a container and its anonymous
// volumes. It tolerates a NotFound (the container may already be gone) but
// returns any other error so the creation guard can join it onto the primary
// error rather than swallow it (ADR-004 F6).
func (p *Provider) bestEffortRemoveContainer(ctx context.Context, id string) error {
	if err := p.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// cleanupAmbiguousCreate handles the case where ContainerCreate returned an
// error but may still have created the container (ADR-004 F6). It resolves
// the container by its deterministic name, validates that it is an owned
// in-flight runner for exactly this instance, and removes it. A foreign or
// unrelated container that happens to share the name is deliberately left
// untouched. Cleanup runs on a context detached from the caller's, with its
// own timeout.
func (p *Provider) cleanupAmbiguousCreate(ctx context.Context, instanceName string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	name := spec.RunnerContainerName(instanceName)
	inspected, err := p.cli.ContainerInspect(cleanupCtx, name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil // the create really did leave nothing behind
		}
		return fmt.Errorf("failed to resolve possibly-created container %q for cleanup: %w", name, err)
	}

	labels := map[string]string{}
	if inspected.Config != nil {
		labels = inspected.Config.Labels
	}
	if !spec.IsManagedRunner(labels, p.controllerID) || labels[spec.LabelInstanceName] != instanceName {
		return fmt.Errorf("container %q exists but is not an owned in-flight runner for %q; leaving it untouched", inspected.ID, instanceName)
	}

	if err := p.bestEffortRemoveContainer(cleanupCtx, inspected.ID); err != nil {
		return fmt.Errorf("failed to remove ambiguously-created container %s for %q: %w", inspected.ID, instanceName, err)
	}
	return nil
}

// validatePlatform rejects bootstrap payloads outside the M0 target
// (linux/amd64) before any Docker operation, so an unsupported platform
// surfaces as a provider_fault rather than a partially-created container
// (ADR F8).
//
// TODO(M4): accept params.Arm64 once the runner image ships a linux/arm64
// manifest (ADR-002 multi-arch release).
func validatePlatform(b params.BootstrapInstance) error {
	if b.OSType != params.Linux {
		return fmt.Errorf("unsupported OS type %q for instance %q: M0 supports only %q", b.OSType, b.Name, params.Linux)
	}
	if b.OSArch != params.Amd64 {
		return fmt.Errorf("unsupported architecture %q for instance %q: M0 supports only %q", b.OSArch, b.Name, params.Amd64)
	}
	return nil
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
