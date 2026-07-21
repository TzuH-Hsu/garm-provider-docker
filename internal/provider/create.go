package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"time"

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

// cleanupTimeout bounds the creation guard's best-effort rollback. Rollback
// runs under a context detached from the caller's (context.WithoutCancel), so
// a caller cancellation cannot abort a partial-allocation cleanup; this
// timeout keeps that detached cleanup from hanging (ADR-004).
const cleanupTimeout = 30 * time.Second

// credentialDeliverCmd is the command run inside the started container to
// extract the streamed credential tar into the credential tmpfs. `-p`
// preserves the archive's 0600 file modes; `-C` targets the tmpfs mount.
var credentialDeliverCmd = []string{"tar", "-x", "-p", "-C", spec.CredentialDir}

// CreateInstance provisions the full per-allocation topology for one runner in
// "none" mode (ADR-001) and delivers its credentials without ever exposing them
// to the container's environment or to host disk (ADR-002). The sequence is the
// ADR-001/ADR-004 allocation flow:
//
//  1. sweep this instance-name's stale leftovers from a crashed prior
//     allocation (claim-marker-aware, grace-windowed), so the idempotent
//     VolumeCreate can never silently reuse a stale workspace volume
//  2. create the CLAIM-MARKER job network FIRST (ADR-004): a labeled bridge
//     network stamped with instance-name, created-at, and this attempt's
//     create-nonce. NetworkCreate's 409 is the atomic duplicate primitive —
//     a same-instance managed collision returns exit 31 here
//  3. create the named, labeled workspace volume
//  4. fetch the credentials provider-side, before the container exists, so a
//     slow fetch can never outlive the entrypoint's credential wait
//  5. pull the runner image if missing
//  6. create the runner container on the job network, with the workspace volume
//     at the runner workdir, a credential tmpfs, and the configured memory limit
//  7. start it, deliver the credentials by streaming an in-memory tar into a
//     `docker exec`-run `tar -x` with an atomic .delivered marker, and verify
//     the container is still running before reporting success
//
// Any error after the claim marker exists triggers a nonce-keyed best-effort
// rollback of THIS attempt's resources (ADR-004 creation guard), so a failed
// create never leaves a partial allocation behind and a concurrent peer's
// resources (a different nonce) are never touched.
func (p *Provider) CreateInstance(ctx context.Context, bootstrap params.BootstrapInstance) (params.ProviderInstance, error) {
	instanceName := bootstrap.Name
	if instanceName == "" {
		return params.ProviderInstance{}, fmt.Errorf("bootstrap instance name is empty")
	}

	// Reject unsupported platforms before ANY Docker operation (ADR F8), so
	// an out-of-scope OS/arch surfaces as a clean provider_fault rather than
	// a partially-created allocation.
	if err := validatePlatform(bootstrap); err != nil {
		return params.ProviderInstance{}, err
	}

	// Opportunistic pre-create sweep (ADR-004): clear a crashed prior
	// allocation's leftovers for THIS instance-name so a stale workspace volume
	// is removed rather than silently reused by the idempotent VolumeCreate. It
	// is grace-windowed, so a concurrent peer's fresh claim is never swept.
	// Best-effort: a sweep failure must not block a legitimate create.
	if err := p.topo.SweepStale(ctx, instanceName); err != nil {
		log.Printf("garm-provider-docker: CreateInstance: pre-create sweep for %q failed (continuing): %v", instanceName, err)
	}

	// enable_job_network=false is reserved and not yet honored (WP2): the job
	// network is ALWAYS created, because it is the isolation guarantee AND
	// ADR-004's claim marker. Warn rather than weaken isolation.
	if !p.cfg.Network.EnableJobNetwork {
		log.Printf("garm-provider-docker: CreateInstance: [network].enable_job_network=false is reserved and not yet honored; creating the isolated per-job network for %q anyway (WP2 keeps job networks always on)", instanceName)
	}

	nonce, err := newCreateNonce()
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to generate create nonce for %q: %w", instanceName, err)
	}

	identity := spec.AllocationIdentity{
		ControllerID: p.controllerID,
		PoolID:       bootstrap.PoolID,
		InstanceName: instanceName,
	}

	// 1. CLAIM-MARKER NETWORK FIRST (ADR-004). A genuine same-instance
	// duplicate returns exit 31; a foreign name collision is a hard error.
	// Nothing of ours exists yet on any error/duplicate here.
	if _, dupErr, err := p.topo.CreateClaimNetwork(ctx, identity, nonce, p.cfg.Network.Internal); dupErr != nil {
		return params.ProviderInstance{}, dupErr
	} else if err != nil {
		return params.ProviderInstance{}, err
	}

	// From here the claim marker exists: any error rolls back THIS attempt's
	// resources (network, volume, container) keyed on the nonce, under a
	// context detached from the caller's with its own timeout, so a caller
	// cancellation cannot abort it. A rollback failure is logged and joined
	// onto the primary error rather than swallowed (ADR-004).
	guarded := func(retErr error) (params.ProviderInstance, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := p.topo.Rollback(cleanupCtx, instanceName, nonce); cerr != nil {
			log.Printf("garm-provider-docker: CreateInstance: rollback for %q failed: %v", instanceName, cerr)
			retErr = errors.Join(retErr, fmt.Errorf("creation-guard rollback for %q failed: %w", instanceName, cerr))
		}
		return params.ProviderInstance{}, retErr
	}

	// 2. Workspace volume (named, labeled, fresh — replaces any stale volume the
	// idempotent VolumeCreate would otherwise return).
	if err := p.topo.CreateWorkspaceVolume(ctx, identity, nonce); err != nil {
		return guarded(err)
	}

	// 3. Fetch credentials (ADR-002 F7): never enter the container env, built
	// into an in-memory tar with the atomic ready marker as its last entry.
	creds, err := p.fetchCredentials(ctx, bootstrap)
	if err != nil {
		return guarded(fmt.Errorf("failed to fetch credentials for %q: %w", instanceName, err))
	}
	archive, err := metadata.TarArchive(withReadyMarker(creds))
	if err != nil {
		return guarded(fmt.Errorf("failed to build credential archive for %q: %w", instanceName, err))
	}

	// 4. Pull the runner image if missing (image comes from config/flavor).
	runnerImage := p.cfg.EffectiveRunnerImage("")
	if err := p.ensureImage(ctx, runnerImage); err != nil {
		return guarded(err)
	}

	// 5. Build the runner container: ownership labels (ADR-004) + informational
	// os labels + this attempt's nonce, the per-mode env (ADR-002), the memory
	// limit (config/flavor), the credential tmpfs, the NAMED workspace volume at
	// the runner workdir, and attachment to the job network as its sole network.
	createdAt := time.Now()
	labels := identity.ContainerLabels(spec.RoleRunner, createdAt)
	labels[spec.LabelOSType] = string(bootstrap.OSType)
	labels[spec.LabelOSArch] = string(bootstrap.OSArch)
	labels[spec.LabelCreateNonce] = nonce

	env, err := buildRunnerEnv(bootstrap)
	if err != nil {
		return guarded(err)
	}

	memoryBytes, err := p.cfg.EffectiveRunnerMemoryBytes("")
	if err != nil {
		return guarded(fmt.Errorf("failed to resolve runner memory limit for %q: %w", instanceName, err))
	}

	cfg, hostCfg := spec.BuildRunnerContainer(spec.RunnerContainerSpec{
		Image:               runnerImage,
		Env:                 env,
		Labels:              labels,
		MemoryBytes:         memoryBytes,
		WorkspaceVolumeName: spec.WorkspaceVolumeName(instanceName),
		NetworkName:         spec.JobNetworkName(instanceName),
	})

	created, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, spec.RunnerContainerName(instanceName))
	if err != nil {
		// We hold the claim network exclusively, so a container-create failure
		// (including an ambiguous daemon leak of a nonce-tagged container) is
		// handled by the nonce-keyed rollback: it removes any container this
		// attempt leaked along with our network and volume, and never touches a
		// stale/foreign container carrying a different nonce.
		return guarded(fmt.Errorf("failed to create container for %q: %w", instanceName, err))
	}

	// 6. Start it; the entrypoint now blocks waiting for the delivery marker.
	if err := p.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return guarded(fmt.Errorf("failed to start container for %q: %w", instanceName, err))
	}

	// 7. Deliver the credentials into the running container's tmpfs by streaming
	// the in-memory tar into a `docker exec`-run `tar -x` (memory → exec stdin →
	// tmpfs, never host disk; docker cp cannot write a running container's user
	// tmpfs — see docker.Client.ExecStream).
	code, err := p.cli.ExecStream(ctx, created.ID, credentialDeliverCmd, archive)
	if err != nil {
		return guarded(fmt.Errorf("failed to deliver credentials to %q: %w", instanceName, err))
	}
	if code != 0 {
		return guarded(fmt.Errorf("credential delivery exec for %q exited with code %d", instanceName, code))
	}

	// 8. Verify the container is still running before reporting success
	// (ADR-002 F7): a container that exited during or right after delivery must
	// not be reported to GARM as a healthy running instance.
	inspected, err := p.cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return guarded(fmt.Errorf("failed to verify container state for %q: %w", instanceName, err))
	}
	if inspected.State == nil || !inspected.State.Running {
		return guarded(fmt.Errorf("container for %q is not running after credential delivery", instanceName))
	}

	// CreateInstance reports running (research.md §2.A): the container is up and
	// the runner registers once its entrypoint consumes the delivered
	// credentials. The provider_id is the runner container's ID (ADR-004).
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
// image comes from provider config/flavor, pulled only when missing). The
// bootstrap image field is informational and deliberately ignored — image
// provenance stays an operator config choice.
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

// newCreateNonce returns a random hex nonce for one CreateInstance attempt
// (ADR-004 claim-marker nonce). crypto/rand makes collisions between two
// concurrent attempts effectively impossible, which is what lets the nonce-keyed
// rollback safely distinguish "the resources I created" from a concurrent
// peer's.
func newCreateNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// validatePlatform rejects bootstrap payloads outside the M0/M1 target
// (linux/amd64) before any Docker operation, so an unsupported platform
// surfaces as a provider_fault rather than a partially-created allocation
// (ADR F8).
//
// TODO(M4): accept params.Arm64 once the runner image ships a linux/arm64
// manifest (ADR-002 multi-arch release).
func validatePlatform(b params.BootstrapInstance) error {
	if b.OSType != params.Linux {
		return fmt.Errorf("unsupported OS type %q for instance %q: only %q is supported", b.OSType, b.Name, params.Linux)
	}
	if b.OSArch != params.Amd64 {
		return fmt.Errorf("unsupported architecture %q for instance %q: only %q is supported", b.OSArch, b.Name, params.Amd64)
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
