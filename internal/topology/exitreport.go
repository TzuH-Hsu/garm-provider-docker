package topology

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// exitReportTimeout bounds the Docker calls one exited-runner report makes:
// the DinD sidecar inspect and the runner's log read. The sweep runs inside
// ListInstances and at the start of CreateInstance, so a daemon that is slow to
// answer costs the report, not the call it runs in.
const exitReportTimeout = 2 * time.Second

// exitLogTailLines is how many of the runner's last log lines the report asks
// the daemon for.
const exitLogTailLines = 20

// exitLogReadLimit caps the bytes read from the runner's log stream. Twenty
// lines fit in it many times over; it bounds what a runner printing very long
// lines can make the sweep read.
const exitLogReadLimit = 64 << 10

// exitLogLineLimit caps each logged line, in bytes.
const exitLogLineLimit = 240

// exitedRunner is a runner container the sweep found exited: its list summary
// and the inspect the sweep aged it with.
type exitedRunner struct {
	summary types.Container
	info    types.ContainerJSON
}

// reportExitedRunner logs one line about a runner that exited on its own,
// before the sweep tears its allocation down and the container, with its exit
// state and logs, is gone: its exit code, State.FinishedAt, OOMKilled, the
// exit details of the allocation's DinD sidecar if that has stopped too, and
// the last exitLogTailLines lines of its output, screened by
// logging.SafeLogLines so no credential reaches the log.
//
// It is best-effort and log-only, bounded by exitReportTimeout: when the logs
// cannot be read the line is logged at warn with the reason in place of the
// tail, and nothing it does changes or fails the sweep.
func (m *Manager) reportExitedRunner(ctx context.Context, r exitedRunner, containers []types.Container) {
	ctx, cancel := context.WithTimeout(ctx, exitReportTimeout)
	defer cancel()

	attrs := []any{
		"instance", r.summary.Labels[spec.LabelInstanceName],
		"pool", r.summary.Labels[spec.LabelPoolID],
	}
	attrs = append(attrs, exitStateAttrs("", r.info.State)...)
	attrs = append(attrs, m.dindExitAttrs(ctx, containers)...)

	tty := r.info.Config != nil && r.info.Config.Tty
	tail, withheld, truncated, err := m.runnerLogTail(ctx, r.summary.ID, tty)
	if err != nil {
		attrs = append(attrs, "log_tail_error", err.Error())
		slog.WarnContext(ctx, "orphan sweep: runner exited on its own; its log tail is unavailable", attrs...)
		return
	}
	attrs = append(attrs, "log_tail", strings.Join(tail, "\n"), "log_tail_withheld", withheld)
	if truncated {
		attrs = append(attrs, "log_tail_truncated", true)
	}
	slog.InfoContext(ctx, "orphan sweep: runner exited on its own", attrs...)
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
// and stderr, at most exitLogReadLimit bytes of them, and screens them with
// logging.SafeLogLines. The stream is stdcopy-multiplexed unless the container
// has a TTY. truncated reports that the byte cap cut the stream short; a line
// it cut through is dropped.
func (m *Manager) runnerLogTail(ctx context.Context, id string, tty bool) (tail []string, withheld int, truncated bool, err error) {
	rc, err := m.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       strconv.Itoa(exitLogTailLines),
	})
	if err != nil {
		return nil, 0, false, fmt.Errorf("failed to open the runner's logs: %w", err)
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
		return nil, 0, false, fmt.Errorf("failed to read the runner's logs: %w", err)
	}
	truncated = src.N <= 0

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
	tail, withheld = logging.SafeLogLines(lines, exitLogLineLimit)
	return tail, withheld, truncated, nil
}
