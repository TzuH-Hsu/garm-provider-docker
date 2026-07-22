package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Binary (1024-based) and decimal SI (1000-based) byte multipliers for
// ParseByteSize. GiB/MiB and GB/MB are kept deliberately DISTINCT — unlike
// github.com/docker/go-units' RAMInBytes (an indirect dependency already in
// go.mod via the moby SDK), which keys its unit table on the multiplier's
// first letter alone and so treats "8GB" and "8GiB" as the same 1024-based
// value. That conflation is a real, silent misparse for a decimal-suffixed
// operator input and was evaluated and rejected for this reason; see the
// WP1 report for the full comparison. A small local parser that treats
// GiB/MiB (binary) and GB/MB (decimal) as genuinely different units is
// worth the ~40 lines given operators write [resources] values by hand.
const (
	bytesPerKiB int64 = 1 << 10
	bytesPerMiB int64 = 1 << 20
	bytesPerGiB int64 = 1 << 30
	bytesPerKB  int64 = 1_000
	bytesPerMB  int64 = 1_000_000
	bytesPerGB  int64 = 1_000_000_000
)

// byteSizeUnits maps a lowercased unit suffix to its byte multiplier. A
// bare number (no suffix) or an explicit "b" suffix both mean plain bytes.
var byteSizeUnits = map[string]int64{
	"":   1,
	"b":  1,
	"kb": bytesPerKB,
	"mb": bytesPerMB,
	"gb": bytesPerGB,
	// The "ib" forms are the binary units; the "b" forms above are decimal SI.
	"kib": bytesPerKiB,
	"mib": bytesPerMiB,
	"gib": bytesPerGiB,
}

// ParseByteSize parses a human-readable byte size ("8GiB", "512MiB", "4GB",
// "1024", "1024B", ...) into a byte count (ADR-005's [resources] table:
// runner_memory, dind_memory). GiB/MiB/... are binary (1024-based); GB/MB
// are decimal SI (1000-based); a bare integer or one suffixed "B" is a
// plain byte count. Units are case-insensitive. Only non-negative integer
// magnitudes are accepted (no fractional sizes like "1.5GiB", no leading
// "+"/"-") — the two current callers (runner_memory/dind_memory) only ever
// need whole-byte precision, and rejecting fractional input outright is
// simpler than defining its rounding rule.
func ParseByteSize(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("byte size is empty")
	}

	i := 0
	for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("byte size %q must start with a digit", s)
	}
	numPart, unitPart := trimmed[:i], strings.ToLower(strings.TrimSpace(trimmed[i:]))

	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("byte size %q has an invalid numeric part: %w", s, err)
	}

	mult, ok := byteSizeUnits[unitPart]
	if !ok {
		return 0, fmt.Errorf("byte size %q has an unrecognized unit %q (want one of: B, KB, MB, GB, KiB, MiB, GiB, or no unit)", s, unitPart)
	}

	if n != 0 && mult != 0 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("byte size %q overflows int64", s)
	}
	return n * mult, nil
}

// ParsePositiveByteSize parses s exactly like ParseByteSize but additionally
// rejects a NON-POSITIVE result — any zero representation ("0", "00", "0GiB",
// "0 B") or, since ParseByteSize already refuses a leading "-", a negative
// magnitude. It exists for the extra_specs memory-override channel (ADR-005 H1):
// a pool's runner_memory/dind_memory override must be a genuine, positive limit,
// because Docker treats a memory limit of 0 as UNSET (unlimited), so accepting
// "0GiB" from a pool would SILENTLY REMOVE the operator's finite memory ceiling.
// The operator's own "no limit" intent is expressed by an EMPTY value, never a
// literal 0, so this stricter parse is safe for that channel while ParseByteSize
// keeps its 0-is-a-valid-count semantics for the operator-config tier.
func ParsePositiveByteSize(s string) (int64, error) {
	n, err := ParseByteSize(s)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("byte size %q must be a positive value (0 is not a valid limit — Docker treats a 0 memory limit as unset/unlimited, which would remove the operator's ceiling)", s)
	}
	return n, nil
}
