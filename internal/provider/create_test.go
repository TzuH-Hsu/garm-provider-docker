package provider

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/docker"
	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

const (
	testInstanceToken = "super-secret-instance-token"
	testMetadataToken = "Bearer " + testInstanceToken
)

// newJITMetadataServer serves the three JIT credential files and asserts the
// Bearer token on every request.
func newJITMetadataServer(t *testing.T) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/credentials/runner":                "RUNNER-FILE",
		"/credentials/credentials":           "CREDENTIALS-FILE",
		"/credentials/credentials_rsaparams": "RSAPARAMS-FILE",
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != testMetadataToken {
			t.Errorf("Authorization = %q, want %q", r.Header.Get("Authorization"), testMetadataToken)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
}

// newRegistrationTokenServer serves the non-JIT registration token.
func newRegistrationTokenServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != testMetadataToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/runner-registration-token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("REG-TOKEN"))
	}))
}

func jitBootstrap(metadataURL string) params.BootstrapInstance {
	return params.BootstrapInstance{
		Name:             "Test-Instance-01",
		RepoURL:          "https://github.com/example-org/example-repo",
		MetadataURL:      metadataURL,
		InstanceToken:    testInstanceToken,
		OSType:           params.Linux,
		OSArch:           params.Amd64,
		PoolID:           "pool-xyz",
		JitConfigEnabled: true,
	}
}

// listAll returns the number of containers in the fake regardless of labels,
// so tests can assert "zero leftovers".
func listAll(t *testing.T, p *Provider) int {
	t.Helper()
	out, err := p.cli.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		t.Fatalf("ContainerList returned unexpected error: %v", err)
	}
	return len(out)
}

// decodeTar decodes a tar archive into a name→contents map.
func decodeTar(t *testing.T, data []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		var body bytes.Buffer
		if _, err := io.Copy(&body, tr); err != nil {
			t.Fatalf("reading tar body: %v", err)
		}
		out[hdr.Name] = body.String()
	}
	return out
}

// tarEntryOrder returns the tar entry names in archive order, so a test can
// assert the ready marker is delivered last.
func tarEntryOrder(t *testing.T, data []byte) []string {
	t.Helper()
	var order []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		order = append(order, hdr.Name)
	}
	return order
}

func TestCreateInstanceHappyPathJIT(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	inst, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}

	// Returned ProviderInstance.
	if inst.ProviderID == "" {
		t.Error("ProviderID is empty, want the container ID")
	}
	if inst.Name != "Test-Instance-01" {
		t.Errorf("Name = %q, want Test-Instance-01", inst.Name)
	}
	if inst.Status != params.InstanceRunning {
		t.Errorf("Status = %q, want running", inst.Status)
	}
	if inst.OSType != params.Linux || inst.OSArch != params.Amd64 {
		t.Errorf("os fields = %q/%q, want linux/amd64", inst.OSType, inst.OSArch)
	}

	// Image was pulled (was missing in the fake).
	if len(fake.PulledImages) != 1 || fake.PulledImages[0] != "ghcr.io/example/runner@sha256:deadbeef" {
		t.Errorf("PulledImages = %v, want the runner image once", fake.PulledImages)
	}

	// Container exists with the right labels.
	got, err := fake.ContainerInspect(context.Background(), inst.ProviderID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	labels := got.Config.Labels
	if labels[spec.LabelManaged] != "true" ||
		labels[spec.LabelControllerID] != "controller-abc" ||
		labels[spec.LabelInstanceName] != "Test-Instance-01" ||
		labels[spec.LabelRole] != spec.RoleRunner ||
		labels[spec.LabelPoolID] != "pool-xyz" {
		t.Errorf("labels missing/incorrect: %v", labels)
	}
	if labels[spec.LabelOSType] != "linux" || labels[spec.LabelOSArch] != "amd64" {
		t.Errorf("os labels = %q/%q", labels[spec.LabelOSType], labels[spec.LabelOSArch])
	}

	// Defect 1: the credential tmpfs must be requested via HostConfig.Tmpfs
	// (short syntax) at the credential dir, owned by the runner uid/gid and
	// mode 0700 — the config the real daemon honors (the Mounts long-syntax
	// uid/gid it rejects). This is the load-bearing assertion that CreateInstance
	// builds a daemon-acceptable tmpfs.
	tmpfsMounts := fake.TmpfsMounts(inst.ProviderID)
	tmpfsOpts, ok := tmpfsMounts[spec.CredentialDir]
	if !ok {
		t.Fatalf("HostConfig.Tmpfs missing the %q credential tmpfs: %v", spec.CredentialDir, tmpfsMounts)
	}
	for _, want := range []string{"uid=1001", "gid=1001", "mode=0700"} {
		if !strings.Contains(tmpfsOpts, want) {
			t.Errorf("credential tmpfs options %q missing %q", tmpfsOpts, want)
		}
	}

	// Credential invisibility (ADR-002 / M0 gate): no env var may carry the
	// instance token or the metadata URL.
	for _, e := range got.Config.Env {
		if strings.Contains(e, testInstanceToken) {
			t.Errorf("env var leaks the instance token: %q", e)
		}
		if strings.Contains(e, srv.URL) {
			t.Errorf("env var leaks the metadata URL: %q", e)
		}
	}
	// JIT env is present; non-JIT-only vars are absent.
	if !hasEnv(got.Config.Env, "JIT_CONFIG_ENABLED=true") {
		t.Errorf("expected JIT_CONFIG_ENABLED=true in env, got %v", got.Config.Env)
	}
	if hasEnvPrefix(got.Config.Env, "RUNNER_EPHEMERAL=") {
		t.Errorf("JIT mode must not set RUNNER_EPHEMERAL, got %v", got.Config.Env)
	}

	// Credentials were streamed into the tmpfs via a docker-exec'd tar.
	if len(fake.Execs) != 1 {
		t.Fatalf("Execs = %d, want 1", len(fake.Execs))
	}
	ex := fake.Execs[0]
	if ex.ContainerID != inst.ProviderID {
		t.Errorf("exec target = %s, want %s", ex.ContainerID, inst.ProviderID)
	}
	// The delivery command must extract the tar into the credential tmpfs.
	if strings.Join(ex.Cmd, " ") != strings.Join(credentialDeliverCmd, " ") {
		t.Errorf("exec cmd = %v, want %v", ex.Cmd, credentialDeliverCmd)
	}
	files := decodeTar(t, ex.Stdin)
	for name, want := range map[string]string{
		"runner":                "RUNNER-FILE",
		"credentials":           "CREDENTIALS-FILE",
		"credentials_rsaparams": "RSAPARAMS-FILE",
	} {
		if files[name] != want {
			t.Errorf("credential %q = %q, want %q", name, files[name], want)
		}
	}
	// The atomic ready marker is delivered as the archive's last entry.
	if _, ok := files[spec.ReadyMarker]; !ok {
		t.Errorf("credential archive missing the %q ready marker (files: %v)", spec.ReadyMarker, files)
	}
	names := tarEntryOrder(t, ex.Stdin)
	if len(names) == 0 || names[len(names)-1] != spec.ReadyMarker {
		t.Errorf("ready marker must be the LAST tar entry, got order %v", names)
	}
}

func TestCreateInstanceHappyPathNonJIT(t *testing.T) {
	srv := newRegistrationTokenServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	b := jitBootstrap(srv.URL)
	b.JitConfigEnabled = false
	b.Labels = []string{"self-hosted", "linux"}

	inst, err := p.CreateInstance(context.Background(), b)
	if err != nil {
		t.Fatalf("CreateInstance (non-JIT) returned unexpected error: %v", err)
	}

	got, err := fake.ContainerInspect(context.Background(), inst.ProviderID)
	if err != nil {
		t.Fatalf("ContainerInspect returned unexpected error: %v", err)
	}
	// Non-JIT env: entity + ephemeral, and still no token.
	if !hasEnv(got.Config.Env, "RUNNER_ORG=example-org") || !hasEnv(got.Config.Env, "RUNNER_REPO=example-repo") {
		t.Errorf("expected RUNNER_ORG/REPO in non-JIT env, got %v", got.Config.Env)
	}
	if !hasEnv(got.Config.Env, "RUNNER_EPHEMERAL=true") {
		t.Errorf("expected RUNNER_EPHEMERAL=true in non-JIT env, got %v", got.Config.Env)
	}
	if !hasEnv(got.Config.Env, "RUNNER_LABELS=self-hosted,linux") {
		t.Errorf("expected RUNNER_LABELS in non-JIT env, got %v", got.Config.Env)
	}
	for _, e := range got.Config.Env {
		if strings.Contains(e, testInstanceToken) {
			t.Errorf("non-JIT env leaks the instance token: %q", e)
		}
	}

	// The registration token is delivered as the single credential file,
	// alongside the ready marker.
	if len(fake.Execs) != 1 {
		t.Fatalf("Execs = %d, want 1", len(fake.Execs))
	}
	files := decodeTar(t, fake.Execs[0].Stdin)
	if files["registration-token"] != "REG-TOKEN" {
		t.Errorf("registration token file = %q, want REG-TOKEN (files: %v)", files["registration-token"], files)
	}
	if _, ok := files[spec.ReadyMarker]; !ok {
		t.Errorf("credential archive missing the %q ready marker (files: %v)", spec.ReadyMarker, files)
	}
}

func TestCreateInstancePullFailureLeavesNoLeftovers(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.PullErr = errors.New("registry unreachable")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on image pull, got nil")
	}
	// The claim network + workspace volume were created before the pull; the
	// guard must roll back ALL kinds, not just the (absent) container.
	assertNoLeftovers(t, fake)
	if len(fake.Execs) != 0 {
		t.Errorf("no credentials should have been delivered, got %d execs", len(fake.Execs))
	}
}

func TestCreateInstanceDuplicateReturnsExit31(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)

	// Seed an existing managed CLAIM-MARKER network for the same instance name
	// (ADR-004): duplicate detection now keys off the network claim marker, not
	// a runner container. A concurrent, still-in-flight CreateInstance that has
	// not reached its container step yet is detected exactly this way.
	seedClaimNetwork(t, fake, "Test-Instance-01", "peer-nonce")

	_, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected a duplicate error, got nil")
	}
	if !errors.Is(err, gErrors.ErrDuplicateEntity) {
		t.Errorf("error is not a duplicate error: %v", err)
	}
	if code := execcommon.ResolveErrorToExitCode(err); code != execcommon.ExitCodeDuplicate {
		t.Errorf("exit code = %d, want %d (duplicate)", code, execcommon.ExitCodeDuplicate)
	}
	// The losing create must not pull an image, create a container, or create a
	// workspace volume — it stops at the claim-marker 409.
	if len(fake.PulledImages) != 0 {
		t.Errorf("duplicate must not pull an image, got %v", fake.PulledImages)
	}
	if n := listAll(t, p); n != 0 {
		t.Errorf("container count = %d, want 0 (no runner created by the loser)", n)
	}
	if len(fake.Execs) != 0 {
		t.Errorf("the losing create must not deliver credentials, got %d execs", len(fake.Execs))
	}
	// The peer's claim marker is untouched (still exactly one, original nonce).
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 1 || nets[0].Labels[spec.LabelCreateNonce] != "peer-nonce" {
		t.Errorf("peer claim marker was clobbered: %+v", nets)
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("the losing create left %d volumes, want 0", len(vols.Volumes))
	}
}

// seedClaimNetwork seeds a managed claim-marker job network for instanceName
// with the given nonce (this controller), as a concurrent peer's or a live
// allocation's claim marker would appear.
func seedClaimNetwork(t *testing.T, fake *docker.FakeClient, instanceName, nonce string) {
	t.Helper()
	id := spec.AllocationIdentity{ControllerID: "controller-abc", PoolID: "pool-xyz", InstanceName: instanceName}
	labels := id.NetworkLabels(time.Now())
	labels[spec.LabelCreateNonce] = nonce
	if _, err := fake.NetworkCreate(context.Background(), spec.JobNetworkName(instanceName), network.CreateOptions{Labels: labels}); err != nil {
		t.Fatalf("seed claim NetworkCreate returned unexpected error: %v", err)
	}
}

func TestCreateInstanceSkipsPullWhenImagePresent(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.PresentImages["ghcr.io/example/runner@sha256:deadbeef"] = true

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err != nil {
		t.Fatalf("CreateInstance returned unexpected error: %v", err)
	}
	if len(fake.PulledImages) != 0 {
		t.Errorf("image was already present; expected no pull, got %v", fake.PulledImages)
	}
}

func TestCreateInstanceNonJITBadRepoURLFailsCleanly(t *testing.T) {
	srv := newRegistrationTokenServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	b := jitBootstrap(srv.URL)
	b.JitConfigEnabled = false
	b.RepoURL = "https://github.com" // host only, no entity path

	if _, err := p.CreateInstance(context.Background(), b); err == nil {
		t.Fatal("expected an error for an unparseable non-JIT repo_url, got nil")
	}
	// The env is built after the claim network + volume; the guard rolls back
	// all kinds so nothing is left.
	assertNoLeftovers(t, fake)
}

func TestCreateInstanceExecDeliveryFailureCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.ExecErr = errors.New("exec into container failed")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on credential delivery, got nil")
	}
	// The creation guard must have rolled back the container, network, and
	// workspace volume it created.
	assertNoLeftovers(t, fake)
}

func TestCreateInstanceExecNonZeroExitCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.ExecExitCode = 2 // tar ran but failed

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on a non-zero delivery exit, got nil")
	}
	assertNoLeftovers(t, fake)
}

func TestCreateInstanceStartFailureCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.StartErr = errors.New("start boom")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on start, got nil")
	}
	assertNoLeftovers(t, fake)
	if len(fake.Execs) != 0 {
		t.Errorf("a start failure must not deliver credentials, got %d execs", len(fake.Execs))
	}
}

func TestCreateInstanceAmbiguousContainerCreateCleansUpAllKinds(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	// ContainerCreate errors but the daemon still created the container
	// (ADR-004 ambiguous create): since the leaked container carries THIS
	// attempt's create-nonce, the nonce-keyed rollback must remove it along
	// with the claim network and workspace volume created earlier in the flow.
	fake.CreateErr = errors.New("transient daemon error")
	fake.CreateErrLeaksContainer = true

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on create, got nil")
	}
	assertNoLeftovers(t, fake)
}

func TestCreateInstanceContainerCreateFailureLeavesForeignUntouched(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	// A foreign container occupies the deterministic Docker name our create
	// would use (RunnerContainerName lowercases to "test-instance-01").
	foreignID := seedRunner(t, fake, "Test-Instance-01", "p1", "different-controller", "running")

	// A clean container-create failure (name conflict): the nonce-keyed rollback
	// must remove only our resources and never the foreign container.
	fake.CreateErr = errors.New("name conflict")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on create, got nil")
	}
	if _, err := fake.ContainerInspect(context.Background(), foreignID); err != nil {
		t.Errorf("rollback removed a foreign container: %v", err)
	}
	// Our own claim network + workspace volume were rolled back; only the
	// foreign container remains.
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 0 {
		t.Errorf("rollback left %d networks, want 0", len(nets))
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("rollback left %d volumes, want 0", len(vols.Volumes))
	}
	if n := listAll(t, p); n != 1 {
		t.Errorf("container count = %d, want 1 (only the foreign container)", n)
	}
}

// assertNoLeftovers asserts zero managed resources of ANY kind remain — the
// creation-guard "zero leftovers of any kind" contract (ADR-004).
func assertNoLeftovers(t *testing.T, fake *docker.FakeClient) {
	t.Helper()
	conts, _ := fake.ContainerList(context.Background(), container.ListOptions{All: true})
	if len(conts) != 0 {
		t.Errorf("leftover containers = %d, want 0", len(conts))
	}
	nets, _ := fake.NetworkList(context.Background(), network.ListOptions{})
	if len(nets) != 0 {
		t.Errorf("leftover networks = %d, want 0", len(nets))
	}
	vols, _ := fake.VolumeList(context.Background(), volume.ListOptions{})
	if len(vols.Volumes) != 0 {
		t.Errorf("leftover volumes = %d, want 0", len(vols.Volumes))
	}
}

func TestCreateInstanceRejectsUnsupportedPlatform(t *testing.T) {
	tests := []struct {
		name   string
		osType params.OSType
		osArch params.OSArch
	}{
		{"windows rejected", params.Windows, params.Amd64},
		{"arm64 rejected in M0", params.Linux, params.Arm64},
		{"empty os rejected", "", params.Amd64},
		{"empty arch rejected", params.Linux, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, fake := newTestProvider(t)
			// Metadata URL is deliberately never contacted: platform
			// validation runs before any Docker op or credential fetch.
			b := jitBootstrap("https://metadata.invalid/")
			b.OSType = tt.osType
			b.OSArch = tt.osArch

			if _, err := p.CreateInstance(context.Background(), b); err == nil {
				t.Fatal("expected an unsupported-platform error, got nil")
			}
			if n := listAll(t, p); n != 0 {
				t.Errorf("platform rejection left %d containers behind, want 0", n)
			}
			if len(fake.PulledImages) != 0 {
				t.Errorf("platform rejection must not pull an image, got %v", fake.PulledImages)
			}
			if len(fake.Execs) != 0 {
				t.Errorf("platform rejection must not deliver credentials, got %d execs", len(fake.Execs))
			}
		})
	}
}

// hasEnv reports whether env contains an exact entry.
func hasEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// hasEnvPrefix reports whether env contains an entry with the given prefix.
func hasEnvPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
