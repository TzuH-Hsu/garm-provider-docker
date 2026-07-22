package provider

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/topology"
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

	// cacheGCStaleLogMax / cacheGCMaxDiagPrunes / helperReapMax bound one GC
	// pass's stale-cache log entries, prune-helper runs, and leaked-helper reaps
	// respectively. (Cache eviction is non-destructive/log-only — NEW-H1 — so
	// there is no removal cap; the cap bounds the operator-visibility log line.)
	cacheGCStaleLogMax   = 50
	cacheGCMaxDiagPrunes = 20
	helperReapMax        = 50

	// cacheGCCruftLogMax caps how many unprovable cache-named volumes one GC pass
	// names in its operator-visibility log line, so a large backlog of cruft cannot
	// produce an unbounded log entry.
	cacheGCCruftLogMax = 50

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
//  2. LOGS (never removes — NEW-H1) superseded/aged cache volumes for THIS
//     controller so an operator can reclaim disk with a deliberate purge. Cache-
//     volume auto-eviction is non-destructive because Docker has no atomic
//     label-qualified volume delete, making an opportunistic by-name delete
//     inherently TOCTOU/foreign-unsafe (see ListStaleCaches / logStaleCaches);
//  3. prunes each diagnostic-logs volume to its retention window via a
//     provider-run helper (the one remaining destructive cache action — it
//     deletes FILES, strict-identity-gated; retention stays OUT of the untrusted
//     runner, F14).
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

	stale, err := p.topo.ListStaleCaches(ctx, decide)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: stale-cache enumeration had errors (continuing): %v", err)
	}
	p.logStaleCaches(stale)

	p.pruneDiagVolumes(ctx, runnerImage)

	// Cruft visibility (ADR-003 never-delete-unprovable): surface any
	// cache-name-prefixed volumes that lack our managed cache labels — unlabeled
	// Moby auto-created replacements or foreign squatters the provider will NOT
	// delete (a Moby auto-created unlabeled volume and a foreign one are
	// indistinguishable), so an operator can reclaim genuinely-stale disk manually.
	p.logUnprovableCacheCruft(ctx)
}

// logStaleCaches surfaces (best-effort, bounded, in ONE consolidated line) the
// cache volumes the GC found stale/superseded/aged, so an operator can reclaim
// disk with a DELIBERATE purge. It removes nothing: ADR-003's cache-volume GC is
// non-destructive (NEW-H1) because Docker has no atomic label-qualified volume
// delete, so an opportunistic auto-delete of a volume is inherently TOCTOU and
// cannot be made foreign-safe. The line names the exact controller-scoped,
// label-filtered `docker volume prune` an operator can run at a quiet moment —
// which, being operator-invoked (no concurrent-create race in practice) and
// operator-accepted, is the sanctioned destructive path opportunistic auto-GC is
// not. `docker volume prune` itself skips any in-use volume, so a warm cache held
// by a live job is never reclaimed even by the manual purge.
func (p *Provider) logStaleCaches(stale []topology.StaleCache) {
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
	log.Printf("garm-provider-docker: cache GC: %d cache volume(s) are stale/superseded/aged and eligible for operator pruning; NOT auto-deleted (ADR-003: Docker has no atomic label-qualified volume delete, so opportunistic auto-GC of a volume is inherently TOCTOU/foreign-unsafe). Reclaim disk with an explicit, quiescent-moment purge: docker volume prune -a --filter label=%s=true --filter label=%s=%s. Stale caches: %s",
		len(stale), spec.LabelCache, spec.LabelControllerID, p.controllerID, strings.Join(parts, ", "))
}

// logUnprovableCacheCruft logs (best-effort, bounded) the cache-named volumes that
// carry no managed cache labels, for operator visibility. It never deletes
// anything — under ADR-003's label-as-identity rule the provider only ever removes
// volumes it can POSITIVELY prove are ours (managed + full identity).
func (p *Provider) logUnprovableCacheCruft(ctx context.Context) {
	cruft, err := p.topo.ListUnprovableCacheCruft(ctx)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: cruft-visibility scan failed (continuing): %v", err)
		return
	}
	if len(cruft) == 0 {
		return
	}
	shown := cruft
	if len(shown) > cacheGCCruftLogMax {
		shown = shown[:cacheGCCruftLogMax]
	}
	log.Printf("garm-provider-docker: cache GC: %d cache-named volume(s) carry no managed cache labels (unlabeled auto-created replacements or foreign squatters); NOT deleting them (ADR-003 never-delete-unprovable) — an operator may reclaim disk manually if these are genuinely stale: %s", len(cruft), strings.Join(shown, ", "))
}

// pruneDiagVolumes runs the retention prune helper against each of this
// controller's diagnostic-logs volumes (ADR-003 F14, provider-side). It needs an
// image with `find`; it reuses the runner image but does NOT pull it — if the
// image is not present locally this pass, the prune is skipped (best-effort) and
// runs on a later pass once the image is present. Bounded per pass.
func (p *Provider) pruneDiagVolumes(ctx context.Context, runnerImage string) {
	refs, err := p.topo.ListDiagVolumes(ctx)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: failed to list diag volumes (continuing): %v", err)
		return
	}
	if len(refs) == 0 {
		return
	}
	if _, _, err := p.cli.ImageInspectWithRaw(ctx, runnerImage); err != nil {
		log.Printf("garm-provider-docker: cache GC: runner image %q not present; skipping diag prune this pass", runnerImage)
		return
	}
	for i, ref := range refs {
		if i >= cacheGCMaxDiagPrunes {
			log.Printf("garm-provider-docker: cache GC: hit the per-pass diag-prune cap (%d); remaining diag volumes pruned on a later pass", cacheGCMaxDiagPrunes)
			break
		}
		p.pruneDiagVolume(ctx, runnerImage, ref)
	}
}

// pruneDiagVolume runs one diagnostic-log prune helper to completion against a
// single diag volume (best-effort: a prune failure is logged, never fatal).
//
// Pin-then-validate (ADR-003 W2 structural redesign): the volume NAME and its
// IDENTITY LABELS came from a ListDiagVolumes snapshot; a concurrent GC could
// remove the volume in the gap before this runs, and the OLD inspect-then-create
// flow left a wedge window (the removed volume would be Moby-AUTO-CREATED UNLABELED
// and the deterministic name rejected forever by the retired M6 guard). Instead we
// create the helper FIRST (pinning the volume, or the unlabeled auto-created
// replacement), then re-validate the pinned volume's FULL identity in the
// afterCreate hook BEFORE the prune runs — the whole managed+cache+controller+
// cache-kind+repo+repo-url-digest tuple against the snapshot, so a reincarnated or
// foreign same-name diag volume of a DIFFERENT repo is not pruned as if it were
// ours (the earlier check compared only controller/kind, not repo/repo-url-digest).
// If the pinned volume is not provably the snapshot's identity, the prune is
// aborted WITHOUT deleting it — a Moby auto-created unlabeled volume and a foreign
// one are indistinguishable, so name-based deletion can never be foreign-safe (B2);
// the volume is left as cruft (logged) and GARM's next EnsureCacheVolume reconciles
// around the slot. An inspect FAILURE aborts closed WITHOUT any delete.
func (p *Provider) pruneDiagVolume(ctx context.Context, runnerImage string, ref topology.DiagVolumeRef) {
	volumeName := ref.Name
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

	afterCreate := func(vctx context.Context) error {
		v, err := p.cli.VolumeInspect(vctx, volumeName)
		if err != nil {
			// inspect failed → don't know → don't prune, abort closed.
			return fmt.Errorf("diag prune target %q could not be re-inspected: %w", volumeName, err)
		}
		// STRICT kind-aware full-identity re-validation of the PINNED volume before
		// the destructive `find -delete` (NEW-H2): it must carry the FULL diag-logs
		// identity (managed+cache+cache-kind=diag-logs+repo+repo-url-digest), be
		// THIS controller's, and match the enumeration snapshot's repo +
		// repo-url-digest. A same-name replacement of a DIFFERENT repo, an unlabeled
		// Moby auto-created volume, or a foreign volume fails here.
		if verr := spec.ValidateDiagPruneTarget(v.Labels, ref.Labels, p.controllerID); verr != nil {
			// Do NOT delete/prune it (never touch an unprovable volume — B2): leave
			// it in place and skip the prune; the next EnsureCacheVolume reconciles
			// around the slot.
			log.Printf("garm-provider-docker: cache GC: diag prune target %q failed strict identity re-validation (%v); leaving it in place (ADR-003 never-delete-unprovable) and skipping the prune", volumeName, verr)
			return fmt.Errorf("diag prune target %q failed strict identity re-validation: %w", volumeName, verr)
		}
		return nil
	}

	code, err := p.runHelperContainer(ctx, cfg, hostCfg, "garm-diagprune-"+nonce, diagPruneTimeout, afterCreate)
	if err != nil {
		log.Printf("garm-provider-docker: cache GC: skipping/failed diag prune of %q (continuing): %v", volumeName, err)
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
//
// Pin-then-validate (H3c): the ContainerCreate PINS every named volume the helper
// references — or, if a concurrent GC removed a referenced cache volume since the
// caller's snapshot, real Moby AUTO-CREATES it UNLABELED and the helper pins THAT
// empty replacement. afterCreate, when non-nil, runs AFTER the create but BEFORE
// the start: it inspects the now-pinned target and returns an error to abort the
// run WITHOUT executing the helper's payload (the seed's copy, the prune's
// `find -delete`). On that abort the deferred removal still runs, UNPINNING the
// orphan so the caller can reap it by name — never leaving a wedging, GC-invisible
// unlabeled volume behind. A nil afterCreate keeps the plain create→start→wait
// behavior.
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
			log.Printf("garm-provider-docker: failed to remove helper %q (continuing): %v", name, rerr)
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
