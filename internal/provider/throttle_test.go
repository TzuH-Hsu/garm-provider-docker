package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// These tests cover the ListInstances throttle (ADR-004 amendment 2026-11-03):
// the controller-wide orphan sweep and the cache GC run at most once per
// interval per controller, across provider processes, each claimed through its
// own never-started stamp container. CreateInstance's pre-create sweep is not
// throttled. The stamp mechanics themselves are covered by
// diag_prune_throttle_test.go.

// newListThrottleProvider returns a provider whose runner image is present, so
// the stamps can be created.
func newListThrottleProvider(t *testing.T) (*Provider, *docker.FakeClient) {
	t.Helper()
	p, fake := newTestProvider(t)
	fake.PresentImages[p.cfg.EffectiveRunnerImage("")] = true
	return p, fake
}

// seedStaleAllocation seeds a past-grace exited allocation the sweep removes.
func seedStaleAllocation(t *testing.T, fake *docker.FakeClient, name string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	seedNet(t, fake, name, old, "n-"+name)
	seedAttachedRunner(t, fake, name, old, "n-"+name, "exited")
	seedWorkspaceVol(t, fake, name, old, "n-"+name, nil)
}

func isSwept(t *testing.T, fake *docker.FakeClient, name string) bool {
	t.Helper()
	_, ok := netByName(t, fake, spec.JobNetworkName(name))
	return !ok
}

func seedNamedStamp(t *testing.T, fake *docker.FakeClient, name string, labels map[string]string) string {
	t.Helper()
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Image: throttleRunnerImage, Labels: labels}, &container.HostConfig{}, nil, nil, name)
	if err != nil {
		t.Fatalf("seed stamp %s: %v", name, err)
	}
	return resp.ID
}

func throttleStampLabels(controllerID string, createdAt time.Time) map[string]string {
	return map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: controllerID,
		spec.LabelRole:         spec.RoleThrottleStamp,
		spec.LabelCreatedAt:    createdAt.UTC().Format(time.RFC3339),
	}
}

func stampID(t *testing.T, fake *docker.FakeClient, name string) string {
	t.Helper()
	c, err := fake.ContainerInspect(context.Background(), name)
	if err != nil {
		t.Fatalf("no stamp %s: %v", name, err)
	}
	return c.ID
}

// TestListInstancesSweepsOnceThenThrottles: the first ListInstances sweeps and
// leaves a never-started stamp; a burst inside the interval sweeps nothing and
// leaves the stamp alone.
func TestListInstancesSweepsOnceThenThrottles(t *testing.T) {
	p, fake := newListThrottleProvider(t)
	ctx := context.Background()
	stamp := spec.ThrottleStampName(p.controllerID, spec.StampOrphanSweep)

	seedStaleAllocation(t, fake, "job-a")
	if _, err := p.ListInstances(ctx, ""); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if !isSwept(t, fake, "job-a") {
		t.Fatal("the first ListInstances did not sweep the stale allocation")
	}
	c, err := fake.ContainerInspect(ctx, stamp)
	if err != nil {
		t.Fatalf("no sweep stamp after the first pass: %v", err)
	}
	if c.State.Status != "created" {
		t.Errorf("stamp state = %q, want created (never started)", c.State.Status)
	}
	if !p.isOwnStamp(c.Config.Labels, spec.RoleThrottleStamp) {
		t.Errorf("stamp labels %v are not this controller's throttle stamp", c.Config.Labels)
	}
	id := c.ID

	seedStaleAllocation(t, fake, "job-b")
	for i := 0; i < 6; i++ {
		if _, err := p.ListInstances(ctx, ""); err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
	}
	if isSwept(t, fake, "job-b") {
		t.Error("a ListInstances inside the interval swept again")
	}
	if got := stampID(t, fake, stamp); got != id {
		t.Errorf("a throttled pass replaced the stamp (%s -> %s)", id, got)
	}
}

// TestListInstancesSweepsAgainWhenIntervalElapsed: an expired sweep stamp is
// replaced and the sweep runs.
func TestListInstancesSweepsAgainWhenIntervalElapsed(t *testing.T) {
	p, fake := newListThrottleProvider(t)
	stamp := spec.ThrottleStampName(p.controllerID, spec.StampOrphanSweep)
	oldID := seedNamedStamp(t, fake, stamp, throttleStampLabels(p.controllerID, time.Now().Add(-(orphanSweepInterval+time.Minute))))
	seedStaleAllocation(t, fake, "job-a")

	if _, err := p.ListInstances(context.Background(), ""); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if !isSwept(t, fake, "job-a") {
		t.Error("an expired stamp did not lead to a sweep")
	}
	if stampID(t, fake, stamp) == oldID {
		t.Error("the expired stamp was not replaced")
	}
}

// TestListInstancesConcurrentProcessesRunOneSweepAndGC: many provider processes
// (one Provider each, sharing one daemon) list at once. The sweep and the GC
// list volumes, so the number of volume listings of the whole burst equals that
// of one ListInstances on a fresh daemon: exactly one sweep and one GC pass.
func TestListInstancesConcurrentProcessesRunOneSweepAndGC(t *testing.T) {
	const processes = 8

	var listings atomic.Int64
	count := func(fake *docker.FakeClient) { fake.VolumeListHook = func() { listings.Add(1) } }

	// Baseline: one ListInstances on a fresh daemon.
	p, fake := newListThrottleProvider(t)
	count(fake)
	if _, err := p.ListInstances(context.Background(), ""); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	baseline := listings.Swap(0)
	if baseline == 0 {
		t.Fatal("a ListInstances listed no volumes; the sweep and GC are not observable")
	}

	p, fake = newListThrottleProvider(t)
	count(fake)
	var wg sync.WaitGroup
	for i := 0; i < processes; i++ {
		peer, err := New(fake, p.cfg, p.controllerID)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := peer.ListInstances(context.Background(), ""); err != nil {
				t.Errorf("ListInstances: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := listings.Load(); got != baseline {
		t.Errorf("%d concurrent ListInstances listed volumes %d times, want %d (one sweep and one GC pass)", processes, got, baseline)
	}
}

// TestCreateInstancePreCreateSweepIsNotThrottled: a fresh sweep stamp (a
// ListInstances just swept) does not stop CreateInstance from tearing down a
// stale allocation of the instance being created.
func TestCreateInstancePreCreateSweepIsNotThrottled(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newListThrottleProvider(t)
	ctx := context.Background()
	if _, err := p.ListInstances(ctx, ""); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	stamp := spec.ThrottleStampName(p.controllerID, spec.StampOrphanSweep)
	id := stampID(t, fake, stamp)

	name := "Test-Instance-01"
	seedNet(t, fake, name, time.Now().Add(-time.Hour), "stale-nonce")
	seedAttachedRunner(t, fake, name, time.Now().Add(-time.Hour), "stale-nonce", "exited")
	seedWorkspaceVol(t, fake, name, time.Now().Add(-time.Hour), "stale-nonce", map[string]string{"test.residue": "prior-job"})

	if _, err := p.CreateInstance(ctx, jitBootstrap(srv.URL)); err != nil {
		t.Fatalf("CreateInstance with a fresh sweep stamp: %v", err)
	}
	v, ok := volByResource(t, fake, name, spec.ResourceWorkspace)
	if !ok {
		t.Fatal("workspace volume missing after create")
	}
	if _, residual := v.Labels["test.residue"]; residual || v.Labels[spec.LabelCreateNonce] == "stale-nonce" {
		t.Error("the stale workspace volume survived the pre-create sweep")
	}
	if got := stampID(t, fake, stamp); got != id {
		t.Errorf("CreateInstance touched the sweep stamp (%s -> %s)", id, got)
	}
}

// TestRunCacheGCThrottled: the GC runs once per cacheGCInterval, whichever hook
// reaches it, and again once the stamp has expired.
func TestRunCacheGCThrottled(t *testing.T) {
	p, fake := newListThrottleProvider(t)
	ctx := context.Background()
	stamp := spec.ThrottleStampName(p.controllerID, spec.StampCacheGC)
	leakedAt := time.Now().Add(-(helperTerminalGrace + time.Hour))

	first := seedHelperContainer(t, fake, p.controllerID, leakedAt, "exited")
	p.runCacheGCThrottled(ctx)
	if helperContainerExists(t, fake, first) {
		t.Fatal("the first pass did not run the GC (leaked helper not reaped)")
	}
	id := stampID(t, fake, stamp)

	second := seedHelperContainer(t, fake, p.controllerID, leakedAt, "exited")
	p.runCacheGCThrottled(ctx)
	if !helperContainerExists(t, fake, second) {
		t.Error("a pass inside the interval ran the GC")
	}
	if got := stampID(t, fake, stamp); got != id {
		t.Errorf("a throttled pass replaced the stamp (%s -> %s)", id, got)
	}

	if err := fake.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatalf("remove stamp: %v", err)
	}
	seedNamedStamp(t, fake, stamp, throttleStampLabels(p.controllerID, time.Now().Add(-(cacheGCInterval+time.Minute))))
	p.runCacheGCThrottled(ctx)
	if helperContainerExists(t, fake, second) {
		t.Error("an expired stamp did not lead to a GC pass")
	}
}

// TestListInstancesSweepAndGCStampsAreIndependent: each pass has its own stamp,
// so an unexpired GC stamp does not hold back the sweep.
func TestListInstancesSweepAndGCStampsAreIndependent(t *testing.T) {
	p, fake := newListThrottleProvider(t)
	seedNamedStamp(t, fake, spec.ThrottleStampName(p.controllerID, spec.StampCacheGC), throttleStampLabels(p.controllerID, time.Now()))
	seedStaleAllocation(t, fake, "job-a")

	if _, err := p.ListInstances(context.Background(), ""); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if !isSwept(t, fake, "job-a") {
		t.Error("a fresh GC stamp throttled the sweep")
	}
}

// TestListInstancesWithoutRunnerImageIsUnthrottled: the stamp needs the runner
// image, so on a host without it every ListInstances still sweeps.
func TestListInstancesWithoutRunnerImageIsUnthrottled(t *testing.T) {
	p, fake := newTestProvider(t)
	for _, name := range []string{"job-a", "job-b"} {
		seedStaleAllocation(t, fake, name)
		if _, err := p.ListInstances(context.Background(), ""); err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		if !isSwept(t, fake, name) {
			t.Errorf("ListInstances without the runner image did not sweep %s", name)
		}
	}
	if _, err := fake.ContainerInspect(context.Background(), spec.ThrottleStampName(p.controllerID, spec.StampOrphanSweep)); err == nil {
		t.Error("a stamp was created although the runner image is absent")
	}
}

// TestListInstancesStampTroubleSkipsWithoutFailing: a daemon error on the stamp,
// or a foreign container squatting its name, skips the pass; the list GARM asked
// for still succeeds and the squatter stays.
func TestListInstancesStampTroubleSkipsWithoutFailing(t *testing.T) {
	t.Run("inspect fails", func(t *testing.T) {
		p, fake := newListThrottleProvider(t)
		seedStaleAllocation(t, fake, "job-a")
		fake.InspectErr = errors.New("daemon unavailable")
		_, err := p.ListInstances(context.Background(), "")
		fake.InspectErr = nil
		if err != nil {
			t.Fatalf("ListInstances failed on a stamp error: %v", err)
		}
		if isSwept(t, fake, "job-a") {
			t.Error("the sweep ran although its stamp could not be read")
		}
	})
	t.Run("foreign container at the stamp name", func(t *testing.T) {
		p, fake := newListThrottleProvider(t)
		labels := throttleStampLabels(p.controllerID, time.Now().Add(-time.Hour))
		labels[spec.LabelRole] = spec.RoleDiagPruneStamp // the wrong role for this pass
		squatter := seedNamedStamp(t, fake, spec.ThrottleStampName(p.controllerID, spec.StampOrphanSweep), labels)
		seedStaleAllocation(t, fake, "job-a")
		logs := captureLog(t)

		if _, err := p.ListInstances(context.Background(), ""); err != nil {
			t.Fatalf("ListInstances: %v", err)
		}
		if isSwept(t, fake, "job-a") {
			t.Error("the sweep ran past a foreign stamp-named container")
		}
		if !helperContainerExists(t, fake, squatter) {
			t.Error("the foreign stamp-named container was removed")
		}
		if !strings.Contains(logs.String(), "not this controller's stamp") {
			t.Errorf("the squatter was not logged; log was:\n%s", logs.String())
		}
	})
}

// TestThrottleStampsAreNotListedOrReaped: a stamp carries no instance-name and
// not the runner role, so ListInstances never reports it, the sweep and the
// leaked-helper reaper leave it, even long past any grace.
func TestThrottleStampsAreNotListedOrReaped(t *testing.T) {
	p, fake := newListThrottleProvider(t)
	old := time.Now().Add(-(helperRunningMaxAge + time.Hour))
	var ids []string
	for _, pass := range []spec.ThrottleStamp{spec.StampOrphanSweep, spec.StampCacheGC} {
		ids = append(ids, seedNamedStamp(t, fake, spec.ThrottleStampName(p.controllerID, pass), throttleStampLabels(p.controllerID, old)))
	}

	if err := p.topo.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	p.reapLeakedHelpers(context.Background())
	for _, id := range ids {
		if !helperContainerExists(t, fake, id) {
			t.Error("a throttle stamp was removed by the sweep or the helper reaper")
		}
	}

	list, err := p.ListInstances(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("ListInstances reported %d instances, want 0 (a stamp is not an instance)", len(list))
	}
}
