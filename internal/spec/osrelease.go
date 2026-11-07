package spec

import (
	"fmt"
	"regexp"
	"strings"
)

// osReleasePattern is the grammar of a runner image's os-release label. The value is a
// segment of the toolcache and pnpm volume names, so it must be Docker-name-safe and
// short. Lowercase only, so two spellings can never name two caches for one release.
var osReleasePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// imageOSKeyPrefix marks an OS key derived from the image content ID because
// the image carries no garm.docker/os-release label. A label value may not use
// it, so a labelled key and a derived key can never be equal.
const imageOSKeyPrefix = "image-"

// imageOSKeyHashLen is the number of image-ID hex characters in a derived OS
// key: 128 bits, the same truncation RepoKey uses for its hash suffix.
const imageOSKeyHashLen = 32

// RunnerOSKey returns the OS key for the toolcache and pnpm volumes, from the runner
// image's labels and its content ID ("sha256:<hex>"). With a garm.docker/os-release
// label, the key is its value, so a rebuilt image on the same release keeps its warm
// caches. A malformed value is an error: the label is in this provider's namespace, so
// it means a broken image. Without the label, the key is "image-" plus 32 hex characters
// of the content ID, which is always correct but cold after every rebuild (ADR-003).
func RunnerOSKey(imageLabels map[string]string, imageID string) (string, error) {
	if v, ok := imageLabels[LabelOSRelease]; ok {
		if !osReleasePattern.MatchString(v) || strings.HasPrefix(v, imageOSKeyPrefix) {
			return "", fmt.Errorf("runner image label %s=%q is invalid: it must match %s and must not start with %q", LabelOSRelease, v, osReleasePattern.String(), imageOSKeyPrefix)
		}
		return v, nil
	}

	hexID := imageID
	if i := strings.IndexByte(hexID, ':'); i >= 0 {
		hexID = hexID[i+1:]
	}
	if len(hexID) < imageOSKeyHashLen || !isLowerHex(hexID) {
		return "", fmt.Errorf("runner image has no %s label and its content ID %q is not a sha256 digest to derive an OS key from", LabelOSRelease, imageID)
	}
	return imageOSKeyPrefix + hexID[:imageOSKeyHashLen], nil
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
