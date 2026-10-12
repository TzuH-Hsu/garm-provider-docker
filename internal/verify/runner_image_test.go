//go:build dockerverify

package verify

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// runnerImageNames are the runner images this repository builds, one directory
// each under runner-images/ (ADR-002).
var runnerImageNames = []string{"noble", "resolute"}

// runnerImageBuildArgs returns the `docker build` arguments, after any
// platform/tag flags, that build the real runner image <name>: its Dockerfile
// in runner-images/<name>/ with runner-images/ as the build context, so the
// shared runner-images/common/entrypoint.sh is in reach.
func runnerImageBuildArgs(root, name string) []string {
	dir := filepath.Join(root, "runner-images")
	return []string{"-f", filepath.Join(dir, name, "Dockerfile"), dir}
}

// realEntrypointPath is the production entrypoint shared by every runner image.
func realEntrypointPath(root string) string {
	return filepath.Join(root, "runner-images", "common", "entrypoint.sh")
}

// imageOSKey returns the OS key the provider derives for a local image, using
// the same spec.RunnerOSKey the provider applies to its image inspect: the
// image's garm.docker/os-release label, or a key derived from its content ID.
func imageOSKey(t *testing.T, image string) string {
	t.Helper()
	out := dockerOut(t, "image", "inspect", image, "-f", "{{json .Config.Labels}}")
	var labels map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &labels); err != nil {
		t.Fatalf("parse labels of %q: %v (%q)", image, err, out)
	}
	id := strings.TrimSpace(dockerOut(t, "image", "inspect", image, "-f", "{{.Id}}"))
	key, err := spec.RunnerOSKey(labels, id)
	if err != nil {
		t.Fatalf("derive OS key of %q: %v", image, err)
	}
	return key
}
