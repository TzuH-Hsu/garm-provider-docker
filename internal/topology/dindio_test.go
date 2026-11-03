package topology

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const (
	ioTestNonce = "n1"
	// ioTestStats is the stats endpoint's sample, the fallback source. Its
	// total differs from ioTestIOStat's so a line shows which one it used.
	ioTestStats = `{"blkio_stats":{"io_service_bytes_recursive":[` +
		`{"major":8,"minor":0,"op":"read","value":5},{"major":8,"minor":0,"op":"write","value":654321}]}}`
	// ioTestIOStat is what the probe exec prints for a readable io.stat: a
	// disk and a zram swap device, 123456 bytes written in all.
	ioTestIOStat = "io_stat\n" +
		"io 8:0 rbytes=5 wbytes=123000 rios=1 wios=2 dbytes=0 dios=0\n" +
		"io 251:0 rbytes=0 wbytes=456 rios=0 wios=1 dbytes=0 dios=0\n"
	ioTestImages = "driver overlay2\nstore classic\nimage sha256:aa 1000\nimage sha256:bb 2500\n"
	ioTestProbe  = ioTestIOStat + ioTestImages
	// ioTestNoIOStat is the probe output on a host where io.stat is missing.
	ioTestNoIOStat = "io_stat_missing\n" + ioTestImages
	ioLogMsg       = "teardown: DinD sidecar IO"
)

var ioTestCreatedAt = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// captureLogs swaps slog's default for a debug-level text handler writing to
// the returned buffer, restored on cleanup. It mutates a process global, so
// tests using it must not run in parallel.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// seedRunnerWithEnv seeds a running runner whose environment names its
// repository, attached to name's job network, and returns its ID.
func seedRunnerWithEnv(t *testing.T, fake *docker.FakeClient, name string, env []string) string {
	t.Helper()
	labels := identityFor(name).ContainerLabels(spec.RoleRunner, ioTestCreatedAt)
	labels[spec.LabelCreateNonce] = ioTestNonce
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels, Env: env},
		&container.HostConfig{NetworkMode: container.NetworkMode(spec.JobNetworkName(name))}, nil, nil, spec.RunnerContainerName(name))
	if err != nil {
		t.Fatalf("seed runner ContainerCreate: %v", err)
	}
	fake.SetState(resp.ID, "running", false)
	return resp.ID
}

// seedSidecar seeds a DinD sidecar with the given HostConfig (its runtime pair
// and network placement) and state, returning its ID.
func seedSidecar(t *testing.T, fake *docker.FakeClient, name string, host *container.HostConfig, state string) string {
	t.Helper()
	labels := identityFor(name).DindContainerLabels(ioTestCreatedAt)
	labels[spec.LabelCreateNonce] = ioTestNonce
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Labels: labels},
		host, nil, nil, spec.DindContainerName(name))
	if err != nil {
		t.Fatalf("seed sidecar ContainerCreate: %v", err)
	}
	fake.SetState(resp.ID, state, false)
	return resp.ID
}

// seedDindAllocation seeds a full privileged-sidecar allocation (claim network,
// dind-state volume, runner, running sidecar in the runner's network namespace
// with stats and probe output) and returns the Manager with a fixed clock 95s
// after creation.
func seedDindAllocation(t *testing.T, name string) (*Manager, *docker.FakeClient, string) {
	t.Helper()
	m, fake := newManager(t)
	m.now = func() time.Time { return ioTestCreatedAt.Add(95 * time.Second) }
	seedNetworkFor(t, fake, testControllerID, name, ioTestCreatedAt, ioTestNonce)
	seedDindStateVolumeFor(t, fake, testControllerID, name, ioTestCreatedAt, ioTestNonce)
	runner := seedRunnerWithEnv(t, fake, name, []string{"PATH=/bin", "RUNNER_ORG=acme", "RUNNER_REPO=widgets"})
	sidecar := seedSidecar(t, fake, name,
		&container.HostConfig{Privileged: true, NetworkMode: spec.ContainerNetworkMode(runner)}, "running")
	fake.SetStats(sidecar, ioTestStats)
	fake.ExecOutputText = ioTestProbe
	return m, fake, sidecar
}

func assertAllocationGone(t *testing.T, fake *docker.FakeClient) {
	t.Helper()
	if n := countContainers(t, fake); n != 0 {
		t.Errorf("containers left = %d, want 0", n)
	}
	if n := countVolumes(t, fake); n != 0 {
		t.Errorf("volumes left = %d, want 0", n)
	}
	if n := countNetworks(t, fake); n != 0 {
		t.Errorf("networks left = %d, want 0", n)
	}
}

// TestTeardownReportsDindIO pins the primary path: the written bytes come from
// the sidecar's io.stat, read in its own exec before the image probe, and the
// stats endpoint is not consulted.
func TestTeardownReportsDindIO(t *testing.T) {
	logs := captureLogs(t)
	m, fake, _ := seedDindAllocation(t, "gha-1")
	fake.StatsErr = errors.New("stats must not be called")

	found, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1")
	if err != nil || !found {
		t.Fatalf("TeardownAllocationReportingIO = (%v, %v), want (true, nil)", found, err)
	}
	assertAllocationGone(t, fake)

	out := logs.String()
	if n := strings.Count(out, ioLogMsg); n != 1 {
		t.Fatalf("got %d IO lines, want exactly 1:\n%s", n, out)
	}
	for _, want := range []string{
		"level=INFO", "instance=gha-1", "pool=pool-1", "repo=acme/widgets", "dind_mode=privileged-sidecar",
		"since_create_s=95", "written_bytes=123456", "written_source=io.stat",
		`written_by_device="8:0=123000,251:0=456"`, "storage_driver=overlay2", "image_store=classic",
		"images=2", "image_bytes=3500",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("IO line missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"content_bytes", "probe_error", "stats_error", "io_stat_error"} {
		if strings.Contains(out, bad) {
			t.Errorf("classic-store io.stat line must not carry %s:\n%s", bad, out)
		}
	}
	if len(fake.OutputExecs) != 2 || fake.OutputExecs[0].Cmd[2] != ioStatScript || fake.OutputExecs[1].Cmd[2] != imageProbeScript {
		t.Errorf("OutputExecs = %+v, want the io.stat exec, then the image probe", fake.OutputExecs)
	}
}

// stallingProbe answers the io.stat exec with ioOut and makes the image probe
// hang until its context ends, as a probe stuck on a stalled disk would.
func stallingProbe(ioOut string) func(context.Context, []string) (string, int, error) {
	return func(ctx context.Context, cmd []string) (string, int, error) {
		if cmd[len(cmd)-1] == ioStatScript {
			return ioOut, 0, nil
		}
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
}

// reportOne runs one sidecar report of gha-1's allocation with the given
// budgets, as removeContainers would before stopping anything.
func reportOne(t *testing.T, m *Manager, fake *docker.FakeClient, sidecar string, budget, step time.Duration) {
	t.Helper()
	list, err := fake.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	r := m.newDindIOReporter(context.Background(), list, &teardownScope{m: m, mode: teardownModeAll, reportDindIO: true})
	if r == nil {
		t.Fatal("expected a reporter for an allocation with a sidecar")
	}
	r.budget, r.stepBudget = budget, step
	for _, c := range list {
		if c.ID == sidecar {
			r.report(context.Background(), c)
		}
	}
}

// TestStalledProbeKeepsIOStatBytes is the regression for an image probe that
// hangs past the report's budget (the sidecar's daemon stuck on a slow disk):
// io.stat is read in its own exec first, so the line keeps written_bytes and
// reports the probe's failure next to it.
func TestStalledProbeKeepsIOStatBytes(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	fake.ExecOutputFunc = stallingProbe(ioTestIOStat)
	fake.StatsErr = errors.New("stats must not be called")

	start := time.Now()
	reportOne(t, m, fake, sidecar, 300*time.Millisecond, 100*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("report took %v, want it bounded by its budget", d)
	}
	out := logs.String()
	for _, want := range []string{"level=INFO", "written_bytes=123456", "written_source=io.stat", "probe_error", "deadline exceeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("IO line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stats_error") || strings.Contains(out, "images=") {
		t.Errorf("line must carry neither a stats error nor image fields:\n%s", out)
	}
}

// TestStalledIOStatExecFallsBackOnFreshBudget proves an io.stat exec that hangs
// costs only its own step budget: the stats fallback then runs with time left
// (the fake's stats call, like the real client, fails on a context that is
// already done), and the image probe still runs after it.
func TestStalledIOStatExecFallsBackOnFreshBudget(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	fake.ExecOutputFunc = func(ctx context.Context, cmd []string) (string, int, error) {
		if cmd[len(cmd)-1] == ioStatScript {
			<-ctx.Done()
			return "", 0, ctx.Err()
		}
		return ioTestImages, 0, nil
	}

	reportOne(t, m, fake, sidecar, 2*time.Second, 100*time.Millisecond)
	out := logs.String()
	for _, want := range []string{"level=INFO", "written_bytes=654321", "written_source=stats",
		"io_stat_error", "failed to read io.stat", "images=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("IO line missing %q:\n%s", want, out)
		}
	}
}

// TestStatsFallbackSkippedWhenBudgetSpent proves the stats fallback does not
// start once the report's budget is gone.
func TestStatsFallbackSkippedWhenBudgetSpent(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	fake.ExecOutputFunc = func(ctx context.Context, _ []string) (string, int, error) {
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
	fake.StatsErr = errors.New("stats must not be called")

	// A step budget longer than the report's: the io.stat exec spends it all.
	reportOne(t, m, fake, sidecar, 100*time.Millisecond, time.Second)
	out := logs.String()
	for _, want := range []string{"level=WARN", "report budget spent before the stats sample", "probe_error"} {
		if !strings.Contains(out, want) {
			t.Errorf("IO line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "must not be called") {
		t.Errorf("the stats fallback ran on a spent budget:\n%s", out)
	}
}

// TestTeardownReportsDindIOAfterRunnerExited is the regression for the live
// failure: the ephemeral runner exits on its own when its job ends, before
// the delete arrives, so the stats request for the privileged sidecar (which
// shares the runner's network namespace) fails with "id is empty". io.stat
// does not depend on the runner, so the line still carries written_bytes.
func TestTeardownReportsDindIOAfterRunnerExited(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	runner, err := fake.ContainerInspect(context.Background(), spec.RunnerContainerName("gha-1"))
	if err != nil {
		t.Fatalf("ContainerInspect runner: %v", err)
	}
	fake.SetState(runner.ID, "exited", false)

	// Guard: the fake really models the daemon's failure for this placement.
	if _, err := fake.ContainerStatsOneShot(context.Background(), sidecar); err == nil || !strings.Contains(err.Error(), "id is empty") {
		t.Fatalf("stats of the sidecar with its netns owner stopped: err = %v, want id is empty", err)
	}

	if _, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	out := logs.String()
	for _, want := range []string{"level=INFO", "written_bytes=123456", "written_source=io.stat", "images=2", "repo=acme/widgets"} {
		if !strings.Contains(out, want) {
			t.Errorf("IO line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "IO unavailable") {
		t.Errorf("line must not take the WARN path:\n%s", out)
	}
}

// TestTeardownReportsZeroIOStat proves an io.stat that is present but empty
// (no device saw IO) is a valid zero, not an absence.
func TestTeardownReportsZeroIOStat(t *testing.T) {
	logs := captureLogs(t)
	m, fake, _ := seedDindAllocation(t, "gha-1")
	fake.ExecOutputText = "io_stat\n" + ioTestImages
	fake.StatsErr = errors.New("stats must not be called")

	if _, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "written_bytes=0 written_source=io.stat") {
		t.Errorf("want an INFO line with written_bytes=0 from io.stat:\n%s", out)
	}
	if strings.Contains(out, "written_by_device") || strings.Contains(out, "stats_error") {
		t.Errorf("an empty io.stat has no devices and needs no fallback:\n%s", out)
	}
}

// TestReportBudgetStartsWhenReportStarts proves time spent between creating the
// reporter and reporting (in a teardown: the runner lookup, or an earlier
// sidecar's report) does not count against the sidecar measurement's budget.
func TestReportBudgetStartsWhenReportStarts(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	list, err := fake.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		t.Fatalf("ContainerList: %v", err)
	}
	scope := &teardownScope{m: m, mode: teardownModeAll, reportDindIO: true}
	r := m.newDindIOReporter(context.Background(), list, scope)
	if r == nil {
		t.Fatal("expected a reporter for an allocation with a sidecar")
	}

	// A budget far shorter than the simulated delay: were it counted from the
	// reporter's creation, it would be spent before the report began.
	r.budget = 200 * time.Millisecond
	time.Sleep(400 * time.Millisecond)

	for _, c := range list {
		if c.ID == sidecar {
			r.report(context.Background(), c)
		}
	}
	if !strings.Contains(logs.String(), "written_bytes=123456") || !strings.Contains(logs.String(), "repo=acme/widgets") {
		t.Errorf("the IO line was not emitted after a delay:\n%s", logs.String())
	}
}

// TestTeardownSamplesDindIOBeforeRunnerRemoval pins the ordering the stats
// fallback depends on: the privileged sidecar shares the runner's network
// namespace, and once the teardown stops or removes the runner the daemon
// cannot sample the sidecar. The report must come first, while the stop/remove
// order stays runner, then sidecar (ADR-004).
func TestTeardownSamplesDindIOBeforeRunnerRemoval(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	fake.ExecOutputText = ioTestNoIOStat
	runner := spec.RunnerContainerName("gha-1")
	info, err := fake.ContainerInspect(context.Background(), runner)
	if err != nil {
		t.Fatalf("ContainerInspect runner: %v", err)
	}

	if _, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	out := logs.String()
	if strings.Contains(out, "IO unavailable") || !strings.Contains(out, "written_bytes=654321 written_source=stats") {
		t.Errorf("the stats sample was not taken before the runner was removed:\n%s", out)
	}
	var containers []string
	for _, r := range fake.RemoveOrder {
		if strings.HasPrefix(r, "container:") {
			containers = append(containers, r)
		}
	}
	want := []string{"container:" + info.ID, "container:" + sidecar}
	if strings.Join(containers, ",") != strings.Join(want, ",") {
		t.Errorf("container remove order = %v, want %v (runner before sidecar)", containers, want)
	}
}

func TestTeardownReportsContainerdContentBytes(t *testing.T) {
	logs := captureLogs(t)
	m, fake, _ := seedDindAllocation(t, "gha-1")
	fake.ExecOutputText = "driver overlayfs\nstore containerd\ncontent_kib 100\nimage sha256:aa 1000\n"

	if _, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, want := range []string{"storage_driver=overlayfs", "image_store=containerd", "content_bytes=102400"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("IO line missing %q:\n%s", want, logs.String())
		}
	}
}

func TestTeardownReportsSysboxMode(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	seedNetworkFor(t, fake, testControllerID, "gha-2", ioTestCreatedAt, ioTestNonce)
	sidecar := seedSidecar(t, fake, "gha-2", &container.HostConfig{Runtime: "sysbox-runc"}, "running")
	fake.SetStats(sidecar, ioTestStats)
	fake.ExecOutputText = ioTestProbe

	if _, err := m.TeardownAllocationReportingIO(context.Background(), "gha-2"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !strings.Contains(logs.String(), "dind_mode=sysbox-runc") {
		t.Errorf("IO line missing dind_mode=sysbox-runc:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "repo=") {
		t.Errorf("repo must be omitted when there is no runner to read it from:\n%s", logs.String())
	}
}

func TestTeardownWithoutReportingStaysSilent(t *testing.T) {
	logs := captureLogs(t)
	m, fake, _ := seedDindAllocation(t, "gha-1")

	if _, err := m.TeardownAllocation(context.Background(), "gha-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	assertAllocationGone(t, fake)
	if strings.Contains(logs.String(), ioLogMsg) || len(fake.OutputExecs) != 0 {
		t.Errorf("plain TeardownAllocation must not report: logs=%q execs=%d", logs.String(), len(fake.OutputExecs))
	}
}

func TestTeardownReportingSkipsNonDindAllocations(t *testing.T) {
	logs := captureLogs(t)
	m, fake := newManager(t)
	seedNetworkFor(t, fake, testControllerID, "gha-3", ioTestCreatedAt, ioTestNonce)
	seedRunnerWithEnv(t, fake, "gha-3", []string{"RUNNER_ORG=acme"})
	fake.ExecOutputErr = errors.New("must not be called")
	fake.StatsErr = errors.New("must not be called")

	found, err := m.TeardownAllocationReportingIO(context.Background(), "gha-3")
	if err != nil || !found {
		t.Fatalf("TeardownAllocationReportingIO = (%v, %v), want (true, nil)", found, err)
	}
	assertAllocationGone(t, fake)
	if strings.Contains(logs.String(), "DinD") {
		t.Errorf("a non-DinD teardown logged DinD output:\n%s", logs.String())
	}
}

// TestTeardownSurvivesReportFailures proves every failure mode of the report
// leaves the teardown complete and error-free, and is logged with its reason.
func TestTeardownSurvivesReportFailures(t *testing.T) {
	tests := []struct {
		name     string
		tweak    func(f *docker.FakeClient, sidecar string)
		wantLog  []string
		wantNone string
	}{
		{
			name:  "io.stat missing falls back to the stats sample",
			tweak: func(f *docker.FakeClient, _ string) { f.ExecOutputText = ioTestNoIOStat },
			wantLog: []string{"level=INFO", "written_bytes=654321", "written_source=stats",
				"io_stat_error", "not readable", "images=2"},
		},
		{
			name: "io.stat missing and stats call fails",
			tweak: func(f *docker.FakeClient, _ string) {
				f.ExecOutputText = ioTestNoIOStat
				f.StatsErr = errors.New("daemon busy")
			},
			wantLog:  []string{"level=WARN", "DinD sidecar IO unavailable", "io_stat_error", "stats_error", "daemon busy", "images=2"},
			wantNone: "written_bytes",
		},
		{
			name: "io.stat missing and stats sample has no blkio counters still logs the probe",
			tweak: func(f *docker.FakeClient, s string) {
				f.ExecOutputText = ioTestNoIOStat
				f.SetStats(s, `{}`)
			},
			wantLog: []string{"level=WARN", "stats_error", "no blkio write counters", "storage_driver=overlay2", "images=2", "image_bytes=3500"},
		},
		{
			name: "io.stat missing and stats body is not JSON",
			tweak: func(f *docker.FakeClient, s string) {
				f.ExecOutputText = ioTestNoIOStat
				f.SetStats(s, `oops`)
			},
			wantLog: []string{"level=WARN", "stats_error", "failed to decode stats sample"},
		},
		{
			name: "malformed io.stat falls back to the stats sample",
			tweak: func(f *docker.FakeClient, _ string) {
				f.ExecOutputText = "io_stat\nio 8:0 wbytes=lots\n" + ioTestImages
			},
			wantLog: []string{"level=INFO", "written_bytes=654321", "written_source=stats", "malformed io.stat line"},
		},
		{
			name:    "probe transport error falls back to the stats sample",
			tweak:   func(f *docker.FakeClient, _ string) { f.ExecOutputErr = errors.New("exec refused") },
			wantLog: []string{"level=INFO", "written_bytes=654321", "written_source=stats", "io_stat_error", "probe_error", "exec refused"},
		},
		{
			name: "exec and stats both fail",
			tweak: func(f *docker.FakeClient, _ string) {
				f.StatsErr = errors.New("daemon busy")
				f.ExecOutputErr = errors.New("exec refused")
			},
			wantLog:  []string{"level=WARN", "stats_error", "daemon busy", "probe_error", "exec refused"},
			wantNone: "written_bytes",
		},
		{
			name:    "image probe exits non-zero keeps the io.stat bytes",
			tweak:   func(f *docker.FakeClient, _ string) { f.ExecOutputExit = 3 },
			wantLog: []string{"level=INFO", "written_bytes=123456", "written_source=io.stat", "probe exited with code 3"},
		},
		{
			name: "image probe prints garbage",
			tweak: func(f *docker.FakeClient, _ string) {
				f.ExecOutputText = ioTestIOStat + "driver overlay2\nimage sha256:aa huge\n"
			},
			wantLog: []string{"level=INFO", "written_bytes=123456", "malformed probe line"},
		},
		{
			name:     "sidecar already stopped",
			tweak:    func(f *docker.FakeClient, s string) { f.SetState(s, "exited", false) },
			wantLog:  []string{"level=DEBUG", "DinD sidecar IO not reported", "sidecar is not running"},
			wantNone: "level=INFO",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			m, fake, sidecar := seedDindAllocation(t, "gha-1")
			tc.tweak(fake, sidecar)

			found, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1")
			if err != nil || !found {
				t.Fatalf("TeardownAllocationReportingIO = (%v, %v), want (true, nil)", found, err)
			}
			assertAllocationGone(t, fake)
			for _, want := range tc.wantLog {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log missing %q:\n%s", want, logs.String())
				}
			}
			if tc.wantNone != "" && strings.Contains(logs.String(), tc.wantNone) {
				t.Errorf("log must not contain %q:\n%s", tc.wantNone, logs.String())
			}
		})
	}
}
