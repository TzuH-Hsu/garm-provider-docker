package spec

import (
	"strconv"
	"time"
)

// CacheGCConfig is the current config EvaluateCacheEviction compares a volume with.
type CacheGCConfig struct {
	// Generation and PnpmMajor are the current salts; another salt is superseded.
	Generation string
	PnpmMajor  string

	// ImageDigest is the current runner image's digest, and an externals volume with
	// another is superseded. It is empty when the provider could not resolve it this
	// pass, which skips that check until a later pass.
	ImageDigest string

	// StaleDays flags a volume older than this many days; <= 0 disables that.
	StaleDays int

	// Grace keeps a superseded volume from being flagged until this old, so an in-flight
	// older job that just created it is not flagged. Age-based flagging ignores it.
	Grace time.Duration
}

// EvaluateCacheEviction reports whether the GC should flag this cache volume as stale,
// and why. Nothing is removed: the caller logs flagged volumes for the operator to
// prune (ADR-003, "Garbage collection"). Past cfg.Grace, a volume is flagged if its salt
// is superseded, or if it is a toolcache or pnpm volume with no os-release label, which
// no allocation mounts. Past cfg.StaleDays since creation, any volume is flagged; labels
// cannot be re-stamped on a hit, so an active cache ages too. A label set whose
// last-used is missing or malformed, or that is not a cache's, is kept.
func EvaluateCacheEviction(labels map[string]string, cfg CacheGCConfig, now time.Time) (bool, string) {
	if labels[LabelCache] != "true" {
		return false, "" // not a cache volume
	}

	created, ok := parseLastUsed(labels)
	if !ok {
		return false, "" // cannot be aged, so keep it
	}
	age := now.Sub(created)

	// Supersession (past the grace window).
	if age >= cfg.Grace {
		switch CacheKind(labels[LabelCacheKind]) {
		case CacheKindToolcache:
			if g := labels[LabelGeneration]; g != "" && g != cfg.Generation {
				return true, "superseded toolcache generation " + g + " (current " + cfg.Generation + ")"
			}
			if labels[LabelOSRelease] == "" {
				return true, preOSKeyReason
			}
		case CacheKindPnpm:
			if m := labels[LabelPnpmMajor]; m != "" && m != cfg.PnpmMajor {
				return true, "superseded pnpm-major " + m + " (current " + cfg.PnpmMajor + ")"
			}
			if labels[LabelOSRelease] == "" {
				return true, preOSKeyReason
			}
		case CacheKindExternals:
			if d := labels[LabelImageDigest]; cfg.ImageDigest != "" && d != "" && d != cfg.ImageDigest {
				return true, "superseded externals image-digest " + shortDigest(d) + " (current " + shortDigest(cfg.ImageDigest) + ")"
			}
		}
	}

	// Age-since-creation backstop.
	if cfg.StaleDays > 0 {
		if age >= time.Duration(cfg.StaleDays)*24*time.Hour {
			return true, "aged out (created " + created.UTC().Format(time.RFC3339) + ", stale after " + strconv.Itoa(cfg.StaleDays) + "d)"
		}
	}

	return false, ""
}

// preOSKeyReason is for a toolcache or pnpm volume with no OS key in its name, which
// the provider never resolves to.
const preOSKeyReason = "superseded: created before toolcache/pnpm volumes were keyed on the OS release, no longer mounted"

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

// shortDigest is for the log message only; the decision compares full labels.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
