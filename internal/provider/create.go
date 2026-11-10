package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/extraspecs"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/metadata"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// credentialFetchDeadline bounds fetching all credential files, the metadata client's
// retries included. JIT configuration stays valid for about an hour.
const credentialFetchDeadline = 60 * time.Second

// cleanupTimeout bounds a rollback or removal that runs under a context detached from
// the caller's, so a cancellation cannot abort it and it cannot hang.
const cleanupTimeout = 30 * time.Second

// credentialDeliverCmd extracts the streamed credential tar into the credential tmpfs.
// -p keeps the archive's 0600 file modes.
var credentialDeliverCmd = []string{"tar", "-x", "-p", "-C", spec.CredentialDir}

// CreateInstance provisions one runner's allocation (job network, volumes, caches,
// runner and, in a DinD mode, a sidecar) and delivers its credentials into the running
// container's tmpfs, so they never reach its environment or host disk (ADR-001,
// ADR-002). It returns the instance name as provider_id.
//
// An instance name that is already claimed returns the duplicate error (exit 31). Any
// later failure rolls back this attempt's resources, selected by its nonce, and leaves
// cache volumes and a concurrent peer's resources alone (ADR-004).
func (p *Provider) CreateInstance(ctx context.Context, bootstrap params.BootstrapInstance) (params.ProviderInstance, error) {
	instanceName := bootstrap.Name
	if instanceName == "" {
		return params.ProviderInstance{}, fmt.Errorf("bootstrap instance name is empty")
	}

	if err := validatePlatform(bootstrap); err != nil {
		return params.ProviderInstance{}, err
	}

	// extra_specs comes from stdin, never GARM_POOL_EXTRASPECS (docs/garm-contract.md).
	// Resolving it first means a payload beyond the operator's limits fails before any
	// Docker operation (ADR-005).
	specs, err := extraspecs.Parse(bootstrap.ExtraSpecs)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("invalid extra_specs for %q: %w", instanceName, err)
	}
	resolved, err := specs.Resolve(p.cfg)
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("invalid extra_specs for %q: %w", instanceName, err)
	}
	dindMode := resolved.DindMode

	// config.Load requires dind_image only when the config's own dind_mode is a DinD
	// mode. A config with dind_mode "none" and a wider allowed_dind_modes can still get
	// a pool that selects one, so check before the first Docker call.
	if dindMode != config.DindModeNone && p.cfg.DindImage == "" {
		return params.ProviderInstance{}, fmt.Errorf(
			"instance %q requests dind_mode %q but no dind_image is configured: set dind_image in the provider config, or narrow allowed_dind_modes so this pool cannot select a DinD mode",
			instanceName, dindMode)
	}

	// A stale allocation of this instance name must go first: its network would make
	// this create a duplicate. A peer's create in progress is protected by the
	// in-flight deadline (ADR-004, Orphan sweep).
	if err := p.topo.SweepOrphans(ctx); err != nil {
		slog.WarnContext(ctx, "CreateInstance: pre-create orphan sweep failed (continuing)",
			"instance", instanceName, "error", err)
	}

	// Best-effort and, unlike the sweep above, throttled.
	p.runCacheGCThrottled(ctx)

	// enable_job_network=false is reserved and not honored: the network is both the
	// isolation boundary and the claim marker, so it is always created.
	if !p.cfg.Network.EnableJobNetwork {
		slog.WarnContext(ctx, "CreateInstance: [network].enable_job_network=false is reserved and not yet honored; creating the isolated per-job network anyway",
			"instance", instanceName)
	}

	nonce, err := newCreateNonce()
	if err != nil {
		return params.ProviderInstance{}, fmt.Errorf("failed to generate create nonce for %q: %w", instanceName, err)
	}

	// Reject a name whose derived volume names would pass the 255-byte limit now,
	// not as an opaque daemon error after the claim network exists.
	if err := spec.ValidateAllocationNames(instanceName, nonce); err != nil {
		return params.ProviderInstance{}, fmt.Errorf("instance name %q produces an invalid Docker resource name: %w", instanceName, err)
	}

	identity := spec.AllocationIdentity{
		ControllerID: p.controllerID,
		PoolID:       bootstrap.PoolID,
		InstanceName: instanceName,
	}

	dindEnabled := dindMode == config.DindModePrivilegedSidecar || dindMode == config.DindModeSysboxRunc

	// The claim network comes first (ADR-004). A failure here needs no rollback:
	// CreateClaimNetwork removes a network it may have leaked, and reports if it could not.
	if _, dupErr, err := p.topo.CreateClaimNetwork(ctx, identity, nonce, p.cfg.Network.Internal); dupErr != nil {
		return params.ProviderInstance{}, dupErr
	} else if err != nil {
		return params.ProviderInstance{}, err
	}

	// From here any error rolls back this attempt's resources by nonce. A rollback
	// failure is joined onto the primary error.
	guarded := func(retErr error) (params.ProviderInstance, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if cerr := p.topo.Rollback(cleanupCtx, instanceName, nonce); cerr != nil {
			slog.ErrorContext(ctx, "CreateInstance: creation-guard rollback failed",
				"instance", instanceName, "nonce", nonce, "error", cerr)
			retErr = errors.Join(retErr, fmt.Errorf("creation-guard rollback for %q failed: %w", instanceName, cerr))
		}
		return params.ProviderInstance{}, retErr
	}

	if err := p.topo.CreateWorkspaceVolume(ctx, identity, nonce); err != nil {
		return guarded(err)
	}

	// The socket volume shares dockerd's socket with the runner; the dind-state volume
	// holds /var/lib/docker, isolated per allocation.
	if dindEnabled {
		if err := p.topo.CreateSocketVolume(ctx, identity, nonce); err != nil {
			return guarded(err)
		}
		if err := p.topo.CreateDindStateVolume(ctx, identity, nonce); err != nil {
			return guarded(err)
		}
	}

	// Before the caches: the toolcache and pnpm volumes are keyed on the OS release
	// read from the image itself (ADR-003).
	runnerImage := resolved.RunnerImage
	if err := p.ensureImage(ctx, runnerImage); err != nil {
		return guarded(err)
	}

	// Cache volumes carry no instance name or nonce, so a rollback leaves them warm for
	// the next job.
	plan, err := p.planCaches(ctx, bootstrap, runnerImage)
	if err != nil {
		return guarded(err)
	}

	// Fetched before the runner exists, so a slow fetch cannot outlive the entrypoint's
	// credential wait.
	creds, err := p.fetchCredentials(ctx, bootstrap)
	if err != nil {
		return guarded(fmt.Errorf("failed to fetch credentials for %q: %w", instanceName, err))
	}
	archive, err := metadata.TarArchive(withReadyMarker(creds))
	if err != nil {
		return guarded(fmt.Errorf("failed to build credential archive for %q: %w", instanceName, err))
	}

	// Every entity scope gets externals: they carry no repository data. A failed seed
	// fails the allocation, but the volume is a cache and is not rolled back.
	if p.cfg.Cache.Enabled {
		externalsVolume, externalsLabels, err := p.planExternals(ctx, runnerImage)
		if err != nil {
			return guarded(fmt.Errorf("failed to prepare externals cache for %q: %w", instanceName, err))
		}
		plan.externalsVolume = externalsVolume
		plan.addCacheRef(externalsVolume, externalsLabels)
	}

	// One timestamp for both containers' created-at labels.
	createdAt := time.Now()

	// Pull before the runner exists, so the pull does not eat into the entrypoint's
	// Docker readiness wait. The socket volume is the runner's only channel to the
	// daemon: never a host docker.sock, never TCP.
	dockerHost := ""
	socketVolumeName := ""
	if dindEnabled {
		if err := p.ensureImage(ctx, p.cfg.DindImage); err != nil {
			return guarded(err)
		}
		dockerHost = spec.DindDockerHost
		socketVolumeName = spec.SocketVolumeName(instanceName, nonce)
	}

	labels := identity.ContainerLabels(spec.RoleRunner, createdAt)
	labels[spec.LabelOSType] = string(bootstrap.OSType)
	labels[spec.LabelOSArch] = string(bootstrap.OSArch)
	labels[spec.LabelCreateNonce] = nonce

	// The entrypoint blocks on this marker, so a half-seeded start is impossible
	// whatever the provider-side timing (ADR-003, Externals).
	externalsSeededMarker := ""
	if plan.externalsVolume != "" {
		externalsSeededMarker = spec.RunnerExternalsSeededMarkerPath
	}
	env, err := buildRunnerEnv(bootstrap, dockerHost, plan.toolcachePath, plan.pnpmPath, plan.diagDir, externalsSeededMarker, resolved.JobHooks, resolved.ExtraRunnerLabels, resolved.ExtraEnv)
	if err != nil {
		return guarded(err)
	}

	memoryBytes := resolved.RunnerMemoryBytes

	cfg, hostCfg := spec.BuildRunnerContainer(spec.RunnerContainerSpec{
		Image:               runnerImage,
		Env:                 env,
		Labels:              labels,
		MemoryBytes:         memoryBytes,
		WorkspaceVolumeName: spec.WorkspaceVolumeName(instanceName, nonce),
		NetworkName:         spec.JobNetworkName(instanceName),
		SocketVolumeName:    socketVolumeName,
		// A cache-ineligible allocation has a path but no volume: RUNNER_TOOL_CACHE
		// points inside the container and nothing persistent is mounted.
		ToolcacheVolumeName: plan.toolcacheVolume,
		ToolcacheMountPath:  plan.toolcachePath,
		PnpmVolumeName:      plan.pnpmVolume,
		PnpmMountPath:       plan.pnpmPath,
		// Externals is mounted read-only. Both are empty with the cache disabled, and
		// diag also for an ineligible scope.
		ExternalsVolumeName: plan.externalsVolume,
		DiagVolumeName:      plan.diagVolume,
	})

	// The entrypoint then blocks waiting for the delivery marker. The rollback also
	// removes a runner that was created but not started.
	runnerID, err := p.createAndStartRunner(ctx, cfg, hostCfg, instanceName, plan)
	if err != nil {
		return guarded(err)
	}

	// The sidecar starts after the runner: in privileged-sidecar mode it joins the
	// runner's network namespace, and the daemon refuses to join one whose owner is not
	// running. Any IPv6 sysctl retry has already happened, so it joins the runner that
	// serves the job.
	var dindID string
	if dindEnabled {
		id, derr := p.startDindSidecar(ctx, identity, nonce, dindMode, resolved.StorageDriver, resolved.DindMemoryBytes, createdAt, runnerID, plan.externalsVolume)
		if derr != nil {
			return guarded(derr)
		}
		dindID = id
	}

	// docker cp cannot write to a running container's user tmpfs, so the tar goes
	// through exec stdin and never touches host disk (ADR-002).
	code, err := p.cli.ExecStream(ctx, runnerID, credentialDeliverCmd, archive)
	if err != nil {
		return guarded(fmt.Errorf("failed to deliver credentials to %q: %w", instanceName, err))
	}
	if code != 0 {
		return guarded(fmt.Errorf("credential delivery exec for %q exited with code %d", instanceName, code))
	}

	// A container that exited during or right after delivery must not be reported
	// running.
	if err := p.verifyRunning(ctx, runnerID, "runner", instanceName); err != nil {
		return guarded(err)
	}
	if dindEnabled {
		if err := p.verifyRunning(ctx, dindID, "DinD sidecar", instanceName); err != nil {
			return guarded(err)
		}
	}

	// After every rollback point, so status reporting can never fail or tear down this
	// allocation.
	p.reportRunnerStatus(ctx, bootstrap, runnerID)

	// provider_id is the instance name, not the container ID: every allocation resource
	// carries that label, so DeleteInstance finds them all even when the runner is
	// already gone (ADR-004, Identity).
	return params.ProviderInstance{
		ProviderID: instanceName,
		Name:       instanceName,
		OSType:     bootstrap.OSType,
		OSArch:     bootstrap.OSArch,
		Status:     params.InstanceRunning,
	}, nil
}

// revalidateReferencedCaches confirms that every cache volume the created runner pins
// still has the identity it was ensured with. The daemon recreates a missing named
// volume empty and unlabelled, so a volume removed since the ensure would otherwise
// reach the runner. A mismatch or an inspect failure returns an error so the allocation
// fails closed. The volume is never deleted, and GARM's retry routes around it
// (ADR-003).
func (p *Provider) revalidateReferencedCaches(ctx context.Context, plan cachePlan) error {
	var bad []string
	for _, ref := range plan.cacheRefs {
		v, err := p.cli.VolumeInspect(ctx, ref.name)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s (inspect failed, not deleting: %v)", ref.name, err))
			continue
		}
		if verr := spec.ValidateAdoptedCacheVolume(ref.name, v.Labels, ref.want); verr != nil {
			slog.WarnContext(ctx, "CreateInstance: referenced cache volume is not our validated cache; leaving it in place as cruft and failing this allocation closed so GARM retries and reconciles around the slot",
				"resource", "cache-volume", "volume", ref.name, "error", verr)
			bad = append(bad, ref.name+" ("+verr.Error()+")")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("cache volume(s) could not be confirmed as our seeded cache during create (a manual-purge/create race or a transient inspect failure); refusing to run the runner against an unverified cache: %s", strings.Join(bad, ", "))
	}
	return nil
}

// bestEffortRemoveContainer force-removes a container under a detached context,
// tolerating NotFound and only logging any other failure.
func (p *Provider) bestEffortRemoveContainer(ctx context.Context, id string) {
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := p.cli.ContainerRemove(rmCtx, id, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		slog.WarnContext(rmCtx, "cache-revalidation rollback: failed to remove runner container (continuing)",
			"resource", "container", "container_id", id, "error", err)
	}
}

// withReadyMarker appends spec.ReadyMarker as the last entry, so tar creates it only
// after every credential file is written and the entrypoint never sees a partial set.
func withReadyMarker(files []metadata.CredentialFileContent) []metadata.CredentialFileContent {
	out := make([]metadata.CredentialFileContent, 0, len(files)+1)
	out = append(out, files...)
	out = append(out, metadata.CredentialFileContent{Name: spec.ReadyMarker, Bytes: nil})
	return out
}

// ensureImage pulls imageRef if it is not present locally.
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

// fetchCredentials fetches the three JIT files, or the non-JIT registration token, from
// GARM's metadata service within credentialFetchDeadline.
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

// newCreateNonce returns a random hex nonce for one CreateInstance attempt. Rollback
// relies on it to tell this attempt's resources from a concurrent peer's.
func newCreateNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// supportedOSArches are the architectures the project ships binaries and images for.
// The provider has no architecture-specific logic: OSArch only labels the container,
// and the daemon resolves a multi-arch image to the host's platform.
var supportedOSArches = []params.OSArch{params.Amd64, params.Arm64}

// validatePlatform rejects a bootstrap payload outside the supported platforms.
func validatePlatform(b params.BootstrapInstance) error {
	if b.OSType != params.Linux {
		return fmt.Errorf("unsupported OS type %q for instance %q: only %q is supported", b.OSType, b.Name, params.Linux)
	}
	if !slices.Contains(supportedOSArches, b.OSArch) {
		return fmt.Errorf("unsupported architecture %q for instance %q: only %v are supported", b.OSArch, b.Name, supportedOSArches)
	}
	return nil
}

// buildRunnerEnv computes the runner's environment (ADR-002, Environment). An empty
// dockerHost, toolCacheDir, pnpmStoreDir, diagDir, externalsSeededMarker or jobHooks omits
// its variable. extraRunnerLabels only feed RUNNER_LABELS, which is emitted without JIT:
// with JIT, GARM bakes the labels into the runner configuration. extraEnv is merged
// last, and provider-injected values win a collision.
func buildRunnerEnv(b params.BootstrapInstance, dockerHost, toolCacheDir, pnpmStoreDir, diagDir, externalsSeededMarker, jobHooks string, extraRunnerLabels []string, extraEnv map[string]string) ([]string, error) {
	labels := b.Labels
	if len(extraRunnerLabels) > 0 {
		labels = append(append([]string{}, b.Labels...), extraRunnerLabels...)
	}
	opts := spec.RunnerEnvOptions{
		JITConfigEnabled: b.JitConfigEnabled,
		GitHubURL:        githubBaseURL(b.RepoURL),
		RunnerWorkDir:    spec.RunnerWorkDir,
		RunnerName:       b.Name,
		RunnerGroup:      b.GitHubRunnerGroup,
		Labels:           labels,
		DockerHost:       dockerHost,
		ToolCacheDir:     toolCacheDir,
		PnpmStoreDir:     pnpmStoreDir,
	}
	if !b.JitConfigEnabled {
		entity, err := spec.ParseEntity(b.RepoURL)
		if err != nil {
			return nil, fmt.Errorf("failed to parse repo_url for non-JIT runner env: %w", err)
		}
		opts.Entity = entity
	}
	env := spec.BuildRunnerEnv(opts)
	// Plain paths, never secrets. They stay out of spec.BuildRunnerEnv, which is
	// structured so that no secret can be emitted.
	if diagDir != "" {
		env = append(env, spec.RunnerDiagDirEnv+"="+diagDir)
	}
	if externalsSeededMarker != "" {
		env = append(env, spec.RunnerExternalsSeededMarkerEnv+"="+externalsSeededMarker)
	}
	if jobHooks != "" {
		env = append(env, spec.RunnerJobHooksEnv+"="+jobHooks)
	}
	return mergeExtraEnv(env, extraEnv), nil
}

// mergeExtraEnv appends extra to env in sorted key order, dropping and logging any key
// env already sets: provider-injected environment always wins. extraspecs has already
// rejected reserved names and enforced allowed_env, so a collision is an allowlisted
// name the provider also injects, such as npm_config_store_dir.
func mergeExtraEnv(env []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return env
	}
	present := make(map[string]bool, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 {
			present[e[:i]] = true
		}
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if present[k] {
			slog.Warn("CreateInstance: extra_specs.extra_env collides with a provider-injected variable; keeping the provider's value",
				"env_key", k)
			continue
		}
		env = append(env, k+"="+extra[k])
	}
	return env
}

// githubBaseURL returns the scheme and host of repoURL, or "" if it is malformed. It
// feeds the informational GITHUB_URL variable, so a bad URL is not an error here.
func githubBaseURL(repoURL string) string {
	u, err := url.Parse(repoURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
