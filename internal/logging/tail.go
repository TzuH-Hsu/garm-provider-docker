package logging

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// WithheldLine stands in for a container log line SafeLogLines would not pass
// through, so a reader still sees that a line was there.
const WithheldLine = "[line withheld: it may carry a credential]"

// riskyWords are case-insensitive substrings that withhold a line. "modulus" and
// "exponent" cover the RSA parameters the runner unpacks from its JIT config.
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

// riskyPatterns withhold a credential-shaped value that has no risky word nearby.
var riskyPatterns = []*regexp.Regexp{
	// GitHub tokens and fine-grained PATs.
	regexp.MustCompile(`gh[opsur]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`github_pat_`),
	// Base64 of a JSON object or a JWT header: the JIT config blob and GARM's
	// instance token both start this way.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}`),
	// AWS access key IDs and Slack tokens, which a job's tooling may print.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`xox[abposr]-`),
	// A URL with userinfo or a query string.
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s/@]+@`),
	regexp.MustCompile(`(?i)https?://\S*\?\S*=`),
}

// credentialRunLen is the length at which a run of credential-alphabet characters
// withholds its line, whatever its mix: composition cannot prove a run is not a
// key. Twenty matches an AWS access key ID, the shortest common format, and still
// passes the runner's status lines. Long paths and digests are withheld too.
const credentialRunLen = 20

// credentialRun leaves out '.' so a name like Runner.Listener does not join two
// words into one run. A JWT's segments are each long enough to match alone.
var credentialRun = regexp.MustCompile(`[A-Za-z0-9+/=_-]{` + strconv.Itoa(credentialRunLen) + `,}`)

// bareBlob matches a line that is nothing but base64 characters, such as a
// line of a PEM body whose BEGIN line has already scrolled out of the tail.
var bareBlob = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{8,}$`)

// SafeLogLines returns a container's output lines in order, with each line that
// might carry a credential replaced by WithheldLine, and the number withheld.
// Other lines are made printable and cut to maxLineBytes, so a line cannot forge
// log structure or grow without bound.
//
// The container's output is not this provider's to trust. Screening is coarse and
// covers the whole line before the cut, at the cost of withholding some harmless
// lines.
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
	if credentialRun.MatchString(line) {
		return true
	}
	// A long word of letters, '_' or '-' is not a blob.
	trimmed := strings.TrimSpace(line)
	return bareBlob.MatchString(trimmed) && strings.ContainsAny(trimmed, "0123456789+/=")
}

// printable replaces control characters and invalid UTF-8, and cuts s to at most
// maxBytes bytes on a rune boundary, marking a cut with an ellipsis.
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
