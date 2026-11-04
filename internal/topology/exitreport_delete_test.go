package topology

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// deleteExitLine matches the line a DeleteInstance teardown logs.
const deleteExitLine = `msg="teardown: runner was not running at delete"`

// spyClient counts the report's Docker calls on top of the fake, can hold the
// logs read until the context ends, and records what the log held when the
// teardown first stopped or removed a container.
type spyClient struct {
	*docker.FakeClient
	logs       *bytes.Buffer
	stallLogs  bool
	inspects   int
	logReads   int
	logsAtStop string
	stopped    bool

	// stallNetworkListCall, when non-zero, is the NetworkList call to hold until
	// the context ends, or for stallGiveUp on a context that never does.
	stallNetworkListCall int
	networkLists         int
}

// stallGiveUp ends a stall the report's budget failed to end, so a broken bound
// fails its test on elapsed time instead of hanging the run.
const stallGiveUp = exitReportTimeout + 10*time.Second

func (s *spyClient) NetworkList(ctx context.Context, opts network.ListOptions) ([]network.Summary, error) {
	s.networkLists++
	if s.networkLists == s.stallNetworkListCall {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(stallGiveUp):
		}
	}
	return s.FakeClient.NetworkList(ctx, opts)
}

func (s *spyClient) ContainerInspect(ctx context.Context, id string) (types.ContainerJSON, error) {
	s.inspects++
	return s.FakeClient.ContainerInspect(ctx, id)
}

func (s *spyClient) ContainerLogs(ctx context.Context, id string, opts container.LogsOptions) (io.ReadCloser, error) {
	s.logReads++
	if s.stallLogs {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.FakeClient.ContainerLogs(ctx, id, opts)
}

func (s *spyClient) noteStop() {
	if !s.stopped {
		s.stopped, s.logsAtStop = true, s.logs.String()
	}
}

func (s *spyClient) ContainerStop(ctx context.Context, id string, opts container.StopOptions) error {
	s.noteStop()
	return s.FakeClient.ContainerStop(ctx, id, opts)
}

func (s *spyClient) ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error {
	s.noteStop()
	return s.FakeClient.ContainerRemove(ctx, id, opts)
}

func newSpyManager(t *testing.T) (*Manager, *spyClient) {
	t.Helper()
	spy := &spyClient{FakeClient: docker.NewFakeClient(), logs: captureLogs(t)}
	return New(spy, testControllerID), spy
}

// deleteAndCheck runs the DeleteInstance teardown and fails unless it removed
// the whole allocation and returned (true, nil), whatever the report did.
func deleteAndCheck(t *testing.T, m *Manager, fake *docker.FakeClient, name string) {
	t.Helper()
	found, err := m.TeardownAllocationReportingIO(context.Background(), name)
	if err != nil || !found {
		t.Fatalf("TeardownAllocationReportingIO = (%v, %v), want (true, nil)", found, err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("teardown left resources behind")
	}
}

// TestDeleteReportsExitedRunnerBeforeRemoving pins the line that keeps the
// evidence of a mid-job crash: GARM deletes the instance as soon as the job's
// completed message arrives, so this is the only record of the exit.
func TestDeleteReportsExitedRunnerBeforeRemoving(t *testing.T) {
	m, spy := newSpyManager(t)
	id, finishedAt := seedExitedRunner(t, spy.FakeClient, "job-1", 137, true)
	spy.AppendLog(id, docker.LogStdout, listeningLine)
	spy.AppendLog(id, docker.LogStdout, runningLine)

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	out := spy.logs.String()
	if n := strings.Count(out, deleteExitLine); n != 1 {
		t.Fatalf("got %d delete exit lines, want exactly 1:\n%s", n, out)
	}
	if !strings.Contains(spy.logsAtStop, deleteExitLine) {
		t.Errorf("the line was not logged before the first stop or remove:\n%s", spy.logsAtStop)
	}
	line := lineWith(t, out, deleteExitLine)
	for _, want := range []string{
		"level=WARN",
		"instance=job-1",
		"pool=pool-1",
		"state=exited",
		"exit_code=137",
		"finished_at=" + finishedAt.Format(time.RFC3339Nano),
		"oom_killed=true",
		"runner_phase=" + phaseJobStarted,
		"log_tail_withheld=0",
		"Running job: build",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("delete exit line lacks %q:\n%s", want, line)
		}
	}
	if n := countExitLines(out); n != 0 {
		t.Errorf("a DeleteInstance teardown logged the sweep's line:\n%s", out)
	}
}

// TestDeleteExitLineReportsStoppedDindSidecar pins that the delete line carries
// the sidecar fields the sweep's does.
func TestDeleteExitLineReportsStoppedDindSidecar(t *testing.T) {
	m, spy := newSpyManager(t)
	_, old := seedExitedRunner(t, spy.FakeClient, "job-1", 2, false)
	sidecar := seedDindSidecarFor(t, spy.FakeClient, testControllerID, "job-1", old, "n1")
	spy.SetState(sidecar, "exited", false)
	spy.SetExitCode(sidecar, 1)

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	line := lineWith(t, spy.logs.String(), deleteExitLine)
	for _, want := range []string{"dind_state=exited", "dind_exit_code=1", "dind_oom_killed=false", "dind_finished_at="} {
		if !strings.Contains(line, want) {
			t.Errorf("delete exit line lacks %q:\n%s", want, line)
		}
	}
}

// TestDeleteExitLineLevels pins that only the normal ephemeral end stays out of
// a warn-level log.
func TestDeleteExitLineLevels(t *testing.T) {
	cases := []struct {
		name  string
		exit  int
		oom   bool
		lines []string
		level string
	}{
		{"job completed, exit 0", 0, false, []string{runningLine, completedLine}, "level=DEBUG"},
		{"job completed, exit 1", 1, false, []string{runningLine, completedLine}, "level=WARN"},
		{"job completed, OOM with exit 0", 0, true, []string{runningLine, completedLine}, "level=WARN"},
		{"job started, exit 0", 0, false, []string{runningLine}, "level=WARN"},
		{"listening, exit 0", 0, false, []string{listeningLine}, "level=WARN"},
		{"before listening, exit 0", 0, false, nil, "level=WARN"},
		{"tail past the markers, exit 0", 0, false, fullTail(), "level=WARN"},
		{"job started, exit 137", 137, false, []string{runningLine}, "level=WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, spy := newSpyManager(t)
			id, _ := seedExitedRunner(t, spy.FakeClient, "job-1", tc.exit, tc.oom)
			for _, l := range tc.lines {
				spy.AppendLog(id, docker.LogStdout, l)
			}

			deleteAndCheck(t, m, spy.FakeClient, "job-1")

			line := lineWith(t, spy.logs.String(), deleteExitLine)
			if !strings.Contains(line, tc.level) {
				t.Errorf("delete exit line lacks %s:\n%s", tc.level, line)
			}
		})
	}
}

// TestDeleteOfRunningRunnerReportsNothing pins the ordinary scale-down: no line
// and not one extra Docker call.
func TestDeleteOfRunningRunnerReportsNothing(t *testing.T) {
	m, spy := newSpyManager(t)
	old := time.Now().Add(-time.Hour).UTC()
	seedNetworkFor(t, spy.FakeClient, testControllerID, "job-1", old, "n1")
	id := seedRunnerFor(t, spy.FakeClient, testControllerID, "job-1", old, "n1", "running")
	spy.AppendLog(id, docker.LogStdout, listeningLine)

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	if out := spy.logs.String(); strings.Contains(out, deleteExitLine) {
		t.Errorf("a running runner was reported:\n%s", out)
	}
	if spy.inspects != 0 || spy.logReads != 0 {
		t.Errorf("a running runner cost %d inspects and %d log reads, want none", spy.inspects, spy.logReads)
	}
}

// TestDeleteOfRunningRunnerAddsNoNetworkList pins the same for an allocation
// with no claim network, where the removal check reads the network list: the
// report must not run that check for a running runner.
func TestDeleteOfRunningRunnerAddsNoNetworkList(t *testing.T) {
	lists := func(teardown func(*Manager) (bool, error)) int {
		m, spy := newSpyManager(t)
		seedRunnerFor(t, spy.FakeClient, testControllerID, "job-1", time.Now().UTC(), "n1", "running")
		if _, err := teardown(m); err != nil {
			t.Fatalf("teardown returned unexpected error: %v", err)
		}
		return spy.networkLists
	}
	plain := lists(func(m *Manager) (bool, error) { return m.TeardownAllocation(context.Background(), "job-1") })
	reporting := lists(func(m *Manager) (bool, error) {
		return m.TeardownAllocationReportingIO(context.Background(), "job-1")
	})
	if reporting != plain {
		t.Errorf("the reporting teardown made %d NetworkList calls, the plain one %d", reporting, plain)
	}
}

// TestDeleteExitReportSkipsOtherPaths pins who does not report: the teardown the
// sweep runs after its own line, a runner this teardown would not remove, and a
// runner another teardown is already removing.
func TestDeleteExitReportSkipsOtherPaths(t *testing.T) {
	t.Run("plain teardown", func(t *testing.T) {
		m, spy := newSpyManager(t)
		seedExitedRunner(t, spy.FakeClient, "job-1", 1, false)

		if _, err := m.TeardownAllocation(context.Background(), "job-1"); err != nil {
			t.Fatalf("TeardownAllocation returned unexpected error: %v", err)
		}
		if out := spy.logs.String(); strings.Contains(out, deleteExitLine) {
			t.Errorf("a plain teardown reported the runner:\n%s", out)
		}
		if spy.inspects != 0 || spy.logReads != 0 {
			t.Errorf("a plain teardown cost %d inspects and %d log reads, want none", spy.inspects, spy.logReads)
		}
	})
	t.Run("another generation", func(t *testing.T) {
		m, spy := newSpyManager(t)
		old := time.Now().Add(-time.Hour).UTC()
		seedNetworkFor(t, spy.FakeClient, testControllerID, "job-1", old, "n2")
		seedRunnerFor(t, spy.FakeClient, testControllerID, "job-1", old, "n1", "exited")

		// The error is not under test: the claim network cannot go while the
		// other generation's runner, which stays, is still attached to it.
		_, _ = m.TeardownAllocationReportingIO(context.Background(), "job-1")

		if out := spy.logs.String(); strings.Contains(out, deleteExitLine) {
			t.Errorf("a runner of another generation was reported:\n%s", out)
		}
		if countContainers(t, spy.FakeClient) != 1 {
			t.Errorf("a runner of another generation was removed")
		}
	})
	t.Run("already being removed", func(t *testing.T) {
		m, spy := newSpyManager(t)
		id, _ := seedExitedRunner(t, spy.FakeClient, "job-1", 1, false)
		spy.SetState(id, "removing", false)

		deleteAndCheck(t, m, spy.FakeClient, "job-1")

		if out := spy.logs.String(); strings.Contains(out, deleteExitLine) {
			t.Errorf("a runner already being removed was reported:\n%s", out)
		}
		if spy.logReads != 0 {
			t.Errorf("a runner already being removed cost %d log reads, want none", spy.logReads)
		}
	})
}

// TestExitIsLoggedOnceWhicheverPathRunsFirst pins that the sweep and
// DeleteInstance do not both log one exit: the first removes the container.
func TestExitIsLoggedOnceWhicheverPathRunsFirst(t *testing.T) {
	t.Run("sweep first", func(t *testing.T) {
		logs := captureLogs(t)
		m, fake := newManager(t)
		seedExitedRunner(t, fake, "job-1", 1, false)

		if err := m.SweepOrphans(context.Background()); err != nil {
			t.Fatalf("SweepOrphans returned unexpected error: %v", err)
		}
		found, err := m.TeardownAllocationReportingIO(context.Background(), "job-1")
		if err != nil || found {
			t.Fatalf("TeardownAllocationReportingIO = (%v, %v), want (false, nil)", found, err)
		}
		out := logs.String()
		if countExitLines(out) != 1 || strings.Contains(out, deleteExitLine) {
			t.Errorf("want one sweep line and no delete line:\n%s", out)
		}
	})
	t.Run("delete first", func(t *testing.T) {
		logs := captureLogs(t)
		m, fake := newManager(t)
		seedExitedRunner(t, fake, "job-1", 1, false)

		if _, err := m.TeardownAllocationReportingIO(context.Background(), "job-1"); err != nil {
			t.Fatalf("TeardownAllocationReportingIO returned unexpected error: %v", err)
		}
		if err := m.SweepOrphans(context.Background()); err != nil {
			t.Fatalf("SweepOrphans returned unexpected error: %v", err)
		}
		out := logs.String()
		if countExitLines(out) != 0 || strings.Count(out, deleteExitLine) != 1 {
			t.Errorf("want one delete line and no sweep line:\n%s", out)
		}
	})
}

// TestDeleteExitReportStopsAtItsBudget pins the bound on the added latency: a
// daemon that never answers the logs read costs the report, and the teardown
// goes on with its result unchanged.
func TestDeleteExitReportStopsAtItsBudget(t *testing.T) {
	m, spy := newSpyManager(t)
	seedExitedRunner(t, spy.FakeClient, "job-1", 1, false)
	spy.stallLogs = true

	start := time.Now()
	deleteAndCheck(t, m, spy.FakeClient, "job-1")
	elapsed := time.Since(start)

	if elapsed < exitReportTimeout || elapsed > exitReportTimeout+5*time.Second {
		t.Errorf("teardown took %v, want about the %v budget", elapsed, exitReportTimeout)
	}
	line := lineWith(t, spy.logs.String(), deleteExitLine)
	for _, want := range []string{"level=WARN", "exit_code=1", "runner_phase=" + phaseUnknown, "log_tail_error=", "deadline exceeded"} {
		if !strings.Contains(line, want) {
			t.Errorf("delete exit line lacks %q:\n%s", want, line)
		}
	}
}

// TestDeleteExitReportBudgetCoversEveryRunner pins that the budget is one per
// teardown, not one per runner: leftovers of several runners do not add up.
func TestDeleteExitReportBudgetCoversEveryRunner(t *testing.T) {
	m, spy := newSpyManager(t)
	_, old := seedExitedRunner(t, spy.FakeClient, "job-1", 1, false)
	labels := identityFor("job-1").ContainerLabels(spec.RoleRunner, old)
	labels[spec.LabelCreateNonce] = "n1"
	second, err := spy.ContainerCreate(context.Background(), &container.Config{Labels: labels}, nil, nil, nil, "job-1-second")
	if err != nil {
		t.Fatalf("ContainerCreate: %v", err)
	}
	spy.SetState(second.ID, "exited", false)
	spy.stallLogs = true

	start := time.Now()
	deleteAndCheck(t, m, spy.FakeClient, "job-1")
	elapsed := time.Since(start)

	if elapsed >= 2*exitReportTimeout {
		t.Errorf("teardown took %v, want one %v budget across both runners", elapsed, exitReportTimeout)
	}
	if n := strings.Count(spy.logs.String(), deleteExitLine); n != 2 {
		t.Errorf("got %d delete exit lines, want one per stopped runner:\n%s", n, spy.logs.String())
	}
}

// TestDeleteExitReportBudgetCoversTheRemovalCheck pins that the removal check's
// live read, made when the allocation has no claim network, is inside the
// budget, and that the removal loop still decides for itself when it fails.
func TestDeleteExitReportBudgetCoversTheRemovalCheck(t *testing.T) {
	m, spy := newSpyManager(t)
	id := seedRunnerFor(t, spy.FakeClient, testControllerID, "job-1", time.Now().Add(-time.Hour).UTC(), "n1", "exited")
	spy.SetExitCode(id, 1)
	spy.stallNetworkListCall = 2 // the first is the teardown's claim read

	start := time.Now()
	deleteAndCheck(t, m, spy.FakeClient, "job-1")
	elapsed := time.Since(start)

	if elapsed < exitReportTimeout || elapsed >= stallGiveUp {
		t.Errorf("teardown took %v, want about the %v budget", elapsed, exitReportTimeout)
	}
	if out := spy.logs.String(); strings.Contains(out, deleteExitLine) {
		t.Errorf("reported a runner whose removal check failed:\n%s", out)
	}
}

// TestDeleteExitLineWhenLogsUnreadable pins a logs failure: the exit details
// are still logged, with the reason, and the teardown result is unchanged.
func TestDeleteExitLineWhenLogsUnreadable(t *testing.T) {
	m, spy := newSpyManager(t)
	seedExitedRunner(t, spy.FakeClient, "job-1", 3, false)
	spy.LogsErr = errors.New("simulated logs failure")

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	line := lineWith(t, spy.logs.String(), deleteExitLine)
	for _, want := range []string{"level=WARN", "exit_code=3", "runner_phase=" + phaseUnknown, "log_tail_error=", "simulated logs failure"} {
		if !strings.Contains(line, want) {
			t.Errorf("delete exit line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "log_tail=") {
		t.Errorf("delete exit line carries a tail it could not read:\n%s", line)
	}
}

// TestDeleteExitLineWhenInspectFails pins that an inspect failure, which the
// list cannot make up for, still leaves a line and changes nothing else.
func TestDeleteExitLineWhenInspectFails(t *testing.T) {
	m, spy := newSpyManager(t)
	seedExitedRunner(t, spy.FakeClient, "job-1", 3, false)
	spy.InspectErr = errors.New("simulated inspect failure")

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	line := lineWith(t, spy.logs.String(), deleteExitLine)
	for _, want := range []string{"level=WARN", "instance=job-1", "pool=pool-1", "state=exited", "runner_phase=" + phaseUnknown, "inspect_error=", "simulated inspect failure"} {
		if !strings.Contains(line, want) {
			t.Errorf("delete exit line lacks %q:\n%s", want, line)
		}
	}
	if spy.logReads != 0 {
		t.Errorf("read the logs of a runner it could not inspect")
	}
}

// TestDeleteExitLineSkippedWhenRunnerAlreadyGone pins a runner removed between
// the list and the inspect: whoever removed it has logged, so there is no line
// and no log read.
func TestDeleteExitLineSkippedWhenRunnerAlreadyGone(t *testing.T) {
	m, spy := newSpyManager(t)
	seedExitedRunner(t, spy.FakeClient, "job-1", 3, false)
	spy.InspectErr = errdefs.NotFound(errors.New("no such container"))

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	if out := spy.logs.String(); strings.Contains(out, deleteExitLine) {
		t.Errorf("a runner that is already gone was reported:\n%s", out)
	}
	if spy.logReads != 0 {
		t.Errorf("read the logs of a runner that is already gone")
	}
}

// TestDeleteExitLineLeaksNoSecrets is the redaction proof for the delete path,
// on the same fixture as the sweep's.
func TestDeleteExitLineLeaksNoSecrets(t *testing.T) {
	m, spy := newSpyManager(t)
	id, _ := seedExitedRunner(t, spy.FakeClient, "job-1", 1, false)
	secrets := appendSecretLogLines(spy.FakeClient, id)

	deleteAndCheck(t, m, spy.FakeClient, "job-1")

	out := spy.logs.String()
	assertNoSecretMaterial(t, out, secrets)
	line := lineWith(t, out, deleteExitLine)
	if !strings.Contains(line, "Listening for Jobs") || !strings.Contains(line, "log_tail_withheld=11") {
		t.Errorf("delete exit line should keep the harmless line and count 11 withheld:\n%s", line)
	}
}
