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

	gErrors "github.com/cloudbase/garm-provider-common/errors"
	execcommon "github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/docker/docker/api/types/container"

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

	// Credentials were streamed to the tmpfs via docker cp.
	if len(fake.Copies) != 1 {
		t.Fatalf("Copies = %d, want 1", len(fake.Copies))
	}
	cp := fake.Copies[0]
	if cp.ContainerID != inst.ProviderID || cp.DstPath != spec.CredentialDir {
		t.Errorf("copy target = %s:%s, want %s:%s", cp.ContainerID, cp.DstPath, inst.ProviderID, spec.CredentialDir)
	}
	files := decodeTar(t, cp.Content)
	for name, want := range map[string]string{
		"runner":                "RUNNER-FILE",
		"credentials":           "CREDENTIALS-FILE",
		"credentials_rsaparams": "RSAPARAMS-FILE",
	} {
		if files[name] != want {
			t.Errorf("credential %q = %q, want %q", name, files[name], want)
		}
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

	// The registration token is delivered as the single credential file.
	if len(fake.Copies) != 1 {
		t.Fatalf("Copies = %d, want 1", len(fake.Copies))
	}
	files := decodeTar(t, fake.Copies[0].Content)
	if files["registration-token"] != "REG-TOKEN" {
		t.Errorf("registration token file = %q, want REG-TOKEN (files: %v)", files["registration-token"], files)
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
	if n := listAll(t, p); n != 0 {
		t.Errorf("pull failure left %d containers behind, want 0", n)
	}
	if len(fake.Copies) != 0 {
		t.Errorf("no credentials should have been delivered, got %d copies", len(fake.Copies))
	}
}

func TestCreateInstanceDuplicateReturnsExit31(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)

	// Seed an existing managed container with the same instance-name label.
	_, err := fake.ContainerCreate(context.Background(), &container.Config{
		Labels: map[string]string{
			spec.LabelManaged:      "true",
			spec.LabelControllerID: "controller-abc",
			spec.LabelInstanceName: "Test-Instance-01",
			spec.LabelRole:         spec.RoleRunner,
		},
	}, nil, nil, nil, "test-instance-01")
	if err != nil {
		t.Fatalf("seed ContainerCreate returned unexpected error: %v", err)
	}

	_, err = p.CreateInstance(context.Background(), jitBootstrap(srv.URL))
	if err == nil {
		t.Fatal("expected a duplicate error, got nil")
	}
	if !errors.Is(err, gErrors.ErrDuplicateEntity) {
		t.Errorf("error is not a duplicate error: %v", err)
	}
	if code := execcommon.ResolveErrorToExitCode(err); code != execcommon.ExitCodeDuplicate {
		t.Errorf("exit code = %d, want %d (duplicate)", code, execcommon.ExitCodeDuplicate)
	}
	// No new container / no image pull for a rejected duplicate.
	if len(fake.PulledImages) != 0 {
		t.Errorf("duplicate must not pull an image, got %v", fake.PulledImages)
	}
	if n := listAll(t, p); n != 1 {
		t.Errorf("container count = %d, want 1 (only the seeded one)", n)
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

	p, _ := newTestProvider(t)
	b := jitBootstrap(srv.URL)
	b.JitConfigEnabled = false
	b.RepoURL = "https://github.com" // host only, no entity path

	if _, err := p.CreateInstance(context.Background(), b); err == nil {
		t.Fatal("expected an error for an unparseable non-JIT repo_url, got nil")
	}
	// The env is built before the container is created, so nothing is left.
	if n := listAll(t, p); n != 0 {
		t.Errorf("bad repo_url left %d containers behind, want 0", n)
	}
}

func TestCreateInstanceCopyFailureCleansUp(t *testing.T) {
	srv := newJITMetadataServer(t)
	defer srv.Close()

	p, fake := newTestProvider(t)
	fake.CopyErr = errors.New("cp into container failed")

	if _, err := p.CreateInstance(context.Background(), jitBootstrap(srv.URL)); err == nil {
		t.Fatal("expected CreateInstance to fail on credential delivery, got nil")
	}
	// The creation guard must have removed the container it created.
	if n := listAll(t, p); n != 0 {
		t.Errorf("copy failure left %d containers behind, want 0 (guard should clean up)", n)
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
