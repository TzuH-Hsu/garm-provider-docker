package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// externalsSeedTimeout bounds the one-time externals seed copy (ADR-003 W2).
// The full copy is ~380MB across thousands of files (research.md §3), which can
// take a while on a cold, slow NAS filesystem; this is comfortable headroom over
// that while still bounding a wedged seed. On a warm volume the seed is a fast
// flock+marker-check no-op well within this.
const externalsSeedTimeout = 10 * time.Minute

// planExternals resolves the runner image's digest, create-or-reuses the shared
// externals volume keyed on it, and SEEDS it (blocking until seeding completes)
// so the runner never mounts a half-populated externals tree (ADR-003 W2). It
// returns the externals volume name to mount READ-ONLY into the runner.
//
// It runs for EVERY allocation when the cache feature is enabled, regardless of
// entity scope: externals are Node runtimes tied to the runner image version,
// not repository data, so they are shared across every repository and carry no
// isolation risk (the read-only mount and privileged-only seeding are what make
// that sharing safe). It is called AFTER the runner image is ensured present, so
// the image is available both as the digest source and as the seed's copy source.
func (p *Provider) planExternals(ctx context.Context, runnerImage string) (string, error) {
	digest, err := p.imageDigestHex(ctx, runnerImage)
	if err != nil {
		return "", err
	}

	name := spec.ExternalsVolumeName(digest)
	if err := spec.ValidateDerivedName("externals cache volume", name); err != nil {
		return "", err
	}

	id := spec.ExternalsVolumeIdentity{ControllerID: p.controllerID, ImageDigest: digest}
	if _, err := p.topo.EnsureCacheVolume(ctx, name, id.ExternalsLabels(time.Now())); err != nil {
		return "", fmt.Errorf("failed to ensure externals volume %q: %w", name, err)
	}

	// Seed to completion BEFORE the runner mounts it read-only. Idempotent and
	// concurrency-safe via the in-volume flock + atomic marker: a warm volume is
	// a fast no-op, a fresh one is populated by exactly one seeder even under a
	// first-job race for the same digest.
	if err := p.seedExternals(ctx, runnerImage, name); err != nil {
		return "", err
	}
	return name, nil
}

// seedExternals runs the dedicated, short-lived externals SEED container to
// completion (ADR-003 F15): it populates the externals volume from the runner
// image's own externals payload under a lock, once. A non-zero exit or a run
// error fails the allocation (the guarded create rolls back the allocation's own
// resources; the externals volume itself is never rolled back — a cache).
//
// The seed container is uniquely named per invocation (a create-nonce suffix),
// so two concurrent provider processes seeding the same digest do not collide on
// the container NAME — the in-volume flock, not the name, is the seeding mutex.
func (p *Provider) seedExternals(ctx context.Context, runnerImage, volumeName string) error {
	nonce, err := newCreateNonce()
	if err != nil {
		return fmt.Errorf("failed to generate externals seed nonce: %w", err)
	}
	cfg, hostCfg := spec.BuildExternalsSeedContainer(spec.ExternalsSeedContainerSpec{
		Image:      runnerImage,
		VolumeName: volumeName,
		Labels:     p.helperLabels(),
	})
	code, err := p.runHelperContainer(ctx, cfg, hostCfg, "garm-seed-"+nonce, externalsSeedTimeout)
	if err != nil {
		return fmt.Errorf("externals seed for %q failed: %w", volumeName, err)
	}
	if code != 0 {
		return fmt.Errorf("externals seed for %q exited with code %d", volumeName, code)
	}
	return nil
}

// imageDigestHex resolves an image reference to its content-addressable digest
// hex (the image ID with the "sha256:" algorithm prefix stripped), the stable,
// Docker-name-safe token the externals volume is keyed on. Inspecting the local
// image works for BOTH a digest-pinned config reference and an unpinned
// dev/tag reference (the pinned digest is the manifest digest, not this content
// ID — using the content ID keys externals to the exact image bytes either way).
func (p *Provider) imageDigestHex(ctx context.Context, imageRef string) (string, error) {
	insp, _, err := p.cli.ImageInspectWithRaw(ctx, imageRef)
	if err != nil {
		return "", fmt.Errorf("failed to inspect runner image %q for its externals-cache digest: %w", imageRef, err)
	}
	id := insp.ID
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id = id[i+1:]
	}
	if id == "" {
		return "", fmt.Errorf("runner image %q has no content ID to key the externals cache on", imageRef)
	}
	return id, nil
}
