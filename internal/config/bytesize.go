package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Binary (KiB, MiB, GiB) and decimal (KB, MB, GB) multipliers. They stay
// distinct so that "8GB" is never read as "8GiB".
const (
	bytesPerKiB int64 = 1 << 10
	bytesPerMiB int64 = 1 << 20
	bytesPerGiB int64 = 1 << 30
	bytesPerKB  int64 = 1_000
	bytesPerMB  int64 = 1_000_000
	bytesPerGB  int64 = 1_000_000_000
)

// byteSizeUnits maps a lowercased unit suffix to its multiplier. No suffix means
// bytes.
var byteSizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"kb":  bytesPerKB,
	"mb":  bytesPerMB,
	"gb":  bytesPerGB,
	"kib": bytesPerKiB,
	"mib": bytesPerMiB,
	"gib": bytesPerGiB,
}

// ParseByteSize parses a size such as "8GiB", "512MiB", "4GB", "1024" or
// "1024B" into bytes. KiB, MiB and GiB are binary and KB, MB and GB decimal;
// units are case-insensitive. Only non-negative whole numbers are accepted, so
// "1.5GiB" and a leading sign are errors.
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

// ParsePositiveByteSize is ParseByteSize but also rejects zero in any spelling.
// It is for a pool's memory override: Docker treats a zero memory limit as
// unlimited, so accepting "0GiB" would remove the operator's ceiling. The
// operator's own "no limit" is an empty value, never a literal 0.
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
