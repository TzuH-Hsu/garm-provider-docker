package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// These tests cover the diag-prune throttle (ADR-003 amendment 2026-10-26): the
// GC runs on every CreateInstance and ListInstances, but a diag-prune round runs
// at most once per diagPruneInterval per controller, across provider processes,
// claimed through a never-started stamp container.

const throttleRunnerImage = "ghcr.io/example/runner@sha256:deadbeef"

// newThrottleProvider returns a cache provider whose runner image is present and
// that owns n full-identity diag volumes.
func newThrottleProvider(t *testing.T, n int) (*Provider, *docker.FakeClient) {
	t.Helper()
	p, fake := newCacheProvider(t, false)
	fake.PresentImages[throttleRunnerImage] = true
	for i := 0; i < n; i++ {
		repoKey := "repo-throttle-" + string(rune('a'+i))
		id := spec.CacheVolumeIdentity{ControllerID: p.controllerID, RepoKey: repoKey, RepoURLDigest: spec.RepoURLDigest("https://github.com/x/" + repoKey)}
		seedVol(t, fake, spec.DiagVolumeName(repoKey), id.DiagLabels(time.Now()))
	}
	return p, fake
}

// pruneHelperRuns counts the diag-prune helpers (role=cache-helper running a
// find) the fake has recorded since index from.
func pruneHelperRuns(fake *docker.FakeClient, from int) int {
	n := 0
	for _, c := range fake.Created[from:] {
		if c.Labels[spec.LabelRole] == spec.RoleCacheHelper && len(c.Entrypoint) == 3 && strings.Contains(c.Entrypoint[2], "find") {
			n++
		}
	}
	return n
}

// seedStamp creates a container at the stamp name with the given labels and
// returns its ID.
func seedStamp(t *testing.T, fake *docker.FakeClient, controllerID string, labels map[string]string) string {
	t.Helper()
	resp, err := fake.ContainerCreate(context.Background(), &container.Config{Image: throttleRunnerImage, Labels: labels}, &container.HostConfig{}, nil, nil, spec.DiagPruneStampName(controllerID))
	if err != nil {
		t.Fatalf("seed stamp: %v", err)
	}
	return resp.ID
}

// stampLabels returns the labels of this controller's stamp created at createdAt
// (an empty createdAt leaves the label out).
func stampLabels(controllerID, createdAt string) map[string]string {
	labels := map[string]string{
		spec.LabelManaged:      "true",
		spec.LabelControllerID: controllerID,
		spec.LabelRole:         spec.RoleDiagPruneStamp,
	}
	if createdAt != "" {
		labels[spec.LabelCreatedAt] = createdAt
	}
	return labels
}

// inspectStamp returns the current stamp container, or fails.
func inspectStamp(t *testing.T, fake *docker.FakeClient, controllerID string) (id string, labels map[string]string, state string) {
	t.Helper()
	c, err := fake.ContainerInspect(context.Background(), spec.DiagPruneStampName(controllerID))
	if err != nil {
		t.Fatalf("no diag-prune stamp: %v", err)
	}
	return c.ID, c.Config.Labels, c.State.Status
}

// TestDiagPruneFirstRoundPrunesAndStamps: with no stamp, the pass prunes every
// diag volume once and leaves a never-started stamp with this controller's
// labels, no instance-name, no mounts, and a fresh created-at.
func TestDiagPruneFirstRoundPrunesAndStamps(t *testing.T) {
	p, fake := newThrottleProvider(t, 3)

	p.pruneDiagVolumes(context.Background(), throttleRunnerImage)

	if got := pruneHelperRuns(fake, 0); got != 3 {
		t.Fatalf("first round ran %d prune helpers, want 3 (one per diag volume)", got)
	}
	_, labels, state := inspectStamp(t, fake, p.controllerID)
	if !p.isOwnDiagPruneStamp(labels) {
		t.Errorf("stamp labels %v are not this controller's stamp", labels)
	}
	if state != "created" {
		t.Errorf("stamp state = %q, want created (never started)", state)
	}
	age, known := helperAge(time.Now(), labels[spec.LabelCreatedAt])
	if !known || age < 0 || age > time.Minute {
		t.Errorf("stamp created-at %q is not a fresh time", labels[spec.LabelCreatedAt])
	}
	for _, c := range fake.Created {
		if c.Name == spec.DiagPruneStampName(p.controllerID) && len(c.Mounts) != 0 {
			t.Errorf("stamp mounts %v, want none (it must never pin a volume)", c.Mounts)
		}
	}
}

// TestDiagPruneThrottledWithinInterval: a second pass inside the interval, and
// a whole burst of them through runCacheGC (the ListInstances/CreateInstance
// hook), run no prune helper and leave the stamp untouched.
func TestDiagPruneThrottledWithinInterval(t *testing.T) {
	p, fake := newThrottleProvider(t, 6)
	ctx := context.Background()

	p.pruneDiagVolumes(ctx, throttleRunnerImage)
	stampID, _, _ := inspectStamp(t, fake, p.controllerID)

	before := len(fake.Created)
	for i := 0; i < 8; i++ {
		p.runCacheGC(ctx)
	}
	if got := pruneHelperRuns(fake, before); got != 0 {
		t.Errorf("a burst of GC passes inside the interval ran %d prune helpers, want 0", got)
	}
	if id, _, _ := inspectStamp(t, fake, p.controllerID); id != stampID {
		t.Errorf("a throttled pass replaced the stamp (%s -> %s)", stampID, id)
	}
}

// TestDiagPruneIntervalElapsedPrunesAgain: an expired stamp is replaced by a
// fresh one and the round runs again.
func TestDiagPruneIntervalElapsedPrunesAgain(t *testing.T) {
	p, fake := newThrottleProvider(t, 2)
	expired := time.Now().Add(-(diagPruneInterval + time.Minute)).UTC().Format(time.RFC3339)
	oldID := seedStamp(t, fake, p.controllerID, stampLabels(p.controllerID, expired))

	before := len(fake.Created)
	p.pruneDiagVolumes(context.Background(), throttleRunnerImage)

	if got := pruneHelperRuns(fake, before); got != 2 {
		t.Errorf("an expired stamp led to %d prune helpers, want 2", got)
	}
	id, labels, _ := inspectStamp(t, fake, p.controllerID)
	if id == oldID {
		t.Error("the expired stamp was not replaced")
	}
	if age, known := helperAge(time.Now(), labels[spec.LabelCreatedAt]); !known || age > time.Minute {
		t.Errorf("the new stamp's created-at %q is not fresh", labels[spec.LabelCreatedAt])
	}
}

// TestDiagPruneConcurrentProcessesRunOneRound: many provider processes (one
// Provider each, sharing one daemon) hit the GC at once with no stamp; exactly
// one claims the round and prunes, the others skip.
func TestDiagPruneConcurrentProcessesRunOneRound(t *testing.T) {
	p, fake := newThrottleProvider(t, 6)
	const processes = 8

	var wg sync.WaitGroup
	for i := 0; i < processes; i++ {
		peer, err := New(fake, p.cfg, p.controllerID)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer.pruneDiagVolumes(context.Background(), throttleRunnerImage)
		}()
	}
	wg.Wait()

	if got := pruneHelperRuns(fake, 0); got != 6 {
		t.Errorf("%d concurrent processes ran %d prune helpers, want 6 (one round over 6 volumes)", processes, got)
	}
}

// TestDiagPrunePeerClaimedFirstSkips: a peer process creates the stamp between
// this pass's inspect and its create (the already-running case). The create
// collides on the name, this pass skips without pruning, and the peer's stamp
// is left alone.
func TestDiagPrunePeerClaimedFirstSkips(t *testing.T) {
	p, fake := newThrottleProvider(t, 2)
	var peerID string
	fake.CreateHook = func() {
		fake.CreateHook = nil
		peerID = seedStamp(t, fake, p.controllerID, stampLabels(p.controllerID, time.Now().UTC().Format(time.RFC3339)))
	}

	p.pruneDiagVolumes(context.Background(), throttleRunnerImage)

	if got := pruneHelperRuns(fake, 0); got != 0 {
		t.Errorf("a pass that lost the claim ran %d prune helpers, want 0", got)
	}
	if id, _, _ := inspectStamp(t, fake, p.controllerID); id != peerID {
		t.Errorf("the peer's stamp %s was replaced by %s", peerID, id)
	}
}

// TestDiagPruneUnusableStampResetsWithoutPruning: a stamp whose created-at is
// missing, unparseable, or in the future is replaced WITHOUT a prune (prune
// later, never a storm), and the very next pass is throttled by the new stamp.
func TestDiagPruneUnusableStampResetsWithoutPruning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		createdAt string
	}{
		{"missing created-at", ""},
		{"unparseable created-at", "not-a-time"},
		{"created-at in the future", time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newThrottleProvider(t, 3)
			oldID := seedStamp(t, fake, p.controllerID, stampLabels(p.controllerID, tc.createdAt))
			logs := captureLog(t)

			p.pruneDiagVolumes(context.Background(), throttleRunnerImage)

			if got := pruneHelperRuns(fake, 0); got != 0 {
				t.Errorf("an unusable stamp led to %d prune helpers, want 0 (prune later)", got)
			}
			id, labels, _ := inspectStamp(t, fake, p.controllerID)
			if id == oldID {
				t.Error("the unusable stamp was not reset")
			}
			if age, known := helperAge(time.Now(), labels[spec.LabelCreatedAt]); !known || age < 0 || age > time.Minute {
				t.Errorf("the reset stamp's created-at %q is not a fresh time", labels[spec.LabelCreatedAt])
			}
			if !strings.Contains(logs.String(), "reset it") {
				t.Errorf("the reset was not logged; log was:\n%s", logs.String())
			}

			p.pruneDiagVolumes(context.Background(), throttleRunnerImage)
			if got := pruneHelperRuns(fake, 0); got != 0 {
				t.Errorf("the pass after a reset ran %d prune helpers, want 0 (throttled)", got)
			}
		})
	}
}

// TestDiagPruneForeignStampNameIsLeftAlone: a container at the stamp name that
// is not this controller's stamp is never removed, and no round runs.
func TestDiagPruneForeignStampNameIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels func(controllerID string) map[string]string
	}{
		{"another controller", func(string) map[string]string { return stampLabels("controller-other", "") }},
		{"wrong role", func(id string) map[string]string {
			l := stampLabels(id, "")
			l[spec.LabelRole] = spec.RoleCacheHelper
			return l
		}},
		{"a job allocation", func(id string) map[string]string {
			l := stampLabels(id, "")
			l[spec.LabelInstanceName] = "job-1"
			return l
		}},
		{"unlabeled", func(string) map[string]string { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newThrottleProvider(t, 2)
			squatter := seedStamp(t, fake, p.controllerID, tc.labels(p.controllerID))

			p.pruneDiagVolumes(context.Background(), throttleRunnerImage)

			if got := pruneHelperRuns(fake, 0); got != 0 {
				t.Errorf("ran %d prune helpers past a foreign stamp-named container, want 0", got)
			}
			if !helperContainerExists(t, fake, squatter) {
				t.Error("the foreign stamp-named container was removed")
			}
		})
	}
}

// TestDiagPruneDaemonErrorsSkipTheRound: a failed stamp inspect, or a failed
// removal of an expired stamp, skips the round (prune later) without failing.
func TestDiagPruneDaemonErrorsSkipTheRound(t *testing.T) {
	t.Run("inspect fails", func(t *testing.T) {
		p, fake := newThrottleProvider(t, 2)
		fake.InspectErr = errors.New("daemon unavailable")
		p.pruneDiagVolumes(context.Background(), throttleRunnerImage)
		fake.InspectErr = nil
		if got := pruneHelperRuns(fake, 0); got != 0 {
			t.Errorf("ran %d prune helpers after a failed stamp inspect, want 0", got)
		}
		if _, err := fake.ContainerInspect(context.Background(), spec.DiagPruneStampName(p.controllerID)); err == nil {
			t.Error("a stamp was created although the inspect failed")
		}
	})
	t.Run("removing the expired stamp fails", func(t *testing.T) {
		p, fake := newThrottleProvider(t, 2)
		expired := time.Now().Add(-(diagPruneInterval + time.Minute)).UTC().Format(time.RFC3339)
		oldID := seedStamp(t, fake, p.controllerID, stampLabels(p.controllerID, expired))
		fake.RemoveErr = errors.New("daemon unavailable")
		p.pruneDiagVolumes(context.Background(), throttleRunnerImage)
		fake.RemoveErr = nil
		if got := pruneHelperRuns(fake, 0); got != 0 {
			t.Errorf("ran %d prune helpers after a failed stamp removal, want 0", got)
		}
		if id, _, _ := inspectStamp(t, fake, p.controllerID); id != oldID {
			t.Errorf("stamp %s changed to %s although its removal failed", oldID, id)
		}
	})
}

// TestDiagPruneNoVolumesNoStamp: with no diag volume there is nothing to
// throttle, so no stamp is created.
func TestDiagPruneNoVolumesNoStamp(t *testing.T) {
	p, fake := newThrottleProvider(t, 0)
	p.pruneDiagVolumes(context.Background(), throttleRunnerImage)
	if _, err := fake.ContainerInspect(context.Background(), spec.DiagPruneStampName(p.controllerID)); err == nil {
		t.Error("a stamp was created although there are no diag volumes")
	}
}

// TestReapLeakedHelpersLeavesDiagPruneStamp: the stamp is never started, so it
// stays `created` for its whole interval; the leaked-helper reaper, which reaps
// a `created` cache helper past helperRunningMaxAge, must not touch it.
func TestReapLeakedHelpersLeavesDiagPruneStamp(t *testing.T) {
	p, fake := newThrottleProvider(t, 0)
	old := time.Now().Add(-(helperRunningMaxAge + time.Hour)).UTC().Format(time.RFC3339)
	stamp := seedStamp(t, fake, p.controllerID, stampLabels(p.controllerID, old))

	p.reapLeakedHelpers(context.Background())

	if !helperContainerExists(t, fake, stamp) {
		t.Error("the leaked-helper reaper removed the diag-prune stamp")
	}
}
