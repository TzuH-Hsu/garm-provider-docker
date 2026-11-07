package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/TzuH-Hsu/garm-provider-docker/internal/spec"
)

// externalsSeedTimeout bounds the one-time seed copy of thousands of files, which can
// take minutes on a slow filesystem. On a warm volume the seed is a quick no-op.
const externalsSeedTimeout = 10 * time.Minute

// planExternals ensures and seeds the externals volume for runnerImage's content ID,
// and returns its name and labels. It blocks until seeding completes, so the runner
// never mounts a half-populated tree. runnerImage must be present locally. Every entity
// scope shares this volume: it carries no repository data, and the read-only mount and
// provider-only seeding make that safe (ADR-003, Externals).
func (p *Provider) planExternals(ctx context.Context, runnerImage string) (string, map[string]string, error) {
	digest, err := p.imageDigestHex(ctx, runnerImage)
	if err != nil {
		return "", nil, err
	}

	name := spec.ExternalsVolumeName(digest)
	if err := spec.ValidateDerivedName("externals cache volume", name); err != nil {
		return "", nil, err
	}

	id := spec.ExternalsVolumeIdentity{ControllerID: p.controllerID, ImageDigest: digest}
	wantLabels := id.ExternalsLabels(time.Now())
	res, err := p.topo.EnsureCacheVolume(ctx, name, wantLabels)
	if err != nil {
		return "", nil, fmt.Errorf("failed to ensure externals volume %q: %w", name, err)
	}
	// The resolved name may be an alternate, when something that is not our cache holds
	// the preferred one. Seeding the preferred name would pin that volume instead.
	name = res.Name

	// A nil error proves the atomic marker exists: the script exits 0 only after
	// writing it.
	if err := p.seedExternals(ctx, runnerImage, name, wantLabels); err != nil {
		return "", nil, err
	}
	return name, wantLabels, nil
}

// seedExternals runs the seed container to completion. A run error or a non-zero exit
// fails the allocation. Each run has a unique name, because the in-volume flock, not
// the container name, is the seeding mutex.
func (p *Provider) seedExternals(ctx context.Context, runnerImage, volumeName string, wantLabels map[string]string) error {
	nonce, err := newCreateNonce()
	if err != nil {
		return fmt.Errorf("failed to generate externals seed nonce: %w", err)
	}
	cfg, hostCfg := spec.BuildExternalsSeedContainer(spec.ExternalsSeedContainerSpec{
		Image:      runnerImage,
		VolumeName: volumeName,
		Labels:     p.helperLabels(),
	})

	// Creating the helper pins the volume. If it was removed since the ensure, the
	// daemon has recreated it empty and unlabelled, so check before copying into it. A
	// volume that fails the check is left in place, and GARM's retry routes around it
	// (ADR-003).
	afterCreate := func(vctx context.Context) error {
		v, err := p.cli.VolumeInspect(vctx, volumeName)
		if err != nil {
			return fmt.Errorf("externals seed target %q could not be re-inspected before seeding: %w", volumeName, err)
		}
		if verr := spec.ValidateAdoptedCacheVolume(volumeName, v.Labels, wantLabels); verr != nil {
			slog.WarnContext(vctx, "externals seed target is not our validated cache; leaving it in place as cruft and aborting this seed so GARM retries and reconciles around the slot",
				"resource", "cache-volume", "volume", volumeName, "error", verr)
			return fmt.Errorf("externals seed target %q is not our seeded cache (a manual-purge/create race auto-created it): %w", volumeName, verr)
		}
		return nil
	}

	code, err := p.runHelperContainer(ctx, cfg, hostCfg, "garm-seed-"+nonce, externalsSeedTimeout, afterCreate)
	if err != nil {
		return fmt.Errorf("externals seed for %q failed: %w", volumeName, err)
	}
	if code != 0 {
		return fmt.Errorf("externals seed for %q exited with code %d", volumeName, code)
	}
	return nil
}

// imageDigestHex returns imageRef's image ID without its "sha256:" prefix. Unlike a
// manifest digest, the content ID exists for both digest-pinned and tag references.
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
