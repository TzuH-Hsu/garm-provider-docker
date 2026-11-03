package logging

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// fakeSecrets builds realistic-looking but fake credential material at run
// time. Each value is assembled from pieces so that no credential-shaped
// literal sits in the source for a secret scanner to flag.
func fakeSecrets() map[string]string {
	mixed := strings.Repeat("Ab3dE5gH7jK9mN1pQ2rS", 5) // base64-alphabet filler

	jitJSON := `{".runner":"` + base64.StdEncoding.EncodeToString([]byte(`{"agentId":4711,"agentName":"garm-abc","poolId":1,"serverUrl":"https://pipelines.actions.example/abc"}`)) +
		`",".credentials":"` + base64.StdEncoding.EncodeToString([]byte(`{"scheme":"OAuth","data":{"clientId":"0f6c-11","authorizationUrl":"https://vstoken.actions.example/_apis/oauth2/token/abc"}}`)) +
		`",".credentials_rsaparams":"` + base64.StdEncoding.EncodeToString([]byte(`{"d":"`+mixed[:64]+`","modulus":"`+mixed+`","exponent":"AQAB"}`)) + `"}`
	jit := base64.StdEncoding.EncodeToString([]byte(jitJSON))

	return map[string]string{
		"github classic PAT":       "gh" + "p_" + mixed[:36],
		"github installation":      "gh" + "s_" + mixed[1:37],
		"github oauth":             "gh" + "o_" + mixed[2:38],
		"github fine-grained PAT":  "github" + "_pat_" + "11ABCDEFG0" + "_" + mixed[:59],
		"jit config":               jit,
		"jit config fragment":      jit[100:180],
		"registration token":       "A" + strings.ToUpper(hex.EncodeToString([]byte("registration-tok")))[:28],
		"garm instance token":      "ey" + "J" + "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + "ey" + "J" + "zdWIiOiJnYXJtIn0." + mixed[:43],
		"hex secret":               hex.EncodeToString([]byte("a-forty-character-hex-encoded-key!")),
		"lower alphanumeric key":   "k7q2m9x4" + "w1z8r3t6" + "p5n0v2y7" + "s4j9h1g3" + "c8b6",
		"aws access key":           "AK" + "IA" + "Q3EGRIDTESTKEY12",
		"slack token":              "xo" + "xb-" + "1234-" + mixed[:24],
		"rsa private key body":     mixed[:64],
		"rsa private key tail":     "Zm9v" + "YmFy" + "MTIz=",
		"metadata url with token":  "https://garm.example:9997/api/v1/metadata/runner-registration-token/?t=" + mixed[:20],
		"callback url with secret": "https://garm.example/api/v1/callbacks/status?instance=garm-abc&sig=" + mixed[:30],
		"userinfo url":             "https://x-access-" + "token:" + mixed[:30] + "@github.com/acme/app.git",
	}
}

// TestSafeLogLinesWithholdsEveryFakeSecret is the redaction proof for the
// orphan sweep's runner log tail: a realistic runner tail with credential
// material spliced into its lines in every way the runner or an image could
// print it comes out with none of that material anywhere in the result.
func TestSafeLogLinesWithholdsEveryFakeSecret(t *testing.T) {
	secrets := fakeSecrets()
	pemBegin := "-----" + "BEGIN RSA PRIVATE " + "KEY-----"
	pemEnd := "-----" + "END RSA PRIVATE " + "KEY-----"

	lines := []string{
		"[entrypoint] waiting up to 120s for the delivery marker /run/garm/.delivered",
		"√ Connected to GitHub",
		"2026-11-03 10:00:00Z: Listening for Jobs",
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + secrets["jit config"],
		"run.sh --jitconfig " + secrets["jit config"],
		"decoded: " + secrets["jit config fragment"],
		"config.sh --url https://github.com/acme --token " + secrets["registration token"],
		"registering with " + secrets["registration token"],
		"Authorization: Bearer " + secrets["garm instance token"],
		"auth header value " + secrets["garm instance token"],
		"git push using " + secrets["github classic PAT"],
		"GITHUB_TOKEN set to " + secrets["github installation"],
		"oauth " + secrets["github oauth"],
		"fine grained " + secrets["github fine-grained PAT"],
		"key " + secrets["hex secret"],
		"api " + secrets["lower alphanumeric key"],
		"aws " + secrets["aws access key"],
		"slack " + secrets["slack token"],
		"fetching " + secrets["metadata url with token"],
		"posting " + secrets["callback url with secret"],
		"cloning " + secrets["userinfo url"],
		pemBegin,
		secrets["rsa private key body"],
		secrets["rsa private key tail"],
		pemEnd,
		// A PEM body whose BEGIN line scrolled out of the tail.
		secrets["rsa private key body"],
		"  " + secrets["rsa private key tail"],
		`{"clientId":"0f6c-11","modulus":"` + secrets["rsa private key body"] + `"}`,
		"2026-11-03 10:05:00Z: Running job: build",
	}

	safe, withheld := SafeLogLines(lines, 240)
	out := strings.Join(safe, "\n")

	for name, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%s leaked into the screened tail:\n%s", name, out)
		}
		// No 12-character window of a secret may survive either, so a line
		// that was cut or partly kept cannot carry a fragment.
		for i := 0; i+12 <= len(s); i++ {
			if strings.Contains(out, s[i:i+12]) {
				t.Errorf("a fragment of %s (%q) leaked into the screened tail", name, s[i:i+12])
				break
			}
		}
	}
	for _, kept := range []string{
		"√ Connected to GitHub",
		"2026-11-03 10:00:00Z: Listening for Jobs",
		"2026-11-03 10:05:00Z: Running job: build",
		"[entrypoint] waiting up to 120s for the delivery marker /run/garm/.delivered",
	} {
		if !strings.Contains(out, kept) {
			t.Errorf("harmless line %q was withheld:\n%s", kept, out)
		}
	}
	if len(safe) != len(lines) {
		t.Errorf("got %d lines, want %d (withheld lines keep their place)", len(safe), len(lines))
	}
	if want := len(lines) - 4; withheld != want {
		t.Errorf("withheld = %d, want %d:\n%s", withheld, want, out)
	}
}

// TestSafeLogLinesKeepsOrdinaryRunnerOutput pins that the screen is not so
// coarse it drops the lines an operator needs to see why a runner exited.
func TestSafeLogLinesKeepsOrdinaryRunnerOutput(t *testing.T) {
	lines := []string{
		"Current runner version: '2.329.0'",
		"2026-11-03 10:00:00Z: Listening for Jobs",
		"An error occurred: Runner listener exit with retryable error, re-launch runner in 5 seconds.",
		"Runner listener exit because of updating, re-launch runner after successful update",
		"Http response code: Unauthorized from 'POST https://pipelines.actions.example/abc/_apis/distributedtask/pools/1/sessions'",
		"/actions-runner/_work/_tool/node/20.19.1/x64/bin/node exited",
		"pulled sha256:9c8e5a1d0f7b4e3a2c6d8f0b1a3c5e7d9f1b3d5f7a9c1e3f5a7b9d1f3e5a7c9b", // a digest is hex too
	}
	safe, withheld := SafeLogLines(lines, 0)
	for i, line := range lines[:6] {
		if safe[i] != line {
			t.Errorf("line %d = %q, want it kept as %q", i, safe[i], line)
		}
	}
	// A 64-hex digest is indistinguishable from a hex-encoded key, so it is
	// withheld; this is the screen's documented bias.
	if safe[6] != WithheldLine || withheld != 1 {
		t.Errorf("digest line = %q (withheld %d), want it withheld", safe[6], withheld)
	}
}

// TestSafeLogLinesMakesLinesPrintableAndBounded pins that a kept line cannot
// carry control characters or exceed the byte cap, and is cut on a rune
// boundary.
func TestSafeLogLinesMakesLinesPrintableAndBounded(t *testing.T) {
	safe, _ := SafeLogLines([]string{
		"\x1b[31mred\x1b[0m\rdone\x00",
		strings.Repeat("é", 50),
		"bad \xff utf8",
	}, 21)
	if strings.ContainsAny(safe[0], "\x1b\r\x00") {
		t.Errorf("control characters survived: %q", safe[0])
	}
	if got := safe[1]; got != strings.Repeat("é", 10)+"…" {
		t.Errorf("cut line = %q, want ten runes and an ellipsis", got)
	}
	if strings.Contains(safe[2], "\xff") {
		t.Errorf("invalid UTF-8 survived: %q", safe[2])
	}
}
