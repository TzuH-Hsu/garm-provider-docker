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

	"github.com/TzuH-Hsu/garm-provider-docker/internal/config"
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

// CreateInstance provisions the full per-allocation topology for one runner —
// in "none" mode or, when dind_mode is a DinD mode (privileged-sidecar, or
// WP4's sysbox-runc — the two share this entire flow, differing only in the
// sidecar's Privileged/Runtime pair, ADR-001), with an isolated DinD sidecar —
// and delivers the runner's credentials without ever exposing them to the
// container's environment or to host disk (ADR-002). Two checks run before
// ANY Docker operation and fail closed on their own: platform support, and
// (WP4) resolving dind_mode within the operator's allowed_dind_modes ceiling
// (ADR-001 F7) — a misconfiguration here is rejected before a network or
// volume ever exists for the attempt. After those, the sequence is the
// ADR-001/ADR-004 allocation flow:
//
//  1. run the host-wide, controller-scoped orphan sweep (F9): collect every
//     abandoned allocation past its grace window (a crashed prior allocation of
//     THIS instance-name included, so the idempotent VolumeCreate can never
//     silently reuse a stale volume), while a peer's in-flight create is left
//     untouched by F5's long in-flight-create deadline
//  2. create the CLAIM-MARKER job network FIRST (ADR-004): a labeled bridge
//     network stamped with instance-name, created-at, and this attempt's
//     create-nonce. NetworkCreate's 409 is the atomic duplicate primitive —
//     a same-instance managed collision returns exit 31 here
//  3. create the named, labeled workspace volume — and, in DinD modes, the
//     socket and dind-state volumes alongside it
//  4. fetch the credentials provider-side, before the container exists, so a
//     slow fetch can never outlive the entrypoint's credential wait
//  5. pull the runner image if missing
//  6. in DinD modes, pull the dind image and start the sidecar (privileged-
//     sidecar or, WP4, sysbox-runc — the two differ only in Privileged/
//     Runtime) on the job network FIRST, so the runner's entrypoint has a
//     daemon to reach over the shared socket volume (its `until docker info`
//     wait blocks on it)
//  7. create the runner container on the job network, with the workspace volume
//     at the runner workdir, a credential tmpfs, the configured memory limit,
//     and — in DinD modes — DOCKER_HOST plus the shared socket volume mount
//  8. start it, deliver the credentials by streaming an in-memory tar into a
//     `docker exec`-run `tar -x` with an atomic .delivered marker, and verify
//     BOTH the runner and (in DinD modes) the sidecar are running before
//     reporting success
//
// Any error after the claim marker exists triggers a nonce-keyed best-effort
// rollback of THIS attempt's resources — network, all volumes, the runner AND
// the DinD sidecar (ADR-004 creation guard) — so a failed create never leaves a
// partial allocation behind and a concurrent peer's resources (a different
// nonce) are never touched.
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

	// Resolve dind_mode before ANY Docker operation too (ADR-001 F7): a
	// misconfiguration that would escalate past the operator's
	// allowed_dind_modes ceiling must fail closed with a clear message, not
	// after a network/volume has already been created for the attempt.
	dindMode, err := p.resolveDindMode()
	if err != nil {
		return params.ProviderInstance{}, err
	}

	// Opportunistic pre-create orphan sweep (ADR-004, F9): CreateInstance is one
	// of the two host-wide sweep hooks (the other being ListInstances), so it
	// runs the FULL controller-scoped sweep — not just this instance-name — to
	// collect abandoned allocations across the host. This is safe under F5's
	// split grace: a peer's still-in-progress create (no runner yet, within the
	// long in-flight-create deadline) is never swept, while a genuinely stale
	// prior allocation of THIS instance-name is cleared so a stale workspace
	// volume is removed rather than silently reused by the idempotent
	// VolumeCreate. Best-effort: a sweep failure must not block a legitimate
	// create.
	if err := p.topo.SweepOrphans(ctx); err != nil {
		log.Printf("garm-provider-docker: CreateInstance: pre-create orphan sweep failed (continuing): %v", err)
	}

	// Opportunistic, best-effort cache GC (ADR-003 W2): CreateInstance is one of
	// the two piggyback hooks (the other being ListInstances) where superseded/
	// aged cache volumes are evicted, diagnostic logs pruned to their retention
	// window, and leaked helper containers reaped — there is no background daemon
	// (ADR-004). It never hard-fails the create: every step logs and continues.
	p.runCacheGC(ctx)

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

	// Fail EARLY — before this attempt's first Docker call — if instanceName
	// is long enough (or charset-unsafe enough) that a derived name would be
	// rejected by the daemon: the generation-nonce-qualified volume names
	// (F4) add a fixed ~44 bytes on top of the instance name, so a
	// pathologically long GARM instance name could otherwise push a volume
	// name past Docker's 255-byte resource-name limit and surface as an
	// opaque daemon rejection deep inside VolumeCreate, after the claim
	// network (and possibly other resources) already exist.
	if err := spec.ValidateAllocationNames(instanceName, nonce); err != nil {
		return params.ProviderInstance{}, fmt.Errorf("instance name %q produces an invalid Docker resource name: %w", instanceName, err)
	}

	identity := spec.AllocationIdentity{
		ControllerID: p.controllerID,
		PoolID:       bootstrap.PoolID,
		InstanceName: instanceName,
	}

	// The effective DinD mode (resolved and ceiling-checked above) decides
	// whether this allocation gets the full isolated DinD topology (socket +
	// dind-state volumes and a sidecar) or the none-mode flow. dindEnabled is
	// an explicit positive test for the two DinD modes, so "none" — and any
	// unset/empty mode from a hand-built config that skipped config.Load's
	// defaulting — takes the none-mode path, byte-for-byte the M0/WP2 flow.
	dindEnabled := dindMode == config.DindModePrivilegedSidecar || dindMode == config.DindModeSysboxRunc

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

	// 2b. DinD volumes (DinD modes only): the shared socket volume (dockerd's
	// unix socket, shared with the runner) and the dind-state volume
	// (/var/lib/docker, isolated per allocation), created alongside the
	// workspace volume (ADR-001). Both carry this attempt's nonce, so the guard
	// rolls them back on any later failure.
	if dindEnabled {
		if err := p.topo.CreateSocketVolume(ctx, identity, nonce); err != nil {
			return guarded(err)
		}
		if err := p.topo.CreateDindStateVolume(ctx, identity, nonce); err != nil {
			return guarded(err)
		}
	}

	// 2c. Persistent, repo-scoped caches (ADR-003): resolve entity scope and,
	// when eligible, create-or-reuse the toolcache and pnpm store volumes.
	// Cache volumes carry no instance-name/create-nonce, so the creation-guard
	// rollback never removes them — a create that fails after this point leaves
	// the warm cache intact for the next job. An org/enterprise pool without
	// allow_org_shared (or a disabled cache) gets no persistent cache volumes.
	plan, err := p.planCaches(ctx, bootstrap)
	if err != nil {
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

	// 4b. Externals cache (ADR-003 W2): resolve the runner image's digest,
	// create-or-reuse the shared externals volume keyed on it, and SEED it once
	// (blocking) so the runner mounts a fully-populated, read-only externals
	// tree. Gated only on the cache feature being enabled — externals apply to
	// EVERY allocation regardless of entity scope, since they carry no repo data.
	// It runs AFTER ensureImage so the image is present as both the digest source
	// and the seed's copy source. A seed failure fails the allocation (guarded);
	// the externals volume itself is never rolled back (it is a cache).
	if p.cfg.Cache.Enabled {
		externalsVolume, err := p.planExternals(ctx, runnerImage)
		if err != nil {
			return guarded(fmt.Errorf("failed to prepare externals cache for %q: %w", instanceName, err))
		}
		plan.externalsVolume = externalsVolume
	}

	// One timestamp for both containers' created-at labels this allocation.
	createdAt := time.Now()

	// 5. DinD sidecar (DinD modes only): pull the dind image, then create and
	// start the privileged sidecar on the job network BEFORE the runner, so the
	// runner's entrypoint (which blocks on `until docker info`) has a daemon to
	// reach over the shared socket volume (ADR-001). The runner then gets
	// DOCKER_HOST pointed at that socket and mounts the same socket volume as
	// its SOLE channel to the daemon — never a host docker.sock, never TCP.
	dockerHost := ""
	socketVolumeName := ""
	var dindID string
	if dindEnabled {
		id, derr := p.startDindSidecar(ctx, identity, nonce, dindMode, createdAt)
		if derr != nil {
			return guarded(derr)
		}
		dindID = id
		dockerHost = spec.DindDockerHost
		socketVolumeName = spec.SocketVolumeName(instanceName, nonce)
	}

	// 6. Build the runner container: ownership labels (ADR-004) + informational
	// os labels + this attempt's nonce, the per-mode env (ADR-002, plus
	// DOCKER_HOST in DinD modes), the memory limit (config/flavor), the
	// credential tmpfs, the NAMED workspace volume at the runner workdir,
	// attachment to the job network as its sole network, and — in DinD modes —
	// the shared socket volume mount.
	labels := identity.ContainerLabels(spec.RoleRunner, createdAt)
	labels[spec.LabelOSType] = string(bootstrap.OSType)
	labels[spec.LabelOSArch] = string(bootstrap.OSArch)
	labels[spec.LabelCreateNonce] = nonce

	env, err := buildRunnerEnv(bootstrap, dockerHost, plan.toolcachePath, plan.pnpmPath, plan.diagDir)
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
		WorkspaceVolumeName: spec.WorkspaceVolumeName(instanceName, nonce),
		NetworkName:         spec.JobNetworkName(instanceName),
		SocketVolumeName:    socketVolumeName,
		// Persistent, repo-scoped cache mounts (ADR-003). ToolcacheMountPath is
		// set whenever the cache is enabled, but a mount is added only when the
		// VOLUME name is also set (cache-eligible) — so a cache-ineligible
		// allocation gets RUNNER_TOOL_CACHE (ephemeral) but no persistent mount.
		ToolcacheVolumeName: plan.toolcacheVolume,
		ToolcacheMountPath:  plan.toolcachePath,
		PnpmVolumeName:      plan.pnpmVolume,
		PnpmMountPath:       plan.pnpmPath,
		// Shared externals volume, mounted READ-ONLY (ADR-003 W2 red-line F4), and
		// the per-repo diagnostic-logs volume, mounted read-write. Both are empty
		// (unset) for a cache-disabled config; externals is set for any scope when
		// the cache is enabled, diag only for a cache-eligible repo scope.
		ExternalsVolumeName: plan.externalsVolume,
		DiagVolumeName:      plan.diagVolume,
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

	// 9. Verify BOTH containers are still running before reporting success
	// (ADR-001 "verify BOTH containers running"; ADR-002 F7): a runner — or, in
	// DinD modes, a sidecar — that exited during or right after delivery must
	// not be reported to GARM as a healthy running instance.
	if err := p.verifyRunning(ctx, created.ID, "runner", instanceName); err != nil {
		return guarded(err)
	}
	if dindEnabled {
		if err := p.verifyRunning(ctx, dindID, "DinD sidecar", instanceName); err != nil {
			return guarded(err)
		}
	}

	// CreateInstance reports running (research.md §2.A): the container is up and
	// the runner registers once its entrypoint consumes the delivered
	// credentials.
	//
	// The provider_id is the GARM INSTANCE NAME, not the runner container's ID
	// (F6, ADR-004 amendment 2026-07-21). GARM passes provider_id back as
	// GARM_INSTANCE_ID on DeleteInstance; keying it to the instance name — the
	// label every allocation resource carries — means DeleteInstance can always
	// resolve-and-teardown the whole allocation (dind sidecar, network, volumes)
	// by that label, even when the runner container has already been removed out
	// of band. Returning the runner container ID here instead would strand the
	// privileged sidecar and volumes the moment the runner was gone, because
	// those are labeled with the instance name, not the container id.
	return params.ProviderInstance{
		ProviderID: instanceName,
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
// parsed) in non-JIT mode. dockerHost, when non-empty (DinD modes), is emitted
// as DOCKER_HOST so the runner reaches the sidecar's daemon over the shared
// socket volume; it is "" in none mode, leaving DOCKER_HOST unset (M0 behavior).
//
// toolCacheDir and pnpmStoreDir carry ADR-003's cache env: toolCacheDir becomes
// RUNNER_TOOL_CACHE (set whenever the cache feature is enabled, from the
// planCaches decision — possibly an ephemeral in-container path when the pool
// is cache-ineligible), and pnpmStoreDir becomes npm_config_store_dir (set only
// when a persistent pnpm store volume is mounted). diagDir, when set (a
// persistent diag volume is mounted), becomes GARM_DIAG_DIR so the entrypoint
// can own the mounted _diag dir before dropping privileges. Any being "" omits
// its env var.
func buildRunnerEnv(b params.BootstrapInstance, dockerHost, toolCacheDir, pnpmStoreDir, diagDir string) ([]string, error) {
	opts := spec.RunnerEnvOptions{
		JITConfigEnabled: b.JitConfigEnabled,
		GitHubURL:        githubBaseURL(b.RepoURL),
		RunnerWorkDir:    spec.RunnerWorkDir,
		RunnerName:       b.Name,
		RunnerGroup:      b.GitHubRunnerGroup,
		Labels:           b.Labels,
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
	// GARM_DIAG_DIR is a plain path (never a secret), appended here rather than
	// threaded through the credential-invisibility-audited spec.BuildRunnerEnv, so
	// that function's structural "no secret can be emitted" property is untouched.
	if diagDir != "" {
		env = append(env, spec.RunnerDiagDirEnv+"="+diagDir)
	}
	return env, nil
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
