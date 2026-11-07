package provider

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// Per-controller throttle intervals for the housekeeping ListInstances runs. GARM calls
// ListInstances from every scale set and pool worker on each consolidation pass, each
// in a fresh process, so without a throttle a dozen scale sets repeat the same
// controller-wide work a dozen times and queue behind each other on a slow daemon. The
// diag prune has its own interval, diagPruneInterval in gc.go.
const (
	// orphanSweepInterval is the least time between two ListInstances orphan sweeps. A
	// sweep only collects what is already abandoned, so running it less often delays a
	// collection and never changes what is collected: an exited runner's leftovers wait
	// at most about 5 minutes, exitedRunnerGrace plus this.
	orphanSweepInterval = 3 * time.Minute

	// cacheGCInterval is the least time between two cache-GC passes. Nothing in a pass
	// is time-critical, and this bounds it to twelve an hour.
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

// sweepThrottled runs the orphan sweep at most once per orphanSweepInterval across
// provider processes, for ListInstances. CreateInstance calls the sweep directly, so a
// stale allocation of the instance being created always goes first.
func (p *Provider) sweepThrottled(ctx context.Context) {
	if !p.claimRound(ctx, p.orphanSweepStamp()) {
		return
	}
	if err := p.topo.SweepOrphans(ctx); err != nil {
		slog.WarnContext(ctx, "ListInstances: orphan sweep failed (continuing)", "error", err)
	}
}

// runCacheGCThrottled runs the cache GC at most once per cacheGCInterval across
// provider processes, whichever of ListInstances and CreateInstance reaches it first.
func (p *Provider) runCacheGCThrottled(ctx context.Context) {
	if p.claimRound(ctx, p.cacheGCStamp()) {
		p.runCacheGC(ctx)
	}
}

// claimRound claims a round of st's pass. A stamp needs the runner image, so without
// one locally the pass runs unthrottled: housekeeping is never skipped for want of an
// image. Any other daemon error skips the round.
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

// claimStamp reports whether this process won a round of st's pass. The provider is a
// fresh process per call with no state directory, so the time of the last round lives
// in Docker: a never-started stamp container whose created-at label is when that round
// began (ADR-003, Garbage collection). A daemon error skips the round; nothing here
// fails the caller.
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
	// Remove by ID, not name, so a slow peer cannot delete the winner's fresh stamp.
	if err := p.cli.ContainerRemove(ctx, existing.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		if errdefs.IsConflict(err) {
			// "removal of container ... is already in progress": a peer found the same
			// stamp expired and owns this round, so losing the claim is not a warning.
			slog.DebugContext(ctx, "throttle: a peer is replacing the stamp; skipping this pass",
				"pass", st.what, "resource", "container", "container_name", st.name, "error", err)
			return false
		}
		slog.WarnContext(ctx, "throttle: failed to replace the stamp; skipping this pass",
			"pass", st.what, "resource", "container", "container_name", st.name, "error", err)
		return false
	}
	// A stamp with no usable time is replaced without running: it delays the pass by
	// one interval and never causes a storm.
	if !known || age < 0 {
		if p.createStamp(ctx, st, image) {
			slog.WarnContext(ctx, "throttle: the stamp had no usable time; reset it, the next round runs one interval from now",
				"pass", st.what, "resource", "container", "container_name", st.name, "interval", st.interval.String())
		}
		return false
	}
	return p.createStamp(ctx, st, image)
}

// createStamp creates st's stamp and reports whether this process owns the round. The
// create is the atomic claim: Docker answers a second container of the same name with a
// 409.
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
