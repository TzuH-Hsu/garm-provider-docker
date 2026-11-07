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

// TestVerifyEgressAndIsolation proves that job networks default to
// internal=false, so the runner keeps egress; that internal=true still blocks it;
// and that job-to-job isolation comes from each allocation owning its own network,
// not from the internal flag (ADR-001, Job network). It runs two allocations under
// one controller ID. One has no [network] block and must report Internal=false and
// reach the public internet. The other sets [network] internal=true and must report
// Internal=true and be blocked. The test then checks that they sit on different
// networks and that the default runner cannot reach the other runner by container
// IP. Only a real daemon shows real routing and firewalling.
func TestVerifyEgressAndIsolation(t *testing.T) {
	root := repoRoot(t)
	controllerID := randControllerID(t)

	imageTag := "garm-alloc-verify-sleep:latest"
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

	// Remove everything for this controller ID, whether or not the DeleteInstance
	// calls below succeed.
	defer cleanupController(t, controllerID)

	// Allocation A: the default config gives internal=false and working egress.
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
		t.Errorf("[default] job network Internal=%s, want false (default)", internalA)
	}
	egressOutA, egressErrA := dockerTry("exec", createdA.ProviderID, "sh", "-c", "wget -qO- -T 6 https://api.github.com/zen")
	if egressErrA != nil {
		t.Errorf("[default] runner could NOT reach api.github.com on an internal=false network: %v\n%s", egressErrA, egressOutA)
	} else {
		t.Logf("[default] egress succeeded, api.github.com/zen replied: %q", strings.TrimSpace(egressOutA))
	}

	// Allocation B: internal=true must block egress, so the opt-in still works for
	// operators who want the stricter posture.
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

	// Isolation: two distinct networks, and no reachability by IP between the
	// allocations, whatever either one's internal setting.
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

// bootstrapFor is bootstrap with a caller-chosen instance name, for tests that run
// several allocations under one controller ID.
func bootstrapFor(name, metadataURL string, caBundle []byte) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             name,
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    instanceToken,
		CACertBundle:     caBundle,
		OSType:           params.Linux,
		OSArch:           hostOSArch(),
		PoolID:           poolID,
		JitConfigEnabled: true,
	}
}
