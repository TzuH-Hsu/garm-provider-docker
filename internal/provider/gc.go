package provider

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// Opportunistic cache-GC tuning (ADR-003 W2). The pass is bounded so a large
// backlog can never make a single CreateInstance/ListInstances do unbounded
// housekeeping work; the remainder is caught over successive passes.
const (
	// cacheEvictionGrace protects a very-recently-created SUPERSEDED cache volume
	// from eviction, so an in-flight older-generation/older-image job that just
	// created its cache but has not yet started the runner that would in-use-pin
	// it is not reaped out from under it. It comfortably exceeds GARM's ~20-min
	// bootstrap timeout, the outer bound on how long a create can be in flight.
	cacheEvictionGrace = 30 * time.Minute

	// cacheGCMaxEvictions / cacheGCMaxDiagPrunes / helperReapMax bound one GC
	// pass's removals, prune-helper runs, and leaked-helper reaps respectively.
	cacheGCMaxEvictions  = 50
	cacheGCMaxDiagPrunes = 20
	helperReapMax        = 50

	// diagPruneTimeout bounds one diagnostic-log prune helper (a `find -delete`
	// over one volume — fast; this is generous headroom).
	diagPruneTimeout = 2 * time.Minute

	// helperTerminalGrace and helperRunningMaxAge tune the leaked-helper reaper
	// by container state (H5), so it never races a live peer's helper yet still
	// eventually reclaims a genuinely stuck one:
	//
	//   - helperTerminalGrace leaves a freshly-TERMINAL (exited/dead) helper alone:
	//     a helper that just finished is about to be force-removed by its own
	//     provider's defer, so reaping it inside this grace only races that defer.
	//     It exceeds the create→start→wait handoff; a terminal helper older than it
	//     whose provider clearly died is reaped.
	//   - helperRunningMaxAge is the age past which a still-CREATED-or-RUNNING helper
	//     is treated as WEDGED and reaped. Below it a peer's in-flight seed/prune is
	//     never touched — a legit helper is briefly `created` between ContainerCreate
	//     and ContainerStart, and `running` while it copies/prunes. Above it a crashed
	//     running seeder is reaped so it can no longer hold the externals seeding
	//     flock indefinitely. It comfortably exceeds the externals seed timeout.
	helperTerminalGrace = 2 * time.Minute
	helperRunningMaxAge = externalsSeedTimeout + 5*time.Minute
)

// runCacheGC is the opportunistic, best-effort cache housekeeping pass ADR-003
// piggybacks on CreateInstance and ListInstances (there is no background daemon,
// ADR-004). It NEVER hard-fails the caller: every step logs and continues. It:
//
//  1. reaps any leaked cache-helper containers (a prior seed/prune whose provider
//     process died before its own defer removed it);
//  2. evicts superseded/aged cache volumes for THIS controller (age since
//     creation + generation/pnpm-major/image-digest supersession — the immutable
//     last-used label records creation, so eviction is coarse-but-self-healing;
//     an in-use cache is skipped, never yanked from a live job);
//  3. prunes each diagnostic-logs volume to its retention window via a
//     provider-run helper (retention stays OUT of the untrusted runner, F14).
func (p *Provider) runCacheGC(ctx context.Context) {
	p.reapLeakedHelpers(ctx)

	// Resolve the current runner-image digest best-effort: if the image is not
	// present this pass (e.g. a ListInstances GC before any create), externals
	// supersession is simply skipped this pass and caught later — self-healing.
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

	evicted, err := p.topo.EvictCaches(ctx, decide, cacheGCMaxEvictions)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: eviction pass had errors (continuing): %v", err)
	}
	for _, e := range evicted {
		log.Printf("garm-provider-docker: cache GC: evicted %s (%s)", e.Name, e.Reason)
	}

	p.pruneDiagVolumes(ctx, runnerImage)
}

// pruneDiagVolumes runs the retention prune helper against each of this
// controller's diagnostic-logs volumes (ADR-003 F14, provider-side). It needs an
// image with `find`; it reuses the runner image but does NOT pull it — if the
// image is not present locally this pass, the prune is skipped (best-effort) and
// runs on a later pass once the image is present. Bounded per pass.
func (p *Provider) pruneDiagVolumes(ctx context.Context, runnerImage string) {
	names, err := p.topo.ListDiagVolumes(ctx)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: failed to list diag volumes (continuing): %v", err)
		return
	}
	if len(names) == 0 {
		return
	}
	if _, _, err := p.cli.ImageInspectWithRaw(ctx, runnerImage); err != nil {
		log.Printf("garm-provider-docker: cache GC: runner image %q not present; skipping diag prune this pass", runnerImage)
		return
	}
	for i, name := range names {
		if i >= cacheGCMaxDiagPrunes {
			log.Printf("garm-provider-docker: cache GC: hit the per-pass diag-prune cap (%d); remaining diag volumes pruned on a later pass", cacheGCMaxDiagPrunes)
			break
		}
		p.pruneDiagVolume(ctx, runnerImage, name)
	}
}

// pruneDiagVolume runs one diagnostic-log prune helper to completion against a
// single diag volume (best-effort: a prune failure is logged, never fatal).
//
// H3: it RE-VALIDATES the volume is STILL this controller's diag volume
// immediately before mounting it into the age-scoped `find -delete` helper. The
// name came from a ListDiagVolumes snapshot; a remove/recreate-under-the-same-name
// since then could have replaced it with a foreign volume the prune would
// otherwise wrongly delete files from. If it is no longer our diag volume (or is
// gone), the prune is skipped.
func (p *Provider) pruneDiagVolume(ctx context.Context, runnerImage, volumeName string) {
	v, err := p.cli.VolumeInspect(ctx, volumeName)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: skipping diag prune of %q (re-inspect failed): %v", volumeName, err)
		return
	}
	if v.Labels[spec.LabelCache] != "true" ||
		v.Labels[spec.LabelControllerID] != p.controllerID ||
		v.Labels[spec.LabelCacheKind] != string(spec.CacheKindDiagLogs) {
		log.Printf("garm-provider-docker: cache GC: skipping diag prune of %q — it is no longer this controller's diag volume (a same-name replacement since the snapshot)", volumeName)
		return
	}

	nonce, err := newCreateNonce()
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: failed to name diag-prune helper for %q: %v", volumeName, err)
		return
	}
	cfg, hostCfg := spec.BuildDiagPruneContainer(spec.DiagPruneContainerSpec{
		Image:         runnerImage,
		VolumeName:    volumeName,
		RetentionDays: p.cfg.Cache.DiagnosticLogRetentionDays,
		Labels:        p.helperLabels(),
	})
	code, err := p.runHelperContainer(ctx, cfg, hostCfg, "garm-diagprune-"+nonce, diagPruneTimeout)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: diag prune of %q failed (continuing): %v", volumeName, err)
		return
	}
	if code != 0 {
		log.Printf("garm-provider-docker: cache GC: diag prune of %q exited %d (continuing)", volumeName, code)
		return
	}
	log.Printf("garm-provider-docker: cache GC: pruned diag volume %s (files older than %dd)", volumeName, p.cfg.Cache.DiagnosticLogRetentionDays)
}

// reapLeakedHelpers force-removes a LEAKED cache-helper container for THIS
// controller — a seed/prune helper whose provider process died before its own
// defer removed it — while NEVER racing a live peer's in-flight helper (H5). It
// only touches containers labeled role=cache-helper for this controller and
// carrying NO instance-name (defense-in-depth: a helper is never a job
// allocation), and it gates the removal on the helper's Docker STATE and its
// creation AGE (the created-at label helperLabels stamps):
//
//   - `created`/`running`: a peer's seed/prune is briefly `created` between
//     ContainerCreate and ContainerStart, and `running` while it copies/prunes —
//     both are LEFT ALONE until older than helperRunningMaxAge, at which point a
//     WEDGED/crashed one is reaped so it can no longer hold the externals seeding
//     flock indefinitely (the old reaper skipped every `running` helper forever,
//     leaking the lock, AND reaped every `created` one immediately, racing a peer);
//   - terminal (exited/dead): left to its own provider's defer within
//     helperTerminalGrace, then reaped as a genuine leak.
//
// Bounded and best-effort.
func (p *Provider) reapLeakedHelpers(ctx context.Context) {
	f := filters.NewArgs(
		filters.Arg("label", spec.LabelManaged+"=true"),
		filters.Arg("label", spec.LabelControllerID+"="+p.controllerID),
		filters.Arg("label", spec.LabelRole+"="+spec.RoleCacheHelper),
	)
	list, err := p.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: failed to list helper containers (continuing): %v", err)
		return
	}
	now := time.Now()
	reaped := 0
	for _, c := range list {
		if reaped >= helperReapMax {
			break
		}
		// Defense-in-depth: only THIS controller's cache-helpers, never a job
		// allocation (which would carry an instance-name).
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
			log.Printf("garm-provider-docker: cache GC: failed to reap leaked helper %s (continuing): %v", c.ID, err)
			continue
		}
		reaped++
	}
	if reaped > 0 {
		log.Printf("garm-provider-docker: cache GC: reaped %d leaked cache-helper container(s)", reaped)
	}
}

// helperReapable decides whether a leaked cache-helper in Docker state `state`,
// aged `age` (known=false when its created-at label was missing/unparseable),
// should be reaped now (H5). A non-terminal helper is reaped only once it is
// clearly WEDGED (older than helperRunningMaxAge, and only when its age is known,
// so a live peer is never guessed at); a terminal helper is reaped once past
// helperTerminalGrace, or immediately when its age is unknown (it holds no lock,
// so reaping it is always safe).
func helperReapable(state string, age time.Duration, known bool) bool {
	switch state {
	case "created", "running":
		return known && age >= helperRunningMaxAge
	default: // exited, dead, or any other terminal state
		return !known || age >= helperTerminalGrace
	}
}

// helperAge parses a helper's created-at label (RFC3339, stamped by helperLabels)
// into an age relative to now. known is false when the label is missing or
// unparseable, which the reaper treats conservatively (never reaping a
// non-terminal helper it cannot age).
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

// helperLabels returns the labels every short-lived cache-helper container (the
// externals seeder / diag pruner) carries: managed + controller-id +
// role=cache-helper + a CREATION-time created-at (H5). It carries NO
// instance-name, so ADR-004's teardown/sweep predicate structurally excludes it
// and ListInstances (role=runner) never reports it; the role label lets
// reapLeakedHelpers find a crash-leaked helper and the created-at label lets it
// age one by state without ever racing a live peer's in-flight helper.
func (p *Provider) helperLabels() map[string]string {
	return map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: p.controllerID,
		spec.LabelRole:         spec.RoleCacheHelper,
		spec.LabelCreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
}

// runHelperContainer creates, starts, and waits for a short-lived helper
// container to run to COMPLETION, then force-removes it, returning its exit
// code. It is the shared run-to-completion primitive behind the externals seeder
// and the diag pruner. The removal runs in a defer under a context detached from
// the caller's (so a caller cancellation cannot strand the helper), and NotFound
// on removal is tolerated. A per-helper timeout bounds the run.
func (p *Provider) runHelperContainer(ctx context.Context, cfg *container.Config, hostCfg *container.HostConfig, name string, timeout time.Duration) (int, error) {
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
			log.Printf("garm-provider-docker: failed to remove helper %q (continuing): %v", name, rerr)
		}
	}()

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
