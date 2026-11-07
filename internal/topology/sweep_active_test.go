package topology

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/errdefs"
)

// TestSweepSkipsLiveRunnerInAnyState pins that a runner the daemon still holds as
// live is never swept, whatever its age. Neither clock fits it: FinishedAt is zero
// or dates from an earlier run, and the created-at deadline only bounds a create.
func TestSweepSkipsLiveRunnerInAnyState(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	tests := []struct {
		name        string
		state       string
		hasFinished bool
	}{
		{name: "paused, never restarted", state: "paused"},
		{name: "paused after an earlier run", state: "paused", hasFinished: true},
		{name: "restarting after an earlier run", state: "restarting", hasFinished: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, fake := newManager(t)
			seedNetworkFor(t, fake, testControllerID, "job-1", old, "n1")
			id := seedRunnerFor(t, fake, testControllerID, "job-1", old, "n1", tt.state)
			if tt.hasFinished {
				fake.SetFinishedAt(id, old)
			}

			if err := m.SweepOrphans(context.Background()); err != nil {
				t.Fatalf("SweepOrphans returned unexpected error: %v", err)
			}
			if countContainers(t, fake) != 1 || countNetworks(t, fake) != 1 {
				t.Errorf("sweep tore down a %s runner's allocation: %d containers, %d networks",
					tt.state, countContainers(t, fake), countNetworks(t, fake))
			}
		})
	}
}

// TestSweepKeepsAllocationWhenRunnerInspectFails pins that a failed inspect defers
// the decision: the sweep cannot read FinishedAt or the exit details, so it reports
// the error and a later sweep tears the allocation down.
func TestSweepKeepsAllocationWhenRunnerInspectFails(t *testing.T) {
	m, spy := newSpyManager(t)
	seedAllocation(t, spy.FakeClient, "job-1", time.Now().Add(-time.Hour), "n1", "exited")
	spy.InspectErr = errors.New("simulated inspect failure")

	err := m.SweepOrphans(context.Background())
	if err == nil || !strings.Contains(err.Error(), "simulated inspect failure") {
		t.Fatalf("SweepOrphans error = %v, want the inspect failure", err)
	}
	if countContainers(t, spy.FakeClient) != 1 || countNetworks(t, spy.FakeClient) != 1 {
		t.Fatal("sweep tore down an allocation whose runner it could not inspect")
	}

	spy.InspectErr = nil
	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("second SweepOrphans returned unexpected error: %v", err)
	}
	if countContainers(t, spy.FakeClient) != 0 || countNetworks(t, spy.FakeClient) != 0 {
		t.Error("the next sweep did not tear down the exited allocation")
	}
}

// TestSweepAgesAllocationWhenRunnerAlreadyGone pins a runner removed between the
// list and the inspect: there is nothing to defer, so the rest of the allocation is
// aged by its created-at.
func TestSweepAgesAllocationWhenRunnerAlreadyGone(t *testing.T) {
	m, spy := newSpyManager(t)
	seedAllocation(t, spy.FakeClient, "job-1", time.Now().Add(-time.Hour), "n1", "exited")
	spy.InspectErr = errdefs.NotFound(errors.New("no such container"))

	if err := m.SweepOrphans(context.Background()); err != nil {
		t.Fatalf("SweepOrphans returned unexpected error: %v", err)
	}
	if countNetworks(t, spy.FakeClient) != 0 {
		t.Error("sweep left the allocation of a runner that is already gone")
	}
}
