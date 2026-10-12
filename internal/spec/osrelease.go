package spec

import (
	"fmt"
	"regexp"
	"strings"
)

// osReleasePattern is the grammar a runner image's garm.docker/os-release label
// value must match. The value is embedded in the toolcache and pnpm volume
// names (ToolcacheVolumeName, PnpmVolumeName), so it must be a Docker-name-safe
// segment on its own; the length cap keeps those names well inside Docker's
// 255-byte limit. Lowercase only, so "Ubuntu-26.04" and "ubuntu-26.04" can never
// name two different caches for the same release.
var osReleasePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// imageOSKeyPrefix marks an OS key derived from the image content ID because
// the image carries no garm.docker/os-release label. A label value may not use
// it, so a labelled key and a derived key can never be equal.
const imageOSKeyPrefix = "image-"

// imageOSKeyHashLen is the number of image-ID hex characters in a derived OS
// key: 128 bits, the same truncation RepoKey uses for its hash suffix.
const imageOSKeyHashLen = 32

// RunnerOSKey derives the OS key the toolcache and pnpm store volumes are keyed
// on (ADR-003, 2026-10-12 amendment) from the runner image's config labels and
// its content ID (an image inspect's ID, "sha256:<hex>").
//
// An image that carries the garm.docker/os-release label (both images this
// repository publishes do) yields that label's value, e.g. "ubuntu-26.04", so a
// rebuilt image on the same Ubuntu release keeps its warm caches. A value that
// does not match osReleasePattern, or that uses the derived-key prefix, is an
// error: the label is in this provider's own namespace, so a malformed value is
// a broken image, and failing the create is better than guessing.
//
// An image without the label yields "image-" plus the first 32 hex characters
// of its content ID. That keys the caches to the exact image: always correct,
// since two different OS releases can never share a content ID, at the cost of
// a cold cache whenever the image is rebuilt. Operators building their own
// images set the label to avoid that.
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

// isLowerHex reports whether s consists only of lowercase hex digits.
func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
