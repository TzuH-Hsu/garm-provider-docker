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
	kept := []string{
		"Current runner version: '2.329.0'",
		"√ Connected to GitHub",
		"2026-11-03 10:00:00Z: Listening for Jobs",
		"2026-11-03 10:05:00Z: Running job: build",
		"2026-11-03 10:05:00Z: Running job: integration-tests",
		"2026-11-03 10:09:30Z: Job build completed with result: Failed",
		"An error occurred: Runner listener exit with retryable error, re-launch runner in 5 seconds.",
		"Runner listener exit because of updating, re-launch runner after successful update",
		"Runner.Listener exited with code 137",
		"[entrypoint] waiting up to 120s for the delivery marker /run/garm/.delivered",
		"[entrypoint] dropping privileges to runner via gosu",
		"[entrypoint] raised open-file soft limit from 1024 to 65536 (hard limit 524288)",
	}
	safe, withheld := SafeLogLines(kept, 0)
	if withheld != 0 {
		t.Errorf("withheld %d ordinary lines, want 0", withheld)
	}
	for i, line := range kept {
		if safe[i] != line {
			t.Errorf("line %q was withheld or changed to %q", line, safe[i])
		}
	}
}

// TestSafeLogLinesWithholdsLongRunsWhateverTheirMix pins that a run of 20 or
// more credential-alphabet characters withholds its line by length alone. A
// composition rule (digit density, case mix) let credential-shaped values
// through, so none applies; harmless long paths, digests and identifiers are
// withheld with them.
func TestSafeLogLinesWithholdsLongRunsWhateverTheirMix(t *testing.T) {
	for name, line := range map[string]string{
		"lower with few digits":      "key: " + "qwertyuiopasdfghjklz" + "xcvbnm123qwertyuiopasd",
		"upper 29 without digits":    "id " + "QWERTYUIOPASDFGHJ" + "KLZXCVBNMQWE",
		"all lower 40":               "value=" + strings.Repeat("abcdefghij", 4),
		"all upper 20":               "x " + strings.Repeat("ABCDE", 4),
		"mixed with dashes":          "v " + "abc-DEF_ghi-" + "JKL_mno-PQR",
		"base64 with slash and plus": "blob " + "ab+cd/ef+gh" + "/ij+kl/mn==",
		"path":                       "/actions-runner/_work/_tool/node/20.19.1/x64/bin/node exited",
		"api path":                   "Http response code: Unauthorized from 'POST https://pipelines.actions.example/abc/_apis/distributedtask/pools/1/sessions'",
		"digest":                     "pulled sha256:" + strings.Repeat("9c8e5a1d", 8),
		"long identifier":            "ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1",
	} {
		safe, withheld := SafeLogLines([]string{line}, 0)
		if withheld != 1 || safe[0] != WithheldLine {
			t.Errorf("%s: %q kept as %q, want it withheld", name, line, safe[0])
		}
	}
	// One short of the threshold is kept.
	if safe, withheld := SafeLogLines([]string{"x " + strings.Repeat("a", credentialRunLen-1)}, 0); withheld != 0 {
		t.Errorf("a %d-character run was withheld as %q", credentialRunLen-1, safe[0])
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
