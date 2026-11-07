package provider

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/topology"
)

// Cache-GC limits (ADR-003, Garbage collection). Each pass is bounded, so a large
// backlog is worked through over successive passes.
const (
	// cacheEvictionGrace keeps a just-created superseded cache volume out of the stale
	// log, since an older job may still be starting on it. It exceeds GARM's default
	// 20-minute bootstrap timeout, the bound on how long a create can be in flight.
	cacheEvictionGrace = 30 * time.Minute

	// cacheGCStaleLogMax, cacheGCMaxDiagPrunes and helperReapMax cap one pass's
	// stale-cache log entries, prune helpers and reaped helpers.
	cacheGCStaleLogMax   = 50
	cacheGCMaxDiagPrunes = 20
	helperReapMax        = 50

	// cacheGCCruftLogMax caps the unprovable volumes named in one pass's log line.
	cacheGCCruftLogMax = 50

	// diagPruneTimeout bounds one prune helper, a `find -delete` over one volume.
	diagPruneTimeout = 2 * time.Minute

	// diagPruneInterval is the least time between two diag-prune rounds of one
	// controller, across provider processes. Retention is in whole days, so a file
	// outlives its window by at most 6 hours, while the helpers are cut to four rounds
	// a day.
	diagPruneInterval = 6 * time.Hour

	// helperTerminalGrace leaves a just-exited helper to its own provider's deferred
	// removal. helperRunningMaxAge is the age at which a created or running helper
	// counts as wedged. It exceeds the seed timeout, so a peer's live seed is left
	// alone, while a crashed seeder cannot hold the externals flock for ever.
	helperTerminalGrace = 2 * time.Minute
	helperRunningMaxAge = externalsSeedTimeout + 5*time.Minute
)

// runCacheGC is the opportunistic housekeeping pass (ADR-003, Garbage collection). It
// reaps leaked helpers, logs stale and unprovable cache volumes, and prunes diagnostic
// logs at most once per diagPruneInterval. It never removes a cache volume and never
// fails the caller.
func (p *Provider) runCacheGC(ctx context.Context) {
	p.reapLeakedHelpers(ctx)

	// Without the image, this pass skips the externals supersession check; a later one
	// catches it.
	runnerImage := p.cfg.EffectiveRunnerImage("")
	digest := ""
	if d, err := p.imageDigestHex(ctx, runnerImage); err == nil {
		digest = d
	}

	gcCfg := spec.CacheGCConfig{
		Generation:  p.cfg.Cache.Generation,
		PnpmMajor:   p.cfg.Cache.PnpmMajor,
		ImageDigest: digest,
		StaleDays:   p.cfg.Cache.StaleCacheEvictionDays,
		Grace:       cacheEvictionGrace,
	}
	now := time.Now()
	decide := func(labels map[string]string) (bool, string) {
		return spec.EvaluateCacheEviction(labels, gcCfg, now)
	}

	stale, err := p.topo.ListStaleCaches(ctx, decide)
	if err != nil {
		slog.WarnContext(ctx, "cache GC: stale-cache enumeration had errors (continuing)", "error", err)
	}
	p.logStaleCaches(ctx, stale)

	p.pruneDiagVolumes(ctx, runnerImage)

	p.logUnprovableCacheCruft(ctx)
}

// logStaleCaches logs one bounded line naming the stale cache volumes and the `docker
// volume prune` command that reclaims them. It removes nothing: a volume delete is by
// name with no atomic label check, so an automatic one could hit a foreign volume
// (ADR-003). The prune skips volumes in use.
func (p *Provider) logStaleCaches(ctx context.Context, stale []topology.StaleCache) {
	if len(stale) == 0 {
		return
	}
	shown := stale
	if len(shown) > cacheGCStaleLogMax {
		shown = shown[:cacheGCStaleLogMax]
	}
	parts := make([]string, 0, len(shown))
	for _, s := range shown {
		parts = append(parts, s.Name+" ("+s.Reason+")")
	}
	purgeCmd := fmt.Sprintf("docker volume prune -a --filter label=%s=true --filter label=%s=%s",
		spec.LabelCache, spec.LabelControllerID, p.controllerID)
	slog.InfoContext(ctx, "cache GC: cache volume(s) are stale/superseded/aged and eligible for operator pruning; NOT auto-deleted (ADR-003: opportunistic auto-GC of a volume is inherently TOCTOU/foreign-unsafe)",
		"resource", "cache-volume", "count", len(stale), "purge_command", purgeCmd, "stale_caches", strings.Join(parts, ", "))
}

// logUnprovableCacheCruft logs one bounded line naming the cache-named volumes that
// carry no managed cache labels. The provider never deletes them.
func (p *Provider) logUnprovableCacheCruft(ctx context.Context) {
	cruft, err := p.topo.ListUnprovableCacheCruft(ctx)
	if err != nil {
		slog.WarnContext(ctx, "cache GC: cruft-visibility scan failed (continuing)", "error", err)
		return
	}
	if len(cruft) == 0 {
		return
	}
	shown := cruft
	if len(shown) > cacheGCCruftLogMax {
		shown = shown[:cacheGCCruftLogMax]
	}
	slog.InfoContext(ctx, "cache GC: cache-named volume(s) carry no managed cache labels (unlabeled auto-created replacements or foreign squatters); NOT deleting them (ADR-003: a volume that cannot be proved ours is never deleted) — an operator may reclaim disk manually if genuinely stale",
		"resource", "cache-volume", "count", len(cruft), "cruft_volumes", strings.Join(shown, ", "))
}

// pruneDiagVolumes prunes this controller's diagnostic-log volumes to their retention
// window, at most once per diagPruneInterval across provider processes. It uses the
// runner image without pulling it, skipping the round if the image is absent. Volumes
// are visited in random order, so past cacheGCMaxDiagPrunes every one is still reached
// over time.
func (p *Provider) pruneDiagVolumes(ctx context.Context, runnerImage string) {
	refs, err := p.topo.ListDiagVolumes(ctx)
	if err != nil {
		slog.WarnContext(ctx, "cache GC: failed to list diag volumes (continuing)", "error", err)
		return
	}
	if len(refs) == 0 {
		return
	}
	if _, _, err := p.cli.ImageInspectWithRaw(ctx, runnerImage); err != nil {
		slog.InfoContext(ctx, "cache GC: runner image not present; skipping diag prune this pass", "image", runnerImage)
		return
	}
	if !p.claimStamp(ctx, p.diagPruneStamp(), runnerImage) {
		return
	}
	order := slices.Clone(refs)
	rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	for i, ref := range order {
		if i >= cacheGCMaxDiagPrunes {
			slog.InfoContext(ctx, "cache GC: hit the per-round diag-prune cap; remaining diag volumes pruned in a later round",
				"cap", cacheGCMaxDiagPrunes)
			break
		}
		p.pruneDiagVolume(ctx, runnerImage, ref)
	}
}

// pruneDiagVolume runs one prune helper to completion against ref. A failure is only
// logged.
func (p *Provider) pruneDiagVolume(ctx context.Context, runnerImage string, ref topology.DiagVolumeRef) {
	volumeName := ref.Name
	nonce, err := newCreateNonce()
	if err != nil {
		slog.WarnContext(ctx, "cache GC: failed to name diag-prune helper", "resource", "cache-volume", "volume", volumeName, "error", err)
		return
	}
	cfg, hostCfg := spec.BuildDiagPruneContainer(spec.DiagPruneContainerSpec{
		Image:         runnerImage,
		VolumeName:    volumeName,
		RetentionDays: p.cfg.Cache.DiagnosticLogRetentionDays,
		Labels:        p.helperLabels(),
	})

	// Creating the helper pins the volume. It may have been replaced since the listing,
	// by an empty unlabelled volume or another repository's, so check its full identity
	// against the listing before `find -delete` runs. A volume that fails is left in
	// place (ADR-003).
	afterCreate := func(vctx context.Context) error {
		v, err := p.cli.VolumeInspect(vctx, volumeName)
		if err != nil {
			return fmt.Errorf("diag prune target %q could not be re-inspected: %w", volumeName, err)
		}
		if verr := spec.ValidateDiagPruneTarget(v.Labels, ref.Labels, p.controllerID); verr != nil {
			slog.WarnContext(vctx, "cache GC: diag prune target failed strict identity re-validation; leaving it in place and skipping the prune",
				"resource", "cache-volume", "volume", volumeName, "error", verr)
			return fmt.Errorf("diag prune target %q failed strict identity re-validation: %w", volumeName, verr)
		}
		return nil
	}

	code, err := p.runHelperContainer(ctx, cfg, hostCfg, "garm-diagprune-"+nonce, diagPruneTimeout, afterCreate)
	if err != nil {
		slog.WarnContext(ctx, "cache GC: skipping/failed diag prune (continuing)", "resource", "cache-volume", "volume", volumeName, "error", err)
		return
	}
	if code != 0 {
		slog.WarnContext(ctx, "cache GC: diag prune exited non-zero (continuing)", "resource", "cache-volume", "volume", volumeName, "exit_code", code)
		return
	}
	slog.InfoContext(ctx, "cache GC: pruned diag volume", "resource", "cache-volume", "volume", volumeName, "retention_days", p.cfg.Cache.DiagnosticLogRetentionDays)
}

// reapLeakedHelpers force-removes this controller's cache helpers whose provider died
// before its deferred removal, up to helperReapMax per pass. helperReapable decides by
// state and age, so a live peer's helper is never raced.
func (p *Provider) reapLeakedHelpers(ctx context.Context) {
	f := filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
		filters.Arg("label", spec.LabelRole+"="+spec.RoleCacheHelper),
	)
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		slog.WarnContext(ctx, "cache GC: failed to list helper containers (continuing)", "error", err)
		return
	}
	now := time.Now()
	reaped := 0
	for _, c := range list {
		if reaped >= helperReapMax {
			break
		}
		// On top of the filter: a job allocation would carry an instance name.
		if c.Labels[spec.LabelRole] != spec.RoleCacheHelper || c.Labels[spec.LabelControllerID] != p.controllerID {
			continue
		}
		if _, hasInst := c.Labels[spec.LabelInstanceName]; hasInst {
			continue
		}
		age, known := helperAge(now, c.Labels[spec.LabelCreatedAt])
		if !helperReapable(c.State, age, known) {
			continue
		}
		if err := p.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			slog.WarnContext(ctx, "cache GC: failed to reap leaked helper (continuing)", "resource", "container", "container_id", c.ID, "error", err)
			continue
		}
		reaped++
	}
	if reaped > 0 {
		slog.InfoContext(ctx, "cache GC: reaped leaked cache-helper container(s)", "count", reaped)
	}
}

// helperReapable reports whether a helper in the given Docker state should be reaped.
// known is false when its age could not be determined. A created or running helper may
// be a live peer's, so it needs a known age past helperRunningMaxAge. A terminal helper
// holds no lock, so it goes past helperTerminalGrace or when its age is unknown.
func helperReapable(state string, age time.Duration, known bool) bool {
	switch state {
	case "created", "running":
		return known && age >= helperRunningMaxAge
	default: // exited, dead, or any other terminal state
		return !known || age >= helperTerminalGrace
	}
}

// helperAge returns a helper's age from its RFC3339 created-at label. known is false
// when the label is missing or unparseable.
func helperAge(now time.Time, createdAt string) (age time.Duration, known bool) {
	if createdAt == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return 0, false
	}
	return now.Sub(t), true
}

// helperLabels returns a cache helper's labels. With no instance name, teardown and the
// sweep cannot match it and ListInstances never reports it. The role and created-at
// labels let reapLeakedHelpers find and age it.
func (p *Provider) helperLabels() map[string]string {
	return p.ownedHelperLabels(spec.RoleCacheHelper)
}

// ownedHelperLabels returns the labels of a provider-owned container that is not a job
// allocation: a cache helper or a throttle stamp.
func (p *Provider) ownedHelperLabels(role string) map[string]string {
	return map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: p.controllerID,
		spec.LabelRole:         role,
		spec.LabelCreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
}

// runHelperContainer creates, starts and waits for a helper container within timeout,
// then force-removes it and returns its exit code. The removal uses a detached context,
// so a cancellation cannot strand the helper.
//
// afterCreate, if non-nil, runs after the create has pinned the helper's named volumes
// and before the start. An error from it aborts the run before the helper's payload
// executes; the removal then unpins the volume, which is left in place.
func (p *Provider) runHelperContainer(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, name string, timeout time.Duration, afterCreate func(ctx context.Context) error) (int, error) {
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	created, err := p.cli.ContainerCreate(runCtx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return 0, fmt.Errorf("failed to create helper %q: %w", name, err)
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if rerr := p.cli.ContainerRemove(rmCtx, created.ID, container.RemoveOptions{Force: true}); rerr != nil && !errdefs.IsNotFound(rerr) {
			slog.WarnContext(rmCtx, "failed to remove helper container (continuing)", "resource", "container", "container_name", name, "error", rerr)
		}
	}()

	if afterCreate != nil {
		if err := afterCreate(runCtx); err != nil {
			return 0, err
		}
	}

	if err := p.cli.ContainerStart(runCtx, created.ID, container.StartOptions{}); err != nil {
		return 0, fmt.Errorf("failed to start helper %q: %w", name, err)
	}

	statusCh, errCh := p.cli.ContainerWait(runCtx, created.ID, container.WaitConditionNotRunning)
	select {
	case werr := <-errCh:
		return 0, fmt.Errorf("failed to wait for helper %q: %w", name, werr)
	case resp := <-statusCh:
		if resp.Error != nil {
			return int(resp.StatusCode), fmt.Errorf("helper %q reported a daemon error: %s", name, resp.Error.Message)
		}
		return int(resp.StatusCode), nil
	case <-runCtx.Done():
		return 0, fmt.Errorf("helper %q did not complete in time: %w", name, runCtx.Err())
	}
}
