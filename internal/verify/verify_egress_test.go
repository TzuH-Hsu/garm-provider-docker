//go:build dockerverify

package verify

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// TestVerifyOwnerRulingEgressAndIsolation is the live-daemon regression test
// for the 2026-07-21 owner ruling (ADR-001's Amendment): job networks default
// to internal=false so the runner/DinD retain egress, while per-allocation
// network separation — not the internal flag — is what provides job-to-job
// isolation. It drives the REAL provider binary through two allocations under
// one controller-id:
//
//   - "owner-verify-default": [network] omitted, so config.Load's internal=
//     false default applies. Asserts the job network reports Internal=false
//     AND the runner can actually reach the public internet.
//   - "owner-verify-internal-true": [network] internal=true, explicitly
//     exercising the RESERVED advanced/opt-in path this field is kept for.
//     Asserts the job network reports Internal=true AND the runner's
//     outbound attempt is blocked — proving the flag still works exactly as
//     documented for operators who choose the stricter, airgapped posture.
//
// It then asserts the two allocations sit on two DIFFERENT networks, and that
// the default-config runner cannot reach the internal=true runner by
// container IP: per-job isolation holds independently of either allocation's
// own `internal` setting, because isolation comes from each allocation owning
// its own separate network (ADR-001's Decision), not from this flag.
func TestVerifyOwnerRulingEgressAndIsolation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	imageTag := "garm-wp2-verify-sleep:latest"
	buildSleepImage(t, imageTag)

	bin := filepath.Join(t.TempDir(), "garm-provider-docker")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build provider: %v\n%s", err, out)
	}

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	const nameDefault = "owner-verify-default"
	const nameInternal = "owner-verify-internal-true"

	configDir := t.TempDir()
	defaultConfigFile := filepath.Join(configDir, "default.toml")
	writeFile(t, defaultConfigFile, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true
`, imageTag))

	internalConfigFile := filepath.Join(configDir, "internal-true.toml")
	writeFile(t, internalConfigFile, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true

[network]
internal = true
`, imageTag))

	// Label-scoped teardown of everything for THIS controller-id, regardless
	// of whether the explicit DeleteInstance calls below succeed.
	defer cleanupController(t, controllerID)

	// =========================================================================
	// Allocation A: default config -> internal=false, egress must work.
	// =========================================================================
	bDefault := bootstrapFor(nameDefault, srv.URL, caBundle)
	stdoutA, codeA := runProvider(t, bin, defaultConfigFile, controllerID, "CreateInstance", "", &bDefault)
	if codeA != 0 {
		t.Fatalf("CreateInstance(%s) exit=%d, want 0; stdout=%s", nameDefault, codeA, stdoutA)
	}
	var createdA params.ProviderInstance
	if err := json.Unmarshal([]byte(stdoutA), &createdA); err != nil {
		t.Fatalf("parse CreateInstance(%s) stdout %q: %v", nameDefault, stdoutA, err)
	}
	defer runProvider(t, bin, defaultConfigFile, controllerID, "DeleteInstance", nameDefault, nil)

	netA := spec.JobNetworkName(nameDefault)
	internalA := dockerOut(t, "network", "inspect", netA, "-f", "{{.Internal}}")
	t.Logf("[default] network %s Internal=%s", netA, internalA)
	if internalA != "false" {
		t.Errorf("[default] job network Internal=%s, want false (2026-07-21 owner ruling default)", internalA)
	}
	egressOutA, egressErrA := dockerTry("exec", createdA.ProviderID, "sh", "-c", "wget -qO- -T 6 https://api.github.com/zen")
	if egressErrA != nil {
		t.Errorf("[default] runner could NOT reach api.github.com on an internal=false network: %v\n%s", egressErrA, egressOutA)
	} else {
		t.Logf("[default] egress succeeded, api.github.com/zen replied: %q", strings.TrimSpace(egressOutA))
	}

	// =========================================================================
	// Allocation B: internal=true config -> reserved advanced posture, egress
	// must fail, proving the retained knob still works for operators who
	// choose it.
	// =========================================================================
	bInternal := bootstrapFor(nameInternal, srv.URL, caBundle)
	stdoutB, codeB := runProvider(t, bin, internalConfigFile, controllerID, "CreateInstance", "", &bInternal)
	if codeB != 0 {
		t.Fatalf("CreateInstance(%s) exit=%d, want 0; stdout=%s", nameInternal, codeB, stdoutB)
	}
	var createdB params.ProviderInstance
	if err := json.Unmarshal([]byte(stdoutB), &createdB); err != nil {
		t.Fatalf("parse CreateInstance(%s) stdout %q: %v", nameInternal, stdoutB, err)
	}
	defer runProvider(t, bin, internalConfigFile, controllerID, "DeleteInstance", nameInternal, nil)

	netB := spec.JobNetworkName(nameInternal)
	internalB := dockerOut(t, "network", "inspect", netB, "-f", "{{.Internal}}")
	t.Logf("[internal=true] network %s Internal=%s", netB, internalB)
	if internalB != "true" {
		t.Errorf("[internal=true] job network Internal=%s, want true (explicit opt-in still honored)", internalB)
	}
	_, egressErrB := dockerTry("exec", createdB.ProviderID, "sh", "-c", "wget -qO- -T 6 https://api.github.com/zen")
	if egressErrB == nil {
		t.Errorf("[internal=true] runner reached api.github.com on an internal=true network, want blocked")
	} else {
		t.Logf("[internal=true] egress correctly blocked (wget failed as expected): %v", egressErrB)
	}

	// =========================================================================
	// Isolation: two distinct networks; no cross-allocation reachability by
	// IP, independent of either allocation's own `internal` setting.
	// =========================================================================
	if netA == netB {
		t.Fatalf("both allocations resolved to the same network name %q", netA)
	}
	netIDA := dockerOut(t, "network", "inspect", netA, "-f", "{{.Id}}")
	netIDB := dockerOut(t, "network", "inspect", netB, "-f", "{{.Id}}")
	t.Logf("[isolation] network A=%s (%s) network B=%s (%s)", netA, netIDA, netB, netIDB)
	if netIDA == netIDB {
		t.Errorf("[isolation] both allocations share one network ID %s, want distinct networks", netIDA)
	}

	ipB := dockerOut(t, "inspect", createdB.ProviderID, "-f",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")
	t.Logf("[isolation] allocation B container IP: %s", ipB)
	_, crossErr := dockerTry("exec", createdA.ProviderID, "sh", "-c",
		fmt.Sprintf("wget -qO- -T 4 http://%s/", ipB))
	if crossErr == nil {
		t.Errorf("[isolation] allocation A reached allocation B's container %s by IP across separate networks", ipB)
	} else {
		t.Logf("[isolation] allocation A correctly cannot reach allocation B's container %s by IP (per-allocation network separation holds)", ipB)
	}
}

// bootstrapFor is bootstrap (verify_test.go), parameterized by instance name,
// for tests that run more than one allocation under a single controller-id.
func bootstrapFor(name, metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             name,
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken,
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           params.Amd64,
		PoolID:           poolID,
		JitConfigEnabled: true,
	}
}
