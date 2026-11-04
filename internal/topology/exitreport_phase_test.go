package topology

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/logging"
)

// The listener's own lines, as actions/runner prints them (Runner.cs,
// JobDispatcher.cs) and internal/logging/tail_test.go keeps them.
const (
	listeningLine = "2026-11-03 10:00:00Z: Listening for Jobs"
	runningLine   = "2026-11-03 10:05:00Z: Running job: build"
	completedLine = "2026-11-03 10:09:30Z: Job build completed with result: Failed"
)

// fullTail returns exitLogTailLines lines of filler ending in last.
func fullTail(last ...string) []string {
	lines := make([]string, 0, exitLogTailLines)
	for len(lines) < exitLogTailLines-len(last) {
		lines = append(lines, "some step output")
	}
	return append(lines, last...)
}

func TestClassifyRunnerPhase(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		truncated bool
		want      string
	}{
		{"no output at all", nil, false, phaseBeforeListening},
		{"entrypoint only", []string{"[entrypoint] dropping privileges to runner via gosu", "√ Connected to GitHub"}, false, phaseBeforeListening},
		{"listening", []string{"√ Connected to GitHub", listeningLine}, false, phaseListeningNoJob},
		{"listening, then an error", []string{listeningLine, "Runner.Listener exited with code 137"}, false, phaseListeningNoJob},
		{"job started", []string{listeningLine, runningLine}, false, phaseJobStarted},
		{"job completed", []string{listeningLine, runningLine, completedLine}, false, phaseJobCompleted},
		{"crlf", []string{listeningLine + "\r", runningLine + "\r"}, false, phaseJobStarted},
		{"no timestamp", []string{"Listening for Jobs"}, false, phaseListeningNoJob},

		// The latest marker decides, so a relaunched listener reads as what it
		// is doing now, and so does a second job on a runner that takes more.
		{"relaunched after a job", []string{runningLine, listeningLine}, false, phaseListeningNoJob},
		{"second job started", []string{completedLine, listeningLine, runningLine}, false, phaseJobStarted},

		// A full tail may have scrolled past the markers, but it keeps the
		// newest lines, so a marker it does show still stands.
		{"full tail, no marker", fullTail(), false, phaseUnknown},
		{"full tail, marker", fullTail(listeningLine), false, phaseListeningNoJob},
		{"one line short of full, no marker", fullTail()[1:], false, phaseBeforeListening},

		// The byte cap keeps the OLDEST lines of the tail and loses the newest,
		// so a later marker may be gone: only the last marker a job can print
		// stands.
		{"truncated, no marker", []string{"some step output"}, true, phaseUnknown},
		{"truncated, nothing kept", nil, true, phaseUnknown},
		{"truncated, listening", []string{listeningLine}, true, phaseUnknown},
		{"truncated, job started", []string{listeningLine, runningLine}, true, phaseUnknown},
		{"truncated, job completed", []string{runningLine, completedLine}, true, phaseJobCompleted},

		// The job's display name is the workflow author's text and lands inside
		// the listener's lines; it cannot move the phase.
		{"job named like the listening line", []string{"2026-11-03 10:05:00Z: Running job: Listening for Jobs"}, false, phaseJobStarted},
		{"job named like the completed line", []string{"2026-11-03 10:05:00Z: Running job: x completed with result: Failed"}, false, phaseJobStarted},
		{"completed job named like the running line", []string{"2026-11-03 10:09:30Z: Job Running job: x completed with result: Failed"}, false, phaseJobCompleted},
		{"completed job named like the listening line", []string{"2026-11-03 10:09:30Z: Job Listening for Jobs completed with result: Failed"}, false, phaseJobCompleted},

		// Marker text in the middle of some other line is not the listener's.
		{"marker text inside prose", []string{"grep found Listening for Jobs in the log", "see Running job: docs", "the Job x completed with result: ok was logged"}, false, phaseBeforeListening},
		{"different case", []string{"listening for jobs", "running job: x"}, false, phaseBeforeListening},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRunnerPhase(tc.lines, tc.truncated); got != tc.want {
				t.Errorf("classifyRunnerPhase(%q, %v) = %q, want %q", tc.lines, tc.truncated, got, tc.want)
			}
		})
	}
}

// TestClassifyRunnerPhaseReadsLinesTheScreenWithholds pins why the classifier
// runs before the screen: each of these lines is withheld from the log, and its
// phase still counts.
func TestClassifyRunnerPhaseReadsLinesTheScreenWithholds(t *testing.T) {
	cases := []struct{ line, want string }{
		{"2026-11-03 10:05:00Z: Running job: rotate-token", phaseJobStarted},
		{"2026-11-03 10:09:30Z: Job rotate-secrets completed with result: Failed", phaseJobCompleted},
		{"2026-11-03 10:00:00Z: Listening for Jobs secret", phaseListeningNoJob},
	}
	for _, tc := range cases {
		if _, withheld := logging.SafeLogLines([]string{tc.line}, exitLogLineLimit); withheld != 1 {
			t.Fatalf("%q was not withheld, so this case proves nothing", tc.line)
		}
		if got := classifyRunnerPhase([]string{tc.line}, false); got != tc.want {
			t.Errorf("classifyRunnerPhase(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestSweepExitLineCarriesRunnerPhase pins the field on the sweep's line, end to
// end from the runner's logs.
func TestSweepExitLineCarriesRunnerPhase(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"before listening", []string{"[entrypoint] dropping privileges to runner via gosu"}, phaseBeforeListening},
		{"listening", []string{listeningLine}, phaseListeningNoJob},
		{"job started", []string{listeningLine, runningLine}, phaseJobStarted},
		{"job completed", []string{listeningLine, runningLine, completedLine}, phaseJobCompleted},
		{"tail past the markers", fullTail(), phaseUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			m, fake := newManager(t)
			id, _ := seedExitedRunner(t, fake, "job-1", 1, false)
			for _, l := range tc.lines {
				fake.AppendLog(id, docker.LogStdout, l)
			}

			if err := m.SweepOrphans(context.Background()); err != nil {
				t.Fatalf("SweepOrphans returned unexpected error: %v", err)
			}
			line := lineWith(t, logs.String(), exitLogMsg)
			if !strings.Contains(line, "runner_phase="+tc.want) {
				t.Errorf("exit line lacks runner_phase=%s:\n%s", tc.want, line)
			}
		})
	}
}

// TestSweepExitLinePhaseFromWithheldMarker pins the screen's side of it: the
// phase comes from a line the screen withholds, and the line stays withheld.
func TestSweepExitLinePhaseFromWithheldMarker(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	id, _ := seedExitedRunner(t, fake, "job-1", 1, false)
	fake.AppendLog(id, docker.LogStdout, listeningLine)
	fake.AppendLog(id, docker.LogStdout, "2026-11-03 10:05:00Z: Running job: rotate-token")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	line := lineWith(t, logs.String(), exitLogMsg)
	for _, want := range []string{"runner_phase=" + phaseJobStarted, "log_tail_withheld=1", "line withheld"} {
		if !strings.Contains(line, want) {
			t.Errorf("exit line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "rotate-token") {
		t.Errorf("the withheld line reached the log:\n%s", line)
	}
}

// TestSweepExitLinePhaseOnTruncatedTail pins the truncated case end to end: the
// byte cap cuts the newest lines off, so the job's start is lost and the
// listening line left in the tail does not get to name the phase.
func TestSweepExitLinePhaseOnTruncatedTail(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	id, _ := seedExitedRunner(t, fake, "job-1", 1, false)
	fake.AppendLog(id, docker.LogStdout, listeningLine)
	long := strings.Repeat("progress ", 2000) // ~18 KB a line, past the cap in four
	for i := 0; i < 4; i++ {
		fake.AppendLog(id, docker.LogStdout, long)
	}
	fake.AppendLog(id, docker.LogStdout, runningLine)

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	line := lineWith(t, logs.String(), exitLogMsg)
	for _, want := range []string{"runner_phase=" + phaseUnknown, "log_tail_truncated=true", "Listening for Jobs"} {
		if !strings.Contains(line, want) {
			t.Errorf("exit line lacks %q:\n%s", want, line)
		}
	}
}

// TestSweepExitLinePhaseUnknownWhenLogsUnreadable pins the unreadable case.
func TestSweepExitLinePhaseUnknownWhenLogsUnreadable(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	seedExitedRunner(t, fake, "job-1", 3, false)
	fake.LogsErr = errors.New("simulated logs failure")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	line := lineWith(t, logs.String(), exitLogMsg)
	if !strings.Contains(line, "runner_phase="+phaseUnknown) {
		t.Errorf("exit line lacks runner_phase=%s:\n%s", phaseUnknown, line)
	}
}
