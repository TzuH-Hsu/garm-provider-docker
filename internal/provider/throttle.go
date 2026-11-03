package provider

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// Per-controller throttle intervals for the housekeeping ListInstances runs
// (the diag prune has its own, diagPruneInterval in gc.go).
//
// GARM calls ListInstances from every scale set and pool worker on each
// consolidation pass, and each call is a fresh provider process. Without a
// throttle a dozen scale sets run the same controller-wide sweep and GC a dozen
// times per pass, and on a slow daemon those passes queue behind each other.
const (
	// orphanSweepInterval is the least time between two ListInstances orphan
	// sweeps of one controller, across provider processes. A sweep only collects
	// allocations that are already abandoned (an exited runner past
	// exitedRunnerGrace, a create past its in-flight deadline), so running it
	// less often delays a collection and never changes what is collected. Three
	// minutes keeps an exited runner's leftovers to about
	// exitedRunnerGrace+orphanSweepInterval (5 minutes) while cutting a burst of
	// calls to one sweep. CreateInstance's pre-create sweep is not throttled.
	orphanSweepInterval = 3 * time.Minute

	// cacheGCInterval is the least time between two cache-GC passes of one
	// controller. The pass reaps leaked helpers, logs stale caches and cruft for
	// an operator, and prunes diag logs; none of it is time-critical, and a
	// leaked helper is reaped only after helperTerminalGrace (2 minutes) at the
	// earliest. Five minutes bounds the pass to twelve an hour.
	cacheGCInterval = 5 * time.Minute
)

// throttleStamp describes one rate-limited pass: the never-started container
// that records when it last ran, and how often it may run.
type throttleStamp struct {
	name     string        // the stamp's container name; the name is the claim
	role     string        // the role label an own stamp carries
	interval time.Duration // least time between two rounds
	what     string        // the pass, for log lines
}

func (p *Provider) diagPruneStamp() throttleStamp {
	return throttleStamp{
		name:     spec.DiagPruneStampName(p.controllerID),
		role:     spec.RoleDiagPruneStamp,
		interval: diagPruneInterval,
		what:     "diag prune",
	}
}

func (p *Provider) orphanSweepStamp() throttleStamp {
	return throttleStamp{
		name:     spec.ThrottleStampName(p.controllerID, spec.StampOrphanSweep),
		role:     spec.RoleThrottleStamp,
		interval: orphanSweepInterval,
		what:     "orphan sweep",
	}
}

func (p *Provider) cacheGCStamp() throttleStamp {
	return throttleStamp{
		name:     spec.ThrottleStampName(p.controllerID, spec.StampCacheGC),
		role:     spec.RoleThrottleStamp,
		interval: cacheGCInterval,
		what:     "cache GC",
	}
}

// sweepThrottled runs the controller-wide orphan sweep at most once per
// orphanSweepInterval per controller, across provider processes. It is for
// ListInstances; CreateInstance calls the sweep directly so a stale allocation
// of the instance being created is always collected first. A failed sweep is
// logged and never fails the caller.
func (p *Provider) sweepThrottled(ctx context.Context) {
	if !p.claimRound(ctx, p.orphanSweepStamp()) {
		return
	}
	if err := p.topo.SweepOrphans(ctx); err != nil {
		slog.WarnContext(ctx, "ListInstances: orphan sweep failed (continuing)", "error", err)
	}
}

// runCacheGCThrottled runs the cache GC at most once per cacheGCInterval per
// controller, across provider processes, whichever of ListInstances and
// CreateInstance reaches it first.
func (p *Provider) runCacheGCThrottled(ctx context.Context) {
	if p.claimRound(ctx, p.cacheGCStamp()) {
		p.runCacheGC(ctx)
	}
}

// claimRound claims a round of a ListInstances/CreateInstance pass through its
// stamp. The stamp container needs the runner image to exist locally, so when
// the image is not present (a host that has not yet run a job) there is no
// stamp to keep and the pass runs unthrottled: housekeeping is never skipped for
// want of an image, and there is little to repeat on such a host. Any other
// daemon error skips the round, as for the diag prune.
func (p *Provider) claimRound(ctx context.Context, st throttleStamp) bool {
	image := p.cfg.EffectiveRunnerImage("")
	if _, _, err := p.cli.ImageInspectWithRaw(ctx, image); err != nil {
		if errdefs.IsNotFound(err) {
			return true
		}
		slog.WarnContext(ctx, "throttle: failed to inspect the runner image; skipping this pass",
			"pass", st.what, "image", image, "error", err)
		return false
	}
	return p.claimStamp(ctx, st, image)
}

// claimStamp reports whether this pass may run a round of st's pass
// (ADR-003 amendment 2026-10-26, generalised by the 2026-11-03 amendment). The
// provider is a fresh process per GARM call and has no state directory, so the
// time of the last round lives in Docker: a never-started stamp container per
// controller and pass whose created-at label is when that round began.
//
//   - No stamp: create one and run.
//   - A stamp younger than st.interval: skip (throttled).
//   - An expired stamp: remove it BY ID, create a new one and run.
//   - A stamp whose age is unknown (created-at missing or unparseable) or in the
//     future: replace it WITHOUT running, so the next round comes one interval
//     later. A bad stamp delays the pass by one interval; it never causes a
//     storm.
//
// The create is the atomic claim: Docker rejects a second container of the same
// name with a 409 Conflict, so when several processes find the stamp missing or
// expired, exactly one wins and the others skip. Removing the old stamp by its
// ID (not its name) keeps a slow peer from deleting the winner's fresh stamp. A
// container of the stamp's name that is not provably this controller's stamp is
// never removed: the round is skipped and the squatter logged. A 409 Conflict on
// removing the expired stamp means a peer is already replacing it and has won
// the round; that skips quietly, like a conflicting create. Any other daemon
// error also skips the round (run later); nothing here fails the caller.
func (p *Provider) claimStamp(ctx context.Context, st throttleStamp, image string) bool {
	existing, err := p.cli.ContainerInspect(ctx, st.name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return p.createStamp(ctx, st, image)
		}
		slog.WarnContext(ctx, "throttle: failed to inspect the stamp; skipping this pass",
			"pass", st.what, "resource", "container", "container_name", st.name, "error", err)
		return false
	}

	var labels map[string]string
	if existing.Config != nil {
		labels = existing.Config.Labels
	}
	if !p.isOwnStamp(labels, st.role) || existing.ContainerJSONBase == nil || existing.ID == "" {
		slog.WarnContext(ctx, "throttle: a container named like the stamp is not this controller's stamp; leaving it in place and skipping this pass",
			"pass", st.what, "resource", "container", "container_name", st.name)
		return false
	}

	age, known := helperAge(time.Now(), labels[spec.LabelCreatedAt])
	if known && age >= 0 && age < st.interval {
		return false
	}
	if err := p.cli.ContainerRemove(ctx, existing.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		if errdefs.IsConflict(err) {
			// A 409 here is "removal of container ... is already in progress":
			// a peer found the same stamp expired and is replacing it, so it owns
			// this round. Losing the claim is the expected outcome, as for a
			// conflicting create, so it is not a warning.
			slog.DebugContext(ctx, "throttle: a peer is replacing the stamp; skipping this pass",
				"pass", st.what, "resource", "container", "container_name", st.name, "error", err)
			return false
		}
		slog.WarnContext(ctx, "throttle: failed to replace the stamp; skipping this pass",
			"pass", st.what, "resource", "container", "container_name", st.name, "error", err)
		return false
	}
	if !known || age < 0 {
		if p.createStamp(ctx, st, image) {
			slog.WarnContext(ctx, "throttle: the stamp had no usable time; reset it, the next round runs one interval from now",
				"pass", st.what, "resource", "container", "container_name", st.name, "interval", st.interval.String())
		}
		return false
	}
	return p.createStamp(ctx, st, image)
}

// createStamp creates st's stamp and reports whether this process now owns the
// round. A 409 Conflict means a concurrent provider process created it first and
// owns the round.
func (p *Provider) createStamp(ctx context.Context, st throttleStamp, image string) bool {
	cfg, hostCfg := spec.BuildStampContainer(image, p.ownedHelperLabels(st.role))
	if _, err := p.cli.ContainerCreate(ctx, cfg, hostCfg, nil, nil, st.name); err != nil {
		if !errdefs.IsConflict(err) {
			slog.WarnContext(ctx, "throttle: failed to create the stamp; skipping this pass",
				"pass", st.what, "resource", "container", "container_name", st.name, "error", err)
		}
		return false
	}
	return true
}

// isOwnStamp reports whether labels are those of this controller's stamp of the
// given role: managed, this controller-id, that role, and no instance-name (a
// stamp is never a job allocation).
func (p *Provider) isOwnStamp(labels map[string]string, role string) bool {
	if labels[spec.LabelManaged] != "true" || labels[spec.LabelControllerID] != p.controllerID {
		return false
	}
	if labels[spec.LabelRole] != role {
		return false
	}
	_, hasInst := labels[spec.LabelInstanceName]
	return !hasInst
}
