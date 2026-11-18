package kernel

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// PressureLine is one line of a pressure stall information (PSI) file.
type PressureLine struct {
	// Avg10, Avg60 and Avg300 are the share of wall time, in percent, that
	// tasks stalled over the last 10, 60 and 300 seconds.
	Avg10, Avg60, Avg300 float64
	// Total is the stall time accumulated since the counter started: boot for
	// /proc/pressure/memory, the cgroup's creation for memory.pressure.
	Total time.Duration
}

// Pressure is the content of /proc/pressure/memory or a cgroup's
// memory.pressure. "Some" counts time when at least one task stalled on
// memory; "Full" counts time when every non-idle task did.
type Pressure struct {
	Some    PressureLine
	Full    PressureLine
	HasFull bool
}

// ParsePressure parses a PSI file. Linux 4.20 added these files; a file without
// a "some" line is an error.
func ParsePressure(b []byte) (Pressure, error) {
	var p Pressure
	hasSome := false
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		pl, err := parsePressureFields(f[1:])
		if err != nil {
			return Pressure{}, fmt.Errorf("pressure %q line: %w", f[0], err)
		}
		switch f[0] {
		case "some":
			p.Some, hasSome = pl, true
		case "full":
			p.Full, p.HasFull = pl, true
		}
	}
	if !hasSome {
		return Pressure{}, errors.New(`pressure: no "some" line`)
	}
	return p, nil
}

func parsePressureFields(fields []string) (PressureLine, error) {
	var pl PressureLine
	const want = 4
	seen := 0
	for _, kv := range fields {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return PressureLine{}, fmt.Errorf("field %q has no value", kv)
		}
		switch k {
		case "avg10", "avg60", "avg300":
			x, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(x) || x < 0 || x > 100 {
				return PressureLine{}, fmt.Errorf("%s=%q is not a percentage", k, v)
			}
			switch k {
			case "avg10":
				pl.Avg10 = x
			case "avg60":
				pl.Avg60 = x
			default:
				pl.Avg300 = x
			}
			seen++
		case "total":
			us, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return PressureLine{}, fmt.Errorf("total=%q: %w", v, err)
			}
			if us > math.MaxInt64/uint64(time.Microsecond) {
				return PressureLine{}, fmt.Errorf("total=%q: %w", v, errOverflow)
			}
			pl.Total = time.Duration(us) * time.Microsecond
			seen++
		}
	}
	if seen != want {
		return PressureLine{}, fmt.Errorf("want avg10, avg60, avg300 and total, found %d of them", seen)
	}
	return pl, nil
}
