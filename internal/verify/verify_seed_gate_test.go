//go:build dockerverify

// This file adds the M2 consumer-side externals seed-gate real-daemon
// verification (ADR-003 W2 structural redesign, 2026-07-22). It exercises the
// gate VERBATIM against the production runner entrypoint by SOURCING the REAL
// runner-images/noble/entrypoint.sh (whose main is guarded not to run when
// sourced) and invoking the REAL wait_for_externals_seeded under that file's own
// `set -euo pipefail` — the same source-the-real-entrypoint discipline the H4
// non-root tests use — so the behavior proven here is the production code, not a
// re-implementation.
//
// It proves:
//
//   - marker PRESENT  → the gate returns immediately and the runner proceeds;
//
//   - marker ABSENT   → the gate FAILS CLOSED after its bounded timeout with a
//     clear message (the runner never starts against a half-seeded externals tree);
//
//   - env UNSET       → the gate is a no-op (cache disabled / no externals mount).
//
//     go test -tags dockerverify -v -run TestVerifyM2SeedGate ./internal/verify/
//
// It builds a tiny bash image (no GitHub, no 380MB copy) and touches no foreign
// resource.
package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildSeedGateImage layers a wrapper on a minimal bash image that COPIES the
// REAL production entrypoint.sh, SOURCES it, and invokes the REAL
// wait_for_externals_seeded — so the gate is exercised verbatim. It prints a
// GATE_PASSED sentinel only if the gate returns (the marker-present / unset paths);
// on the marker-absent path the entrypoint's `fail` exits non-zero before it.
func buildSeedGateImage(t *testing.T, root, tag string) {
	t.Helper()
	dir := t.TempDir()

	// Copy the real production entrypoint verbatim into the build context.
	realEntrypoint := filepath.Join(root, "runner-images", "noble", "entrypoint.sh")
	data, err := os.ReadFile(realEntrypoint)
	if err != nil {
		t.Fatalf("read real entrypoint.sh: %v", err)
	}
	writeFile(t, filepath.Join(dir, "entrypoint.sh"), string(data))

	wrapper := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"# shellcheck source=/dev/null\n" +
		"source /entrypoint.sh\n" +
		"wait_for_externals_seeded\n" +
		"echo GATE_PASSED\n"
	writeFile(t, filepath.Join(dir, "gate.sh"), wrapper)

	dockerfile := "FROM bash:5\n" +
		"COPY --chmod=0755 entrypoint.sh /entrypoint.sh\n" +
		"COPY --chmod=0755 gate.sh /gate.sh\n" +
		"ENTRYPOINT [\"/gate.sh\"]\n"
	writeFile(t, filepath.Join(dir, "Dockerfile"), dockerfile)
	if out, err := dockerTry("build", "-t", tag, dir); err != nil {
		t.Fatalf("build seed-gate image: %v\n%s", err, out)
	}
}

func TestVerifyM2SeedGate(t *testing.T) {
	root := repoRoot(t)
	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")

	tag := "garm-seedgate:" + randHex(t)
	buildSeedGateImage(t, root, tag)
	defer func() { _, _ = dockerTry("rmi", "-f", tag) }()

	// marker PRESENT (a path that always exists) → gate proceeds immediately.
	t.Run("marker_present_proceeds", func(t *testing.T) {
		out, err := dockerTry("run", "--rm",
			"-e", "GARM_EXTERNALS_SEEDED_MARKER=/etc/hostname",
			tag)
		if err != nil {
			t.Fatalf("[seed-gate] gate failed with the marker PRESENT: %v\n%s", err, out)
		}
		if !strings.Contains(out, "GATE_PASSED") {
			t.Errorf("[seed-gate] gate did not proceed with the marker present: %s", out)
		}
		if !strings.Contains(out, "fully seeded") {
			t.Errorf("[seed-gate] gate did not log seed completion: %s", out)
		}
		t.Logf("[seed-gate] marker present → runner proceeds")
	})

	// marker ABSENT with a short timeout → fail closed with a clear message.
	t.Run("marker_absent_fails_closed", func(t *testing.T) {
		out, err := dockerTry("run", "--rm",
			"-e", "GARM_EXTERNALS_SEEDED_MARKER=/nonexistent/.garm-seeded",
			"-e", "GARM_EXTERNALS_WAIT_SECONDS=2",
			tag)
		if err == nil {
			t.Fatalf("[seed-gate] gate PASSED with the marker ABSENT; want a fail-closed timeout. out=%s", out)
		}
		if strings.Contains(out, "GATE_PASSED") {
			t.Errorf("[seed-gate] the runner proceeded despite a missing marker: %s", out)
		}
		if !strings.Contains(out, "timed out waiting for the externals cache to be seeded") {
			t.Errorf("[seed-gate] fail-closed message missing/unclear: %s", out)
		}
		t.Logf("[seed-gate] marker absent → fail closed after the bounded timeout")
	})

	// env UNSET → the gate is a no-op (no externals mount to wait on).
	t.Run("env_unset_noop", func(t *testing.T) {
		out, err := dockerTry("run", "--rm", tag)
		if err != nil {
			t.Fatalf("[seed-gate] gate errored with the env UNSET (should be a no-op): %v\n%s", err, out)
		}
		if !strings.Contains(out, "GATE_PASSED") {
			t.Errorf("[seed-gate] gate did not no-op with the env unset: %s", out)
		}
		if strings.Contains(out, "waiting up to") {
			t.Errorf("[seed-gate] gate waited despite the env being unset: %s", out)
		}
		t.Logf("[seed-gate] env unset → no-op")
	})

	assertForeignPresent(t, "hbot-lab-mongodb")
	assertForeignPresent(t, "hummingbot")
}
