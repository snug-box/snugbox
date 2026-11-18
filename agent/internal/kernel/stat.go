package kernel

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

var (
	errMalformed = errors.New("malformed line")
	errOverflow  = errors.New("value overflows 64 bits")
)

// Stat holds the counters of a flat-keyed file such as memory.stat,
// memory.events or /proc/vmstat, or the sizes in /proc/meminfo.
type Stat map[string]uint64

// Get returns the value printed for key, or zero when the kernel did not print
// it. Older kernels lack some fields, so a missing field reads as zero and the
// caller uses Has where the difference matters.
func (s Stat) Get(key string) uint64 { return s[key] }

// Has reports whether the kernel printed key.
func (s Stat) Has(key string) bool {
	_, ok := s[key]
	return ok
}

// ParseStat parses a flat-keyed file, where each line holds a key and an
// unsigned decimal number. Lines in any other shape are skipped and counted in
// the second result, so one odd line does not cost the whole sample.
func ParseStat(b []byte) (Stat, int) {
	s := make(Stat, 72)
	skipped := 0
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 2 {
			skipped++
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			skipped++
			continue
		}
		s[f[0]] = n
	}
	return s, skipped
}

// ParseMeminfo parses /proc/meminfo. Lines printed in kB are converted to
// bytes. The few lines without a unit, such as HugePages_Total, are counts and
// are kept as printed.
func ParseMeminfo(b []byte) (Stat, int) {
	s := make(Stat, 64)
	skipped := 0
	for line := range strings.Lines(string(b)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			skipped++
			continue
		}
		n, err := parseSize(strings.Fields(rest))
		if err != nil {
			skipped++
			continue
		}
		s[key] = n
	}
	return s, skipped
}

// parseSize reads "1234 kB" as bytes and a bare "1234" as a count.
func parseSize(f []string) (uint64, error) {
	switch {
	case len(f) == 1:
		return strconv.ParseUint(f[0], 10, 64)
	case len(f) == 2 && f[1] == "kB":
		n, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kibToBytes(n)
	default:
		return 0, errMalformed
	}
}

func kibToBytes(n uint64) (uint64, error) {
	if n > math.MaxUint64/1024 {
		return 0, errOverflow
	}
	return n * 1024, nil
}
