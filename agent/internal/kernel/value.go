package kernel

import (
	"fmt"
	"strconv"
	"strings"
)

// Limit is a cgroup memory limit such as memory.max, memory.high or
// memory.swap.max. The kernel prints "max" for no limit.
type Limit struct {
	Bytes     uint64
	Unlimited bool
}

// ParseLimit parses a single-value limit file.
func ParseLimit(b []byte) (Limit, error) {
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return Limit{Unlimited: true}, nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return Limit{}, fmt.Errorf("limit %q: %w", s, err)
	}
	return Limit{Bytes: n}, nil
}

// ParseUint parses a file that holds one unsigned decimal number, such as
// memory.current or memory.peak.
func ParseUint(b []byte) (uint64, error) {
	s := strings.TrimSpace(string(b))
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value %q: %w", s, err)
	}
	return n, nil
}

// ParseChoice returns the selected word of a sysfs choice file such as
// /sys/kernel/mm/transparent_hugepage/enabled, where the kernel prints every
// option and puts the active one in brackets: "always [madvise] never".
func ParseChoice(b []byte) (string, error) {
	for _, w := range strings.Fields(string(b)) {
		if len(w) > 2 && w[0] == '[' && w[len(w)-1] == ']' {
			return w[1 : len(w)-1], nil
		}
	}
	return "", fmt.Errorf("choice %q: no option in brackets", strings.TrimSpace(string(b)))
}
