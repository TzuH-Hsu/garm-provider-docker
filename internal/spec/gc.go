package spec

import (
	"time"
)

// This file holds ADR-003's opportunistic cache-GC eviction DECISION (M2-W2) as
// a pure function over a cache volume's labels plus the current config snapshot
// and the wall clock. Keeping the decision pure — no Docker calls, the caller
// supplies `now` — makes every eviction case (generation/pnpm-major/image-digest
// supersession, age-since-creation, and the conservative keeps) cheap to
// table-test; the topology layer does the label-scoped listing and the actual
// removal, and re-asserts these same labels are a cache volume before removing.

// CacheGCConfig is the current-config snapshot EvaluateCacheEviction compares a
// cache volume against. The provider builds it from [cache] plus the resolved
// runner-image digest.
type CacheGCConfig struct {
	// Generation / PnpmMajor are the CURRENT toolcache generation and pnpm major
	// salts ([cache].generation / pnpm_major). A cache volume whose own salt
	// label differs is superseded.
	Generation string
	PnpmMajor  string

	// ImageDigest is the CURRENT runner image's digest (image ID hex). An
	// externals volume whose image-digest label differs is superseded. It is
	// EMPTY when the provider could not resolve the current digest this pass
	// (e.g. the runner image is not present locally during a ListInstances GC),
	// in which case externals supersession is skipped this pass (the volume is
	// still subject to age eviction) and caught on a later pass — self-healing.
	ImageDigest string

	// StaleDays is [cache].stale_cache_eviction_days. A cache volume whose
	// CREATION-time last-used label is older than this many days is evicted
	// regardless of salt (the coarse, self-healing age backstop). Zero or
	// negative disables age eviction entirely.
	StaleDays int

	// Grace protects a very-recently-created SUPERSEDED volume from eviction, so
	// an in-flight older-generation/older-image job that just created its cache
	// (but has not yet started the runner that would in-use-pin it) is not
	// reaped out from under it. Measured from the volume's creation-time
	// last-used label. Age eviction does not use the grace (an aged-out volume
	// is old by definition).
	Grace time.Duration
}

// EvaluateCacheEviction decides whether the cache volume with these labels
// should be evicted by the opportunistic GC (ADR-003 W2), given the current
// config snapshot and wall clock. It returns (evict, reason); reason is a short
// human string for the eviction log and is "" when the volume is kept.
//
// The eviction policy, given that the last-used label is IMMUTABLE and records
// CREATION time (not reuse — W1's real-daemon finding: a local volume's labels
// cannot be re-stamped), is:
//
//   - SUPERSESSION: a toolcache volume whose generation != the current
//     generation, a pnpm volume whose pnpm-major != current, or an externals
//     volume whose image-digest != the current runner image's digest — is
//     evicted once it is older than Grace (so an in-flight older job is not
//     killed). diag-logs volumes have no salt and are never superseded.
//   - AGE: any cache volume whose creation-time last-used is older than
//     StaleDays is evicted regardless of salt. This is the deliberately COARSE,
//     self-healing backstop (ADR-003 amendment): because last-used cannot be
//     re-stamped on a hit, even a still-warm cache is aged from its creation, so
//     an active repo's cache is periodically evicted and simply re-created empty
//     on the next job. Chosen over sentinel-mtime LRU for robustness on Docker
//     Desktop, where the provider cannot stat a volume's filesystem from outside
//     the VM (documented in ADR-003).
//
// A volume whose last-used is missing or unparseable cannot be aged, so it is
// conservatively KEPT (never evict something we cannot place in time). A
// non-cache label set (a caller mistake, since topology only lists cache
// volumes) is also kept — defense-in-depth against ever reaping a job-scoped or
// foreign resource here.
func EvaluateCacheEviction(labels map[string]string, cfg CacheGCConfig, now time.Time) (bool, string) {
	if labels[LabelCache] != "true" {
		return false, "" // not a cache volume — never touched here
	}

	created, ok := parseLastUsed(labels)
	if !ok {
		return false, "" // cannot age → conservatively keep
	}
	age := now.Sub(created)

	// Supersession (past the grace window).
	if age >= cfg.Grace {
		switch CacheKind(labels[LabelCacheKind]) {
		case CacheKindToolcache:
			if g := labels[LabelGeneration]; g != "" && g != cfg.Generation {
				return true, "superseded toolcache generation " + g + " (current " + cfg.Generation + ")"
			}
		case CacheKindPnpm:
			if m := labels[LabelPnpmMajor]; m != "" && m != cfg.PnpmMajor {
				return true, "superseded pnpm-major " + m + " (current " + cfg.PnpmMajor + ")"
			}
		case CacheKindExternals:
			// Skip when the current digest is unknown this pass (cfg.ImageDigest
			// == ""): the volume is still subject to age eviction below.
			if d := labels[LabelImageDigest]; cfg.ImageDigest != "" && d != "" && d != cfg.ImageDigest {
				return true, "superseded externals image-digest " + shortDigest(d) + " (current " + shortDigest(cfg.ImageDigest) + ")"
			}
		}
	}

	// Age-since-creation backstop.
	if cfg.StaleDays > 0 {
		if age >= time.Duration(cfg.StaleDays)*24*time.Hour {
			return true, "aged out (created " + created.UTC().Format(time.RFC3339) + ", stale after " + itoa(cfg.StaleDays) + "d)"
		}
	}

	return false, ""
}

// parseLastUsed reads the garm.docker/last-used label (RFC3339, the creation
// instant a cache volume's builder stamps) into a time. The bool reports whether
// the label was present and well-formed.
func parseLastUsed(labels map[string]string) (time.Time, bool) {
	raw := labels[LabelLastUsed]
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// shortDigest trims a long image digest to its first 12 chars for a readable
// eviction-log message. It never affects the eviction decision (that compares
// full labels), only the human string.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// itoa is a tiny local int→string for the eviction reason, avoiding an fmt
// import in this otherwise allocation-light pure file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
