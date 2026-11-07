package topology

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// exitReportTimeout bounds the Docker calls one exit report makes. The reports run
// inside ListInstances, CreateInstance and DeleteInstance, so a slow daemon costs the
// report and not the call.
const exitReportTimeout = 2 * time.Second

// exitLogTailLines is how many of the runner's last log lines the report asks
// the daemon for.
const exitLogTailLines = 20

// exitLogReadLimit caps the bytes read from the runner's log stream. Twenty
// lines fit in it many times over; it bounds what a runner printing very long
// lines can make a report read.
const exitLogReadLimit = 64 << 10

// exitLogLineLimit caps each logged line, in bytes.
const exitLogLineLimit = 240

// The values of runner_phase: how far a runner got, as classifyRunnerPhase reads
// it from the runner's log tail.
const (
	phaseBeforeListening = "before_listening"
	phaseListeningNoJob  = "listening_no_job"
	phaseJobStarted      = "job_started"
	phaseJobCompleted    = "job_completed"
	phaseUnknown         = "unknown"
)

// deleteExitLogMsg is the line a DeleteInstance teardown logs about a runner
// that was not running. The sweep's own line says "orphan sweep", so a reader
// can tell which path reached the runner first.
const deleteExitLogMsg = "teardown: runner was not running at delete"

// runnerProgressLine matches the lines the actions/runner listener prints as it
// advances, each after a timestamp and ": ": "Listening for Jobs", "Running job:
// <name>" and "Job <name> completed with result: <result>". It is anchored at
// the start of the line or just after a ": ", and the leftmost match wins, so a
// job's display name, which is the workflow author's text and lands inside the
// last two, cannot pass for another marker.
var runnerProgressLine = regexp.MustCompile(`(?:^|: )(Listening for Jobs|Running job: |Job .+ completed with result: )`)

// exitedRunner is a runner container a report describes: its list summary and
// its inspect (for the sweep, the one it aged the runner with).
type exitedRunner struct {
	summary types.Container
	info    types.ContainerJSON
}

// runnerTail is what a report keeps of a runner's log: the screened lines, how
// many were withheld, whether the byte cap cut the stream short, and the phase
// the lines showed before they were screened.
type runnerTail struct {
	lines     []string
	withheld  int
	truncated bool
	phase     string
}

// reportExitedRunner logs one line about a runner that exited on its own, before the
// sweep's teardown removes the container and with it the exit state and logs: its exit
// details, its DinD sidecar's if that stopped too, the phase it reached, and its log
// tail, screened so no credential reaches the log. When the logs cannot be read the
// line is at warn with the reason in place of the tail. It is log-only and bounded by
// exitReportTimeout.
func (m *Manager) reportExitedRunner(ctx context.Context, r exitedRunner, containers []types.Container) {
	ctx, cancel := context.WithTimeout(ctx, exitReportTimeout)
	defer cancel()

	attrs, _, err := m.exitReportAttrs(ctx, r, containers)
	if err != nil {
		slog.WarnContext(ctx, "orphan sweep: runner exited on its own; its log tail is unavailable", attrs...)
		return
	}
	slog.InfoContext(ctx, "orphan sweep: runner exited on its own", attrs...)
}

// reportStoppedRunners logs, for a DeleteInstance teardown, one line about each stopped
// runner in list that scope lets the teardown remove, since the removal takes its exit
// state and logs with it. GARM deletes an instance as soon as its job's completed
// message arrives, well inside the sweep's exited-runner grace, so a runner that
// crashed mid-job reaches this path and never the sweep's line.
func (m *Manager) reportStoppedRunners(ctx context.Context, list []types.Container, scope *teardownScope) {
	if !scope.reportOnDelete {
		return
	}
	// One budget for the whole report, the removal check's live read included: it is
	// the delay this adds to DeleteInstance.
	ctx, cancel := context.WithTimeout(ctx, exitReportTimeout)
	defer cancel()

	for _, c := range list {
		// Running: an ordinary scale-down, and it costs no Docker call. Removing:
		// another removal has it and has logged. Created: never started, so no exit or
		// logs to lose.
		if c.Labels[spec.LabelRole] != spec.RoleRunner || c.State == "running" || c.State == "removing" || c.State == "created" {
			continue
		}
		// A failed removal check is left to the removal loop to report.
		if ok, err := scope.mayRemove(ctx, c.Labels); err != nil || !ok {
			continue
		}
		m.reportStoppedRunner(ctx, c, list)
	}
}

// reportStoppedRunner logs the delete line for the runner c. The list summary has
// neither the exit details nor the TTY flag the log read needs, so it inspects first. A
// completed job with a clean exit logs at debug, anything else at warn.
func (m *Manager) reportStoppedRunner(ctx context.Context, c types.Container, list []types.Container) {
	info, err := m.cli.ContainerInspect(ctx, c.ID)
	if errdefs.IsNotFound(err) {
		slog.DebugContext(ctx, "teardown: runner already gone, not reported", "instance", c.Labels[spec.LabelInstanceName])
		return
	}
	if err != nil {
		slog.WarnContext(ctx, deleteExitLogMsg,
			"instance", c.Labels[spec.LabelInstanceName],
			"pool", c.Labels[spec.LabelPoolID],
			"state", c.State,
			"runner_phase", phaseUnknown,
			"inspect_error", err.Error(),
		)
		return
	}

	attrs, phase, err := m.exitReportAttrs(ctx, exitedRunner{summary: c, info: info}, list)
	if err == nil && isNormalRunnerEnd(info.State, phase) {
		slog.DebugContext(ctx, deleteExitLogMsg, attrs...)
		return
	}
	slog.WarnContext(ctx, deleteExitLogMsg, attrs...)
}

// isNormalRunnerEnd reports whether a stopped runner ended the way an ephemeral
// runner should: its job completed and it exited cleanly.
func isNormalRunnerEnd(st *types.ContainerState, phase string) bool {
	return st != nil && st.ExitCode == 0 && !st.OOMKilled && phase == phaseJobCompleted
}

// exitReportAttrs builds the fields the sweep's line and the delete line share.
// When the logs cannot be read it returns the error too, with the reason in
// place of the tail and the phase unknown.
func (m *Manager) exitReportAttrs(ctx context.Context, r exitedRunner, containers []types.Container) ([]any, string, error) {
	attrs := []any{
		"instance", r.summary.Labels[spec.LabelInstanceName],
		"pool", r.summary.Labels[spec.LabelPoolID],
	}
	attrs = append(attrs, exitStateAttrs("", r.info.State)...)
	attrs = append(attrs, m.dindExitAttrs(ctx, containers)...)

	tty := r.info.Config != nil && r.info.Config.Tty
	tail, err := m.runnerLogTail(ctx, r.summary.ID, tty)
	if err != nil {
		attrs = append(attrs, "runner_phase", phaseUnknown, "log_tail_error", err.Error())
		return attrs, phaseUnknown, err
	}
	attrs = append(attrs,
		"runner_phase", tail.phase,
		"log_tail", strings.Join(tail.lines, "\n"),
		"log_tail_withheld", tail.withheld,
	)
	if tail.truncated {
		attrs = append(attrs, "log_tail_truncated", true)
	}
	return attrs, tail.phase, nil
}

// classifyRunnerPhase reads how far a runner got from its raw log tail: the latest
// marker decides. It returns only a phase constant, since it reads lines the screen
// would withhold and nothing of a line may leave it.
func classifyRunnerPhase(rawLines []string, truncated bool) string {
	for i := len(rawLines) - 1; i >= 0; i-- {
		match := runnerProgressLine.FindStringSubmatch(rawLines[i])
		if match == nil {
			continue
		}
		switch {
		case strings.HasPrefix(match[1], "Job "):
			return phaseJobCompleted
		// The byte cap keeps the oldest lines, so a later marker may be gone: only
		// job_completed, the last marker a job can print, stands.
		case truncated:
			return phaseUnknown
		case strings.HasPrefix(match[1], "Running job"):
			return phaseJobStarted
		default:
			return phaseListeningNoJob
		}
	}
	// No marker: the runner never listened, unless a full tail scrolled past the
	// markers or a truncated one lost them.
	if truncated || len(rawLines) >= exitLogTailLines {
		return phaseUnknown
	}
	return phaseBeforeListening
}

// exitStateAttrs returns the exit details of a stopped container, each key
// prefixed with prefix. The daemon's own State.Error, when set, is screened
// like a log line.
func exitStateAttrs(prefix string, st *types.ContainerState) []any {
	if st == nil {
		return []any{prefix + "state", "unknown"}
	}
	attrs := []any{
		prefix + "state", st.Status,
		prefix + "exit_code", st.ExitCode,
		prefix + "finished_at", st.FinishedAt,
		prefix + "oom_killed", st.OOMKilled,
	}
	if st.Error != "" {
		safe, _ := logging.SafeLogLines([]string{st.Error}, exitLogLineLimit)
		attrs = append(attrs, prefix+"error", safe[0])
	}
	return attrs
}

// dindExitAttrs returns the exit details of the allocation's DinD sidecar when
// it has stopped too, only "dind_state" while it still runs, and nothing for
// an allocation without one.
func (m *Manager) dindExitAttrs(ctx context.Context, containers []types.Container) []any {
	for _, c := range containers {
		if !spec.MatchesPredicate(c.Labels, m.controllerID) || c.Labels[spec.LabelRole] != spec.RoleDind {
			continue
		}
		if c.State == "running" {
			return []any{"dind_state", c.State}
		}
		info, err := m.cli.ContainerInspect(ctx, c.ID)
		if err != nil {
			return []any{"dind_state", c.State, "dind_inspect_error", err.Error()}
		}
		return exitStateAttrs("dind_", info.State)
	}
	return nil
}

// runnerLogTail reads the last exitLogTailLines lines of the runner's stdout
// and stderr, at most exitLogReadLimit bytes of them, classifies the raw lines
// and screens them with logging.SafeLogLines; the raw lines do not leave it. The
// stream is stdcopy-multiplexed unless the container has a TTY. truncated
// reports that the byte cap cut the stream short; a line it cut through is
// dropped.
func (m *Manager) runnerLogTail(ctx context.Context, id string, tty bool) (runnerTail, error) {
	rc, err := m.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(exitLogTailLines),
	})
	if err != nil {
		return runnerTail{}, fmt.Errorf("failed to open the runner's logs: %w", err)
	}
	defer rc.Close()

	src := &io.LimitedReader{R: rc, N: exitLogReadLimit}
	var buf bytes.Buffer
	if tty {
		_, err = io.Copy(&buf, src)
	} else {
		_, err = stdcopy.StdCopy(&buf, &buf, src)
	}
	if err != nil {
		return runnerTail{}, fmt.Errorf("failed to read the runner's logs: %w", err)
	}
	truncated := src.N <= 0

	out := buf.String()
	if truncated && !strings.HasSuffix(out, "\n") {
		if i := strings.LastIndexByte(out, '\n'); i >= 0 {
			out = out[:i+1]
		} else {
			out = ""
		}
	}
	var lines []string
	if out = strings.TrimSuffix(out, "\n"); out != "" {
		lines = strings.Split(out, "\n")
	}
	if len(lines) > exitLogTailLines {
		lines = lines[len(lines)-exitLogTailLines:]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	phase := classifyRunnerPhase(lines, truncated)
	safe, withheld := logging.SafeLogLines(lines, exitLogLineLimit)
	return runnerTail{lines: safe, withheld: withheld, truncated: truncated, phase: phase}, nil
}
