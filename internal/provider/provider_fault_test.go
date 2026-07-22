package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
)

// TestGetInstanceErrorStatusPopulatesProviderFault is the M3-W2 provider_fault
// audit's positive proof: GetInstance is the one command whose ProviderFault
// field can actually reach GARM (taxonomy.go explains why CreateInstance's own
// failures cannot — execution.Run discards the ProviderInstance entirely on a
// non-nil Go error). A container in InstanceError status (OOM-killed, or the
// daemon's "dead" state) must carry a non-empty, descriptive ProviderFault.
func TestGetInstanceErrorStatusPopulatesProviderFault(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		oomKilled     bool
		wantSubstring string
	}{
		{"oom_killed", "exited", true, "out-of-memory"},
		{"dead", "dead", false, "dead"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newTestProvider(t)
			id := seedRunner(t, fake, "runner-fault", "pool-1", "controller-abc", tt.state)
			fake.SetState(id, tt.state, tt.oomKilled)

			inst, err := p.GetInstance(context.Background(), "runner-fault")
			if err != nil {
				t.Fatalf("GetInstance returned unexpected error: %v", err)
			}
			if inst.Status != params.InstanceError {
				t.Fatalf("Status = %q, want %q", inst.Status, params.InstanceError)
			}
			if len(inst.ProviderFault) == 0 {
				t.Fatal("ProviderFault is empty for an InstanceError-status instance, want a diagnostic message")
			}
			if !strings.Contains(string(inst.ProviderFault), tt.wantSubstring) {
				t.Errorf("ProviderFault = %q, want it to contain %q", inst.ProviderFault, tt.wantSubstring)
			}
		})
	}
}

// TestGetInstanceHealthyStatusLeavesProviderFaultEmpty guards the converse:
// a healthy (running/stopped/pending_create) instance must NOT carry a
// ProviderFault — the field means "the underlying resource is in trouble",
// not "here is some informational note."
func TestGetInstanceHealthyStatusLeavesProviderFaultEmpty(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{"running", "running"},
		{"exited", "exited"},
		{"created", "created"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newTestProvider(t)
			id := seedRunner(t, fake, "runner-healthy", "pool-1", "controller-abc", tt.state)
			fake.SetState(id, tt.state, false)

			inst, err := p.GetInstance(context.Background(), "runner-healthy")
			if err != nil {
				t.Fatalf("GetInstance returned unexpected error: %v", err)
			}
			if len(inst.ProviderFault) != 0 {
				t.Errorf("ProviderFault = %q for status %q, want empty", inst.ProviderFault, inst.Status)
			}
		})
	}
}

// TestListInstancesErrorStatusPopulatesProviderFaultBestEffort mirrors the
// GetInstance proof for ListInstances' summary-based mapping: it uses the
// summary's own human-readable Status string (already in hand, no extra
// inspect call) rather than the detailed OOM/dead-reason text GetInstance can
// produce — see toProviderInstanceFromSummary's doc comment.
func TestListInstancesErrorStatusPopulatesProviderFaultBestEffort(t *testing.T) {
	p, fake := newTestProvider(t)
	id := seedRunner(t, fake, "runner-list-fault", "pool-1", "controller-abc", "dead")
	fake.SetState(id, "dead", false)

	list, err := p.ListInstances(context.Background(), "")
	if err != nil {
		t.Fatalf("ListInstances returned unexpected error: %v", err)
	}
	var found bool
	for _, inst := range list {
		if inst.Name != "runner-list-fault" {
			continue
		}
		found = true
		if inst.Status != params.InstanceError {
			t.Errorf("Status = %q, want %q", inst.Status, params.InstanceError)
		}
		if len(inst.ProviderFault) == 0 {
			t.Errorf("ProviderFault is empty for a dead-state instance in ListInstances")
		}
	}
	if !found {
		t.Fatalf("runner-list-fault not present in ListInstances output: %+v", list)
	}
}
