package spec

import "testing"

func TestRunnerContainerName(t *testing.T) {
	tests := []struct {
		name         string
		instanceName string
		want         string
	}{
		{name: "already lowercase", instanceName: "my-instance-abc123", want: "my-instance-abc123"},
		{name: "mixed case", instanceName: "My-Instance-ABC123", want: "my-instance-abc123"},
		{name: "all uppercase", instanceName: "MY-INSTANCE", want: "my-instance"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RunnerContainerName(tt.instanceName); got != tt.want {
				t.Errorf("RunnerContainerName(%q) = %q, want %q", tt.instanceName, got, tt.want)
			}
		})
	}
}

func TestDerivedNames(t *testing.T) {
	const instanceName = "My-Instance-ABC123"
	const wantBase = "my-instance-abc123"

	// Container/network names depend only on the instance name (the network is
	// ADR-004's stable claim marker).
	instanceOnly := []struct {
		name string
		fn   func(string) string
		want string
	}{
		{name: "DindContainerName", fn: DindContainerName, want: wantBase + "-dind"},
		{name: "JobNetworkName", fn: JobNetworkName, want: wantBase + "-net"},
	}
	for _, tt := range instanceOnly {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(instanceName); got != tt.want {
				t.Errorf("%s(%q) = %q, want %q", tt.name, instanceName, got, tt.want)
			}
		})
	}

	// Volume names are GENERATION-UNIQUE (F4): <instance>-<nonce>-<suffix>.
	const nonce = "0a1b2c3d"
	volumeNames := []struct {
		name string
		fn   func(string, string) string
		want string
	}{
		{name: "WorkspaceVolumeName", fn: WorkspaceVolumeName, want: wantBase + "-" + nonce + "-workspace"},
		{name: "SocketVolumeName", fn: SocketVolumeName, want: wantBase + "-" + nonce + "-socket"},
		{name: "DindStateVolumeName", fn: DindStateVolumeName, want: wantBase + "-" + nonce + "-dind-state"},
	}
	for _, tt := range volumeNames {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(instanceName, nonce); got != tt.want {
				t.Errorf("%s(%q, %q) = %q, want %q", tt.name, instanceName, nonce, got, tt.want)
			}
		})
	}
}

func TestDerivedNamesAreDistinctFromEachOther(t *testing.T) {
	const instanceName = "my-instance"
	const nonce = "deadbeef"

	names := map[string]string{
		"runner":      RunnerContainerName(instanceName),
		"dind":        DindContainerName(instanceName),
		"job-network": JobNetworkName(instanceName),
		"workspace":   WorkspaceVolumeName(instanceName, nonce),
		"socket":      SocketVolumeName(instanceName, nonce),
		"dind-state":  DindStateVolumeName(instanceName, nonce),
	}

	seen := make(map[string]string, len(names))
	for kind, n := range names {
		if other, ok := seen[n]; ok {
			t.Fatalf("name collision: %q used by both %q and %q", n, kind, other)
		}
		seen[n] = kind
	}
}

// TestVolumeNamesAreGenerationUnique is the F4 name-uniqueness guard: two
// generations of the SAME instance name produce DIFFERENT volume names (because
// each embeds its own create-nonce), which is what makes a name-based teardown
// generation-safe by construction — a stale teardown removing a volume by name
// can never collide with a different generation's freshly-created volume.
func TestVolumeNamesAreGenerationUnique(t *testing.T) {
	const instanceName = "job-1"
	builders := map[string]func(string, string) string{
		"workspace":  WorkspaceVolumeName,
		"socket":     SocketVolumeName,
		"dind-state": DindStateVolumeName,
	}
	for kind, fn := range builders {
		genA := fn(instanceName, "aaaa1111")
		genB := fn(instanceName, "bbbb2222")
		if genA == genB {
			t.Errorf("%s: two generations produced the same name %q — name-based teardown would be generation-unsafe", kind, genA)
		}
	}
	// The stable claim-marker network, by contrast, must be identical across
	// generations of the same instance (it is the dedup primitive).
	if JobNetworkName(instanceName) != JobNetworkName(instanceName) {
		t.Fatal("JobNetworkName is unexpectedly non-deterministic")
	}
}
