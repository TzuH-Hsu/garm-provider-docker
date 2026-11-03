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
	ioTestStats = `{"blkio_stats":{"io_service_bytes_recursive":[` +
		`{"major":8,"minor":0,"op":"read","value":5},{"major":8,"minor":0,"op":"write","value":123456}]}}`
	ioTestProbe = "driver overlay2\nstore classic\nimage sha256:aa 1000\nimage sha256:bb 2500\n"
	ioLogMsg    = "teardown: DinD sidecar IO"
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

// TestTeardownReportsDindIO also pins the ordering the counters depend on:
// the fake, like the daemon, refuses stats and exec on a stopped container and
// returns an empty sample for a sidecar whose network namespace owner (the
// runner) is gone, so a complete line proves the sample was taken before any
// container of the allocation stopped.
func TestTeardownReportsDindIO(t *testing.T) {
	logs := captureLogs(t)
	m, fake, _ := seedDindAllocation(t, "gha-1")

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
		"since_create_s=95", "written_bytes=123456", "storage_driver=overlay2", "image_store=classic",
		"images=2", "image_bytes=3500",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("IO line missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "content_bytes") || strings.Contains(out, "probe_error") {
		t.Errorf("classic-store success line should carry neither content_bytes nor probe_error:\n%s", out)
	}
	if len(fake.OutputExecs) != 1 || fake.OutputExecs[0].Cmd[0] != "sh" {
		t.Errorf("OutputExecs = %+v, want one sh -c probe", fake.OutputExecs)
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

// TestTeardownSamplesDindIOBeforeRunnerRemoval is the regression for a sample
// taken after the runner was removed: the privileged sidecar shares the
// runner's network namespace, and the daemon then returns an empty sample. The
// report must come first, while the stop/remove order stays runner, then
// sidecar (ADR-004).
func TestTeardownSamplesDindIOBeforeRunnerRemoval(t *testing.T) {
	logs := captureLogs(t)
	m, fake, sidecar := seedDindAllocation(t, "gha-1")
	runner := spec.RunnerContainerName("gha-1")
	info, err := fake.ContainerInspect(context.Background(), runner)
	if err != nil {
		t.Fatalf("ContainerInspect runner: %v", err)
	}

	if _, err := m.TeardownAllocationReportingIO(context.Background(), "gha-1"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	out := logs.String()
	if strings.Contains(out, "IO unavailable") || !strings.Contains(out, "written_bytes=123456") {
		t.Errorf("the sample was not taken before the runner was removed:\n%s", out)
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
			name:    "stats call fails",
			tweak:   func(f *docker.FakeClient, _ string) { f.StatsErr = errors.New("daemon busy") },
			wantLog: []string{"level=WARN", "DinD sidecar IO unavailable", "stats_error", "daemon busy", "images=2"},
		},
		{
			name:    "stats sample has no blkio counters still runs the probe",
			tweak:   func(f *docker.FakeClient, s string) { f.SetStats(s, `{}`) },
			wantLog: []string{"level=WARN", "stats_error", "no blkio write counters", "storage_driver=overlay2", "images=2", "image_bytes=3500"},
		},
		{
			name: "stats and probe both fail",
			tweak: func(f *docker.FakeClient, _ string) {
				f.StatsErr = errors.New("daemon busy")
				f.ExecOutputErr = errors.New("exec refused")
			},
			wantLog:  []string{"level=WARN", "stats_error", "daemon busy", "probe_error", "exec refused"},
			wantNone: "written_bytes",
		},
		{
			name:    "stats body is not JSON",
			tweak:   func(f *docker.FakeClient, s string) { f.SetStats(s, `oops`) },
			wantLog: []string{"level=WARN", "stats_error", "failed to decode stats sample"},
		},
		{
			name:    "probe transport error keeps the written bytes",
			tweak:   func(f *docker.FakeClient, _ string) { f.ExecOutputErr = errors.New("exec refused") },
			wantLog: []string{"level=INFO", "written_bytes=123456", "probe_error", "exec refused"},
		},
		{
			name:    "probe exits non-zero",
			tweak:   func(f *docker.FakeClient, _ string) { f.ExecOutputExit = 3 },
			wantLog: []string{"level=INFO", "written_bytes=123456", "probe exited with code 3"},
		},
		{
			name:    "probe prints garbage",
			tweak:   func(f *docker.FakeClient, _ string) { f.ExecOutputText = "driver overlay2\nimage sha256:aa huge\n" },
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
