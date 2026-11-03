package logging

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// WithheldLine stands in for a container log line SafeLogLines would not pass
// through, so a reader still sees that a line was there.
const WithheldLine = "[line withheld: it may carry a credential]"

// riskyWords are case-insensitive substrings that withhold a whole line. They
// name the places a credential shows up in a runner's output: the JIT config
// and the files it unpacks into (.runner, .credentials, .credentials_rsaparams
// with its RSA modulus and exponent), registration and access tokens, HTTP
// authorization headers, GARM's metadata and status-callback endpoints, and
// key material.
var riskyWords = []string{
	"token",
	"bearer",
	"authorization",
	"jitconfig",
	"jit_config",
	"jit-config",
	"jit config",
	"credential",
	"rsaparam",
	"modulus",
	"exponent",
	"password",
	"passwd",
	"secret",
	"apikey",
	"api_key",
	"api-key",
	"private key",
	"-----begin",
	"-----end",
	"metadata",
	"callback",
	"cookie",
}

// riskyPatterns withhold a line that carries a credential-shaped value even
// when no risky word is next to it.
var riskyPatterns = []*regexp.Regexp{
	// GitHub tokens: classic PATs, OAuth, app installation, user-to-server and
	// refresh tokens, and fine-grained PATs.
	regexp.MustCompile(`gh[opsur]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_`),
	// Base64 of a JSON object or a JWT header: the JIT config blob and GARM's
	// instance token both start this way.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}`),
	// AWS access key IDs and Slack tokens, which a job's tooling may print.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[abposr]-`),
	// A URL with userinfo, or with a query string, either of which can carry
	// a credential.
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s/@]+@`),
	regexp.MustCompile(`(?i)https?://\S*\?\S*=`),
}

// tokenRun finds runs of base64/base64url/hex characters long enough to be a
// key or token (a GitHub registration token is 29 upper-case alphanumerics);
// looksLikeSecret decides whether a run is one.
var tokenRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{20,}`)

// hexSecretLen is the shortest run treated as a secret without upper case: an
// all-hex run, or a digit-dense lower-case one.
const hexSecretLen = 32

// bareBlob matches a line that is nothing but base64 characters, such as a
// line of a PEM body whose BEGIN line has already scrolled out of the tail.
var bareBlob = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{8,}$`)

// SafeLogLines returns the lines of a container's own output that may be
// logged, in order, with every line that might carry a credential replaced by
// WithheldLine, and how many lines it withheld.
//
// It exists for the one place this provider logs text it did not produce: the
// tail of a runner container's output (topology's orphan sweep). The runner
// never receives a credential in its environment or arguments (ADR-002), but
// its output is the runner software's and the image's, not this provider's,
// so it is screened rather than trusted. The screen is deliberately coarse: a
// line is withheld whole rather than masked, and anything credential-shaped
// counts, at the cost of withholding some harmless lines. A line is withheld
// when it
//
//   - contains a risky word (token, bearer, authorization, jitconfig,
//     credential, secret, password, metadata, callback, private key, ...);
//   - matches a known token shape (ghp_/ghs_/gho_/ghu_/ghr_, github_pat_,
//     base64 JSON or a JWT, AWS and Slack keys);
//   - holds a URL with userinfo or a query string;
//   - holds a run of 20 or more base64 characters that mixes upper case and
//     digits, or a run of 32 or more that is all hex or at least a fifth
//     digits;
//   - is nothing but 8 or more base64 characters, at least one of them a
//     digit or one of "+", "/" and "=";
//   - lies between a PEM BEGIN line and its END line.
//
// Lines that pass are made printable (control characters become spaces,
// invalid UTF-8 is replaced) and cut to maxLineBytes, so a line cannot forge
// log structure or grow without bound. Screening runs on the whole line before
// it is cut.
func SafeLogLines(lines []string, maxLineBytes int) (safe []string, withheld int) {
	safe = make([]string, 0, len(lines))
	inPEM := false
	for _, line := range lines {
		lower := strings.ToLower(line)
		switch {
		case strings.Contains(lower, "-----begin"):
			inPEM = true
		case inPEM && strings.Contains(lower, "-----end"):
			inPEM = false
			safe, withheld = append(safe, WithheldLine), withheld+1
			continue
		}
		if inPEM || lineIsRisky(line, lower) {
			safe, withheld = append(safe, WithheldLine), withheld+1
			continue
		}
		safe = append(safe, printable(line, maxLineBytes))
	}
	return safe, withheld
}

func lineIsRisky(line, lower string) bool {
	for _, w := range riskyWords {
		if strings.Contains(lower, w) {
			return true
		}
	}
	for _, re := range riskyPatterns {
		if re.MatchString(line) {
			return true
		}
	}
	for _, run := range tokenRun.FindAllString(line, -1) {
		if looksLikeSecret(run) {
			return true
		}
	}
	trimmed := strings.TrimSpace(line)
	return bareBlob.MatchString(trimmed) && strings.ContainsAny(trimmed, "0123456789+/=")
}

// looksLikeSecret reports whether a run of 20 or more base64/hex characters is
// key- or token-like: upper case mixed with digits (base64, or an upper-case
// token such as a registration token), 32 or more hex digits (a hex-encoded
// secret), or 32 or more characters of which at least a fifth are digits (a
// random lower-case alphanumeric key; a UUID counts too). A path or an
// identifier made of plain words is none of these.
func looksLikeSecret(run string) bool {
	var upper, nonHex bool
	digits := 0
	for _, r := range run {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
			nonHex = nonHex || r > 'F'
		case r >= 'a' && r <= 'z':
			nonHex = nonHex || r > 'f'
		case r >= '0' && r <= '9':
			digits++
		default:
			nonHex = true
		}
	}
	long := len(run) >= hexSecretLen
	return (upper && digits > 0) || (long && !nonHex) || (long && digits*5 >= len(run))
}

// printable replaces control characters with spaces and invalid UTF-8 with
// U+FFFD, and cuts s to at most maxBytes bytes on a rune boundary, marking a
// cut with an ellipsis.
func printable(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
