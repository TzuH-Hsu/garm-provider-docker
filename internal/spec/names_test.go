package spec

import (
	"strings"
	"testing"
)

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

// TestValidateDerivedName is the derived-name length/charset hardening guard:
// a name at or under Docker's 255-byte resource-name limit and matching its
// [a-zA-Z0-9][a-zA-Z0-9_.-]* grammar is accepted; anything else is rejected
// with a clear, specific error rather than being handed to the daemon to
// reject opaquely.
func TestValidateDerivedName(t *testing.T) {
	longButValid := strings.Repeat("a", dockerNameMaxLength) // exactly at the limit: ok
	tooLong := strings.Repeat("a", dockerNameMaxLength+1)    // one byte over: rejected

	tests := []struct {
		name    string
		kind    string
		value   string
		wantErr bool
		errSub  string
	}{
		{name: "normal name ok", kind: "workspace volume", value: "my-instance-0a1b2c3d-workspace", wantErr: false},
		{name: "exactly at the 255-byte limit", kind: "workspace volume", value: longButValid, wantErr: false},
		{name: "one byte over the limit", kind: "workspace volume", value: tooLong, wantErr: true, errSub: "exceeds Docker's 255-byte"},
		{name: "empty name", kind: "job network", value: "", wantErr: true, errSub: "is empty"},
		{name: "invalid leading character", kind: "runner container", value: "-leading-dash", wantErr: true, errSub: "not a valid Docker resource name"},
		{name: "invalid embedded character", kind: "runner container", value: "has/a/slash", wantErr: true, errSub: "not a valid Docker resource name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDerivedName(tt.kind, tt.value)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateDerivedName(%q, %q) = nil, want an error", tt.kind, tt.value)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateDerivedName(%q, %q) = %v, want nil", tt.kind, tt.value, err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.errSub)
			}
		})
	}
}

// TestValidateAllocationNamesPathologicallyLongInstanceName is the
// derived-name-length-hardening guard for a full allocation attempt: a
// normal instance name produces valid names for every derived resource, but
// a pathologically long one — long enough that the generation-nonce-
// qualified volume names (F4, ~44 bytes of fixed overhead: a dash, the
// 32-hex-char create-nonce, a dash, and the longest suffix "dind-state") push
// past Docker's 255-byte resource-name limit — fails EARLY with a clear,
// aggregated provider error rather than succeeding here and failing opaquely
// at the daemon later, deep inside VolumeCreate.
func TestValidateAllocationNamesPathologicallyLongInstanceName(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef" // 32 hex chars, matches newCreateNonce's length
	if len(nonce) != 32 {
		t.Fatalf("test fixture bug: nonce is %d chars, want 32", len(nonce))
	}

	t.Run("normal instance name is ok", func(t *testing.T) {
		if err := ValidateAllocationNames("my-normal-instance-01", nonce); err != nil {
			t.Fatalf("ValidateAllocationNames returned unexpected error: %v", err)
		}
	})

	t.Run("pathologically long instance name fails clearly", func(t *testing.T) {
		longName := strings.Repeat("a", 300)
		err := ValidateAllocationNames(longName, nonce)
		if err == nil {
			t.Fatal("ValidateAllocationNames(300-char instance name) = nil, want an error")
		}
		if !strings.Contains(err.Error(), "dind-state volume") {
			t.Errorf("error = %q, want it to name the offending dind-state volume", err.Error())
		}
		if !strings.Contains(err.Error(), "exceeds Docker's 255-byte") {
			t.Errorf("error = %q, want a clear over-the-limit message", err.Error())
		}
	})

	t.Run("boundary: exactly at the limit for every derived name", func(t *testing.T) {
		// dind-state has the largest fixed overhead (44 bytes: '-' + the
		// 32-char nonce + '-' + "dind-state"), so sizing the instance name so
		// THAT name lands exactly at 255 proves every other, shorter derived
		// name is within bounds too.
		instanceName := strings.Repeat("b", dockerNameMaxLength-44)
		if got := len(DindStateVolumeName(instanceName, nonce)); got != dockerNameMaxLength {
			t.Fatalf("test fixture bug: DindStateVolumeName length = %d, want exactly %d", got, dockerNameMaxLength)
		}
		if err := ValidateAllocationNames(instanceName, nonce); err != nil {
			t.Errorf("ValidateAllocationNames at the exact 255-byte boundary returned unexpected error: %v", err)
		}

		// One byte longer pushes dind-state (only) past the limit.
		if err := ValidateAllocationNames(instanceName+"b", nonce); err == nil {
			t.Fatal("ValidateAllocationNames one byte past the boundary = nil, want an error")
		}
	})
}
