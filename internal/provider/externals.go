package provider

import (
	"context"
	"fmt"
	"log"
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
	// Use the name EnsureCacheVolume actually resolved to — which may be a
	// reconciled ALTERNATE when the preferred deterministic slot was occupied by a
	// foreign/unlabeled squatter (ADR-003 label-as-identity). Seeding and mounting
	// the preferred slot instead would pin the squatter and abort; the reconciled
	// volume is the one we created labeled and must seed+mount.
	name = res.Name

	// Seed to completion BEFORE the runner mounts it read-only. Idempotent and
	// concurrency-safe via the in-volume flock + atomic marker: a warm volume is
	// a fast no-op, a fresh one is populated by exactly one seeder even under a
	// first-job race for the same digest. A SUCCESSFUL seed proves the atomic
	// `.garm-seeded` marker is present (the script exits 0 only after the marker
	// exists), so a runner started after this returns can never mount a
	// half-populated externals tree (H3b).
	if err := p.seedExternals(ctx, runnerImage, name, wantLabels); err != nil {
		return "", nil, err
	}
	return name, wantLabels, nil
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
//
// Pin-then-validate (ADR-003 W2 structural redesign): the seed helper's
// ContainerCreate PINS the externals volume; if a concurrent GC evicted it in the
// ensure→seed window, real Moby AUTO-CREATES it UNLABELED and the seeder would
// otherwise copy ~380MB into a volume the runner should not mount. An afterCreate
// hook re-inspects the pinned volume and, only if a SUCCESSFUL inspect PROVES it is
// no longer our seeded cache, aborts the seed BEFORE the copy — WITHOUT deleting
// the mismatched volume (a Moby auto-created unlabeled volume and a foreign one are
// indistinguishable, so name-based deletion can never be foreign-safe — B2). The
// unprovable volume is left as cruft (logged); GARM's retry reconciles AROUND the
// slot (EnsureCacheVolume label-as-identity), so there is no wedge. An inspect
// FAILURE also aborts closed WITHOUT any delete ("don't know → don't delete").
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

	afterCreate := func(vctx context.Context) error {
		v, err := p.cli.VolumeInspect(vctx, volumeName)
		if err != nil {
			// inspect failed → don't know → don't delete, fail closed.
			return fmt.Errorf("externals seed target %q could not be re-inspected before seeding: %w", volumeName, err)
		}
		if verr := spec.ValidateAdoptedCacheVolume(volumeName, v.Labels, wantLabels); verr != nil {
			// A SUCCESSFUL inspect proves this is an unlabeled auto-created
			// replacement (or a foreign squatter). Do NOT delete it (never delete an
			// unprovable volume — B2): leave it as cruft (logged) and abort; the
			// retry reconciles around the slot.
			log.Printf("garm-provider-docker: externals seed target %q is not our validated cache (%v); leaving it in place as cruft (ADR-003 never-delete-unprovable) and aborting this seed so GARM retries and reconciles around the slot", volumeName, verr)
			return fmt.Errorf("externals seed target %q is not our seeded cache (a GC/create race auto-created it): %w", volumeName, verr)
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
