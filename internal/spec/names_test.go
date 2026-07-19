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

	tests := []struct {
		name string
		fn   func(string) string
		want string
	}{
		{name: "DindContainerName", fn: DindContainerName, want: wantBase + "-dind"},
		{name: "JobNetworkName", fn: JobNetworkName, want: wantBase + "-net"},
		{name: "WorkspaceVolumeName", fn: WorkspaceVolumeName, want: wantBase + "-workspace"},
		{name: "SocketVolumeName", fn: SocketVolumeName, want: wantBase + "-socket"},
		{name: "DindStateVolumeName", fn: DindStateVolumeName, want: wantBase + "-dind-state"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(instanceName); got != tt.want {
				t.Errorf("%s(%q) = %q, want %q", tt.name, instanceName, got, tt.want)
			}
		})
	}
}

func TestDerivedNamesAreDistinctFromEachOther(t *testing.T) {
	const instanceName = "my-instance"

	names := map[string]string{
		"runner":      RunnerContainerName(instanceName),
		"dind":        DindContainerName(instanceName),
		"job-network": JobNetworkName(instanceName),
		"workspace":   WorkspaceVolumeName(instanceName),
		"socket":      SocketVolumeName(instanceName),
		"dind-state":  DindStateVolumeName(instanceName),
	}

	seen := make(map[string]string, len(names))
	for kind, n := range names {
		if other, ok := seen[n]; ok {
			t.Fatalf("name collision: %q used by both %q and %q", n, kind, other)
		}
		seen[n] = kind
	}
}
