package topology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// dindIOReportTimeout bounds the whole DinD IO report for one teardown: the
// runner inspect, the sidecar inspect, the stats sample and the image probe
// share it. The report is a measurement aid (ADR-006 rollout step 0), so when
// a stalled disk makes the daemon slow it gives up rather than hold up
// the teardown.
const dindIOReportTimeout = 4 * time.Second

// dindIOReporter logs one line per DinD sidecar about to be torn down: how
// much the sidecar wrote to block devices over its life, and what its daemon
// holds, so an operator can compare the two (ADR-006, phase 0). It is
// best-effort and log-only; nothing it does can fail or change a teardown.
type dindIOReporter struct {
	m        *Manager
	deadline time.Time
	repo     string
}

// newDindIOReporter returns a reporter for a teardown of list, or nil when no
// report applies: the scope does not ask for one (only DeleteInstance does) or
// the allocation has no DinD sidecar, so a non-DinD teardown does no extra
// Docker calls. The runner's repository is read here, while the runner still
// exists: the runner is removed before the sidecar (ADR-004).
func (m *Manager) newDindIOReporter(ctx context.Context, list []types.Container, scope *teardownScope) *dindIOReporter {
	if !scope.reportDindIO || !hasRole(list, spec.RoleDind) {
		return nil
	}
	r := &dindIOReporter{m: m, deadline: time.Now().Add(dindIOReportTimeout)}
	r.repo = r.runnerRepo(ctx, list)
	return r
}

func hasRole(list []types.Container, role string) bool {
	for _, c := range list {
		if c.Labels[spec.LabelRole] == role {
			return true
		}
	}
	return false
}

// runnerRepo reads the repository the allocation's runner registers against
// from its environment, or "" if it cannot (the runner is gone, the inspect
// fails, the pool is not repository scoped).
func (r *dindIOReporter) runnerRepo(ctx context.Context, list []types.Container) string {
	for _, c := range list {
		if c.Labels[spec.LabelRole] != spec.RoleRunner {
			continue
		}
		ctx, cancel := context.WithDeadline(ctx, r.deadline)
		defer cancel()
		info, err := r.m.cli.ContainerInspect(ctx, c.ID)
		if err != nil || info.Config == nil {
			return ""
		}
		return repoFromEnv(info.Config.Env)
	}
	return ""
}

// report logs the IO line for c if it is a DinD sidecar. It is a no-op on a
// nil reporter and on any other container, so removeContainers can call it
// unconditionally. It must run before the sidecar is stopped: stopping tears
// down its cgroup, and with it the counters read here.
func (r *dindIOReporter) report(ctx context.Context, c types.Container) {
	if r == nil || c.Labels[spec.LabelRole] != spec.RoleDind {
		return
	}
	ctx, cancel := context.WithDeadline(ctx, r.deadline)
	defer cancel()

	attrs, err := r.identity(ctx, c)
	if err != nil {
		slog.DebugContext(ctx, "teardown: DinD sidecar IO not reported", "instance", c.Labels[spec.LabelInstanceName], "reason", err.Error())
		return
	}
	written, err := r.writtenBytes(ctx, c.ID)
	if err != nil {
		slog.WarnContext(ctx, "teardown: DinD sidecar IO unavailable", append(attrs, "stats_error", err.Error())...)
		return
	}
	attrs = append(attrs, "written_bytes", written)
	attrs = append(attrs, r.probeAttrs(ctx, c.ID)...)
	slog.InfoContext(ctx, "teardown: DinD sidecar IO", attrs...)
}

// identity builds the log attributes that name the allocation. It errors when
// the sidecar is gone or not running: a stopped sidecar's cgroup, and the
// counters in it, no longer exist, so there is nothing to measure.
func (r *dindIOReporter) identity(ctx context.Context, c types.Container) ([]any, error) {
	info, err := r.m.cli.ContainerInspect(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect sidecar: %w", err)
	}
	if info.State == nil || !info.State.Running || info.HostConfig == nil {
		return nil, errors.New("sidecar is not running")
	}
	attrs := []any{"instance", c.Labels[spec.LabelInstanceName], "pool", c.Labels[spec.LabelPoolID]}
	if r.repo != "" {
		attrs = append(attrs, "repo", r.repo)
	}
	attrs = append(attrs, "dind_mode", spec.DindModeOf(info.HostConfig.Privileged, info.HostConfig.Runtime))
	if created, ok := parseCreatedAt(c.Labels); ok {
		attrs = append(attrs, "since_create_s", int64(r.m.now().Sub(created).Seconds()))
	}
	return attrs, nil
}

// writtenBytes takes one one-shot stats sample of the sidecar and returns its
// cumulative block-device write bytes.
func (r *dindIOReporter) writtenBytes(ctx context.Context, id string) (uint64, error) {
	stats, err := r.m.cli.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("failed to read stats: %w", err)
	}
	defer stats.Body.Close()
	return parseBlkioWriteBytes(stats.Body)
}

// probeAttrs asks the sidecar's daemon what images it holds and returns them as
// log attributes. A failed probe is not fatal to the line: it is reported as
// probe_error, so the written_bytes figure is still logged.
func (r *dindIOReporter) probeAttrs(ctx context.Context, id string) []any {
	out, code, err := r.m.cli.ExecOutput(ctx, id, []string{"sh", "-c", imageProbeScript})
	if err == nil && code != 0 {
		err = fmt.Errorf("probe exited with code %d", code)
	}
	var p imageProbe
	if err == nil {
		p, err = parseImageProbe(out)
	}
	if err != nil {
		return []any{"probe_error", err.Error()}
	}
	attrs := []any{"storage_driver", p.Driver, "image_store", p.Store, "images", p.Images, "image_bytes", p.ImageBytes}
	if p.HasContent {
		attrs = append(attrs, "content_bytes", p.ContentBytes)
	}
	return attrs
}
