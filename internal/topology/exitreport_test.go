package topology

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// exitLogMsg is the prefix both the info and the warn exit lines share.
const exitLogMsg = `msg="orphan sweep: runner exited on its own`

// seedExitedRunner seeds a full allocation for name whose runner exited an
// hour ago (past the exited-runner grace) with exitCode and oomKilled, and
// returns the runner's ID and FinishedAt.
func seedExitedRunner(t *testing.T, fake *docker.FakeClient, name string, exitCode int, oomKilled bool) (string, time.Time) {
	t.Helper()
	old := time.Now().Add(-time.Hour).UTC()
	seedNetworkFor(t, fake, testControllerID, name, old, "n1")
	seedWorkspaceVolumeFor(t, fake, testControllerID, name, old, "n1")
	id := seedRunnerFor(t, fake, testControllerID, name, old, "n1", "exited")
	fake.SetState(id, "exited", oomKilled)
	fake.SetExitCode(id, exitCode)
	return id, old
}

func countExitLines(out string) int {
	return strings.Count(out, exitLogMsg)
}

// TestSweepLogsExitedRunnerBeforeTeardown pins the acceptance line: a runner
// that exited on its own and is then swept leaves exactly one line with its
// instance, pool, exit code, FinishedAt, OOMKilled and the last 20 lines of its
// output, and the allocation is still torn down.
func TestSweepLogsExitedRunnerBeforeTeardown(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	id, finishedAt := seedExitedRunner(t, fake, "job-1", 137, true)
	for i := 1; i <= 25; i++ {
		fake.AppendLog(id, docker.LogStdout, fmt.Sprintf("runner line %02d", i))
	}
	fake.AppendLog(id, docker.LogStdout, "2026-11-03 10:00:00Z: Listening for Jobs")
	fake.AppendLog(id, docker.LogStderr, "Runner listener exited with error code 137")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("sweep did not tear the allocation down")
	}

	out := logs.String()
	if n := countExitLines(out); n != 1 {
		t.Fatalf("got %d exit lines, want exactly 1:\n%s", n, out)
	}
	line := lineWith(t, out, exitLogMsg)
	for _, want := range []string{
		"level=INFO",
		"instance=job-1",
		"pool=pool-1",
		"state=exited",
		"exit_code=137",
		"finished_at=" + finishedAt.Format(time.RFC3339Nano),
		"oom_killed=true",
		"runner_phase=" + phaseListeningNoJob,
		"log_tail_withheld=0",
		"runner line 08",
		"runner line 25",
		"Listening for Jobs",
		"Runner listener exited with error code 137",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("exit line lacks %q:\n%s", want, line)
		}
	}
	// Only the last 20 lines: 25 numbered lines plus two more leave 08..25.
	if strings.Contains(line, "runner line 07") {
		t.Errorf("exit line carries more than the last 20 lines:\n%s", line)
	}
	if strings.Contains(line, "dind_") {
		t.Errorf("an allocation without a sidecar reported sidecar fields:\n%s", line)
	}
}

// TestSweepExitLineReportsStoppedDindSidecar pins that a sidecar which stopped
// too has its exit details on the same line, and a running one only its state.
func TestSweepExitLineReportsStoppedDindSidecar(t *testing.T) {
	cases := []struct {
		name    string
		stopped bool
		want    []string
		notWant []string
	}{
		{"stopped", true, []string{"dind_state=exited", "dind_exit_code=1", "dind_oom_killed=false", "dind_finished_at="}, nil},
		{"running", false, []string{"dind_state=running"}, []string{"dind_exit_code"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			m, fake := newManager(t)
			_, old := seedExitedRunner(t, fake, "job-1", 2, false)
			sidecar := seedDindSidecarFor(t, fake, testControllerID, "job-1", old, "n1")
			if tc.stopped {
				fake.SetState(sidecar, "exited", false)
				fake.SetExitCode(sidecar, 1)
			}

			if err := m.SweepOrphans(context.Background()); err != nil {
				t.Fatalf("SweepOrphans returned unexpected error: %v", err)
			}
			line := lineWith(t, logs.String(), exitLogMsg)
			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Errorf("exit line lacks %q:\n%s", want, line)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(line, nw) {
					t.Errorf("exit line has %q:\n%s", nw, line)
				}
			}
			if countContainers(t, fake) != 0 {
				t.Errorf("sweep left containers behind")
			}
		})
	}
}

// appendSecretLogLines writes a runner tail carrying a JIT config, a
// registration token, GitHub tokens, a bearer header, a metadata URL and a
// private key, 11 lines of it risky and the last one harmless, and returns the
// secret values.
func appendSecretLogLines(fake *docker.FakeClient, id string) []string {
	filler := strings.Repeat("Zq8Xw2Pk5Rt7Ym3Nb6Vc", 4)
	jit := base64.StdEncoding.EncodeToString([]byte(`{".runner":"x",".credentials":"y",".credentials_rsaparams":"` + filler + `"}`))
	secrets := []string{
		jit,
		"AABBC" + "D3EFGHIJKLMNOPQRSTUVWX2YZ", // registration-token shaped
		"gh" + "p_" + filler[:36],             // classic PAT
		"gh" + "s_" + filler[2:38],            // installation token
		"github" + "_pat_" + "11AAAAAAA0_" + filler, // fine-grained PAT
		"ey" + "JhbGciOiJIUzI1NiJ9.ey" + "JzdWIiOiJnYXJtIn0." + filler[:40],
		filler[:64], // a PEM body line
	}
	for _, line := range []string{
		"[entrypoint] linking JIT credential files from /run/garm into /actions-runner",
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + secrets[0],
		"./config.sh --unattended --url https://github.com/acme --token " + secrets[1],
		"remote: using " + secrets[2],
		"installation " + secrets[3],
		"pat " + secrets[4],
		"Authorization: Bearer " + secrets[5],
		"GET https://garm.example/api/v1/metadata/runner-registration-token/?sig=" + filler[:16],
		"-----" + "BEGIN OPENSSH PRIVATE " + "KEY-----",
		secrets[6],
		"-----" + "END OPENSSH PRIVATE " + "KEY-----",
		"2026-11-03 10:00:00Z: Listening for Jobs",
	} {
		fake.AppendLog(id, docker.LogStdout, line)
	}
	return secrets
}

// assertNoSecretMaterial fails if any 12-character window of a secret is in out.
func assertNoSecretMaterial(t *testing.T, out string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		for i := 0; i+12 <= len(s); i++ {
			if strings.Contains(out, s[i:i+12]) {
				t.Fatalf("secret material %q reached the log:\n%s", s[i:i+12], out)
			}
		}
	}
}

// TestSweepExitLineLeaksNoSecrets is the end-to-end redaction proof: a runner
// tail carrying credentials reaches the log with none of them.
func TestSweepExitLineLeaksNoSecrets(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	id, _ := seedExitedRunner(t, fake, "job-1", 1, false)
	secrets := appendSecretLogLines(fake, id)

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	out := logs.String()
	assertNoSecretMaterial(t, out, secrets)
	line := lineWith(t, out, exitLogMsg)
	if !strings.Contains(line, "Listening for Jobs") || !strings.Contains(line, "log_tail_withheld=11") {
		t.Errorf("exit line should keep the harmless line and count 11 withheld:\n%s", line)
	}
}

// TestSweepExitLineWhenLogsUnreadable pins best-effort behaviour: when the
// runner's logs cannot be read, the exit details are still logged, at warn,
// with the reason, and the sweep tears the allocation down as before.
func TestSweepExitLineWhenLogsUnreadable(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	seedExitedRunner(t, fake, "job-1", 3, false)
	fake.LogsErr = errors.New("simulated logs failure")

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, fake) != 0 || countNetworks(t, fake) != 0 || countVolumes(t, fake) != 0 {
		t.Errorf("a logs failure kept the sweep from tearing the allocation down")
	}
	out := logs.String()
	if n := countExitLines(out); n != 1 {
		t.Fatalf("got %d exit lines, want exactly 1:\n%s", n, out)
	}
	line := lineWith(t, out, exitLogMsg)
	for _, want := range []string{"level=WARN", "exit_code=3", "log_tail_error=", "simulated logs failure"} {
		if !strings.Contains(line, want) {
			t.Errorf("warn line lacks %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, "log_tail=") {
		t.Errorf("warn line carries a tail it could not read:\n%s", line)
	}
}

// TestSweepExitLineOnlyPastGraceAndOnlyFromSweep pins that the line changes no
// timing: a runner still inside the exited-runner grace is neither reported
// nor removed, and a DeleteInstance teardown of an exited runner logs its own
// line (exitreport_delete_test.go), not this one.
func TestSweepExitLineOnlyPastGraceAndOnlyFromSweep(t *testing.T) {
	t.Run("within grace", func(t *testing.T) {
		logs := captureLogs(t)
		m, fake := newManager(t)
		id, _ := seedExitedRunner(t, fake, "job-1", 0, false)
		fake.SetFinishedAt(id, time.Now().Add(-exitedRunnerGrace+10*time.Second))

		if err := m.SweepOrphans(context.Background()); err != nil {
			t.Fatalf("SweepOrphans returned unexpected error: %v", err)
		}
		if countContainers(t, fake) != 1 {
			t.Errorf("a runner inside the grace window was removed")
		}
		if n := countExitLines(logs.String()); n != 0 {
			t.Errorf("a runner inside the grace window was reported:\n%s", logs.String())
		}
	})
	t.Run("delete teardown", func(t *testing.T) {
		logs := captureLogs(t)
		m, fake := newManager(t)
		seedExitedRunner(t, fake, "job-1", 0, false)

		if _, err := m.TeardownAllocationReportingIO(context.Background(), "job-1"); err != nil {
			t.Fatalf("TeardownAllocationReportingIO returned unexpected error: %v", err)
		}
		if n := countExitLines(logs.String()); n != 0 {
			t.Errorf("a DeleteInstance teardown reported the exited runner:\n%s", logs.String())
		}
	})
}

// TestRunnerLogTailBounds pins the read bounds: a TTY runner's raw stream is
// read without stdcopy, and a stream past the byte cap is cut, flagged, and
// its lines cut to the per-line cap.
func TestRunnerLogTailBounds(t *testing.T) {
	t.Run("tty", func(t *testing.T) {
		m, fake := newManager(t)
		resp, err := fake.ContainerCreate(context.Background(), &container.Config{Tty: true,
			Labels: identityFor("job-1").ContainerLabels(spec.RoleRunner, time.Now())}, nil, nil, nil, "tty-runner")
		if err != nil {
			t.Fatalf("ContainerCreate: %v", err)
		}
		fake.SetState(resp.ID, "exited", false)
		fake.AppendLog(resp.ID, docker.LogStdout, "raw line\r")

		tail, err := m.runnerLogTail(context.Background(), resp.ID, true)
		if err != nil || tail.withheld != 0 || tail.truncated || len(tail.lines) != 1 || tail.lines[0] != "raw line" {
			t.Errorf("runnerLogTail = (%q, %d, %v, %v), want ([raw line], 0, false, nil)", tail.lines, tail.withheld, tail.truncated, err)
		}
	})
	t.Run("byte cap", func(t *testing.T) {
		m, fake := newManager(t)
		id, _ := seedExitedRunner(t, fake, "job-1", 0, false)
		long := strings.Repeat("progress ", 600) // ~5.4 KB a line, 20 lines past the cap
		for i := 0; i < exitLogTailLines; i++ {
			fake.AppendLog(id, docker.LogStdout, long)
		}

		tail, err := m.runnerLogTail(context.Background(), id, false)
		if err != nil {
			t.Fatalf("runnerLogTail: %v", err)
		}
		if !tail.truncated {
			t.Errorf("a stream past %d bytes was not flagged truncated", exitLogReadLimit)
		}
		if len(tail.lines) == 0 || len(tail.lines) >= exitLogTailLines {
			t.Errorf("got %d lines, want some but fewer than %d", len(tail.lines), exitLogTailLines)
		}
		for _, l := range tail.lines {
			if len(l) > exitLogLineLimit+len("…") {
				t.Errorf("a line of %d bytes passed the %d-byte line cap", len(l), exitLogLineLimit)
			}
		}
	})
}

// lineWith returns the single log line in out containing marker.
func lineWith(t *testing.T, out, marker string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, marker) {
			return l
		}
	}
	t.Fatalf("no log line contains %q:\n%s", marker, out)
	return ""
}
