// Package bytesize parses free-space/capacity thresholds shared by
// internal/config (validating [disk] and cache-volume budget settings) and
// internal/diskguard (comparing them against live statfs results). It is a
// leaf package — it imports nothing internal — specifically so both of
// those packages can import it without an import cycle: internal/diskguard
// transitively imports internal/config (diskguard -> cachestore -> provider
// -> config), so internal/config cannot import internal/diskguard itself.
package bytesize

import (
	"fmt"
	"strconv"
	"strings"
)

// Threshold is a free-space floor, expressed as either a percentage of the
// filesystem's total capacity or an absolute byte count. ParseThreshold
// only ever sets one of the two fields — BytesOf relies on that to decide
// which one applies.
type Threshold struct {
	Percent float64
	Bytes   uint64
}

// byteUnits maps a size suffix to its binary (1024-based) byte multiplier.
// Ordered longest-suffix-first below so "GB" is tried before the bare "B"
// suffix it would otherwise also match.
var byteUnits = []struct {
	suffix     string
	multiplier uint64
}{
	{"TB", 1024 * 1024 * 1024 * 1024},
	{"GB", 1024 * 1024 * 1024},
	{"MB", 1024 * 1024},
	{"KB", 1024},
	{"B", 1},
}

// ParseThreshold parses a free-space threshold: a percentage of total
// capacity like "10%" (0-100 inclusive), or an absolute size like "20GB" or
// "512MB" (binary units, case-insensitive: B/KB/MB/GB/TB).
func ParseThreshold(s string) (Threshold, error) {
	if rest, ok := strings.CutSuffix(s, "%"); ok {
		pct, err := strconv.ParseFloat(rest, 64)
		// Written as the negation of the in-range condition, not as
		// `pct < 0 || pct > 100`: every ordered comparison with NaN is
		// false, so that direct form would let ParseFloat's accepted
		// "NaN"/"Inf" spellings silently produce a threshold requiring 0
		// bytes free — the guard would then never fire for a malformed
		// config value instead of failing loudly at startup.
		if err != nil || !(pct >= 0 && pct <= 100) {
			return Threshold{}, invalidThresholdError(s)
		}
		return Threshold{Percent: pct}, nil
	}

	upper := strings.ToUpper(s)
	for _, u := range byteUnits {
		numPart, ok := strings.CutSuffix(upper, u.suffix)
		if !ok || numPart == "" {
			continue
		}
		n, err := strconv.ParseUint(numPart, 10, 64)
		if err != nil {
			continue
		}
		return Threshold{Bytes: n * u.multiplier}, nil
	}

	return Threshold{}, invalidThresholdError(s)
}

// invalidThresholdError reports the legal formats so a misconfigured value
// is actionable without reading source.
func invalidThresholdError(s string) error {
	return fmt.Errorf("invalid threshold %q: want a percentage like \"10%%\" or a size like \"20GB\"", s)
}

// BytesOf resolves this threshold against a filesystem's total capacity. An
// absolute threshold (Bytes != 0) ignores totalBytes entirely; otherwise
// the percentage is scaled against it.
func (t Threshold) BytesOf(totalBytes uint64) uint64 {
	if t.Bytes != 0 {
		return t.Bytes
	}
	return uint64(t.Percent / 100 * float64(totalBytes))
}
