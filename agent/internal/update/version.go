package update

import (
	"strconv"
	"strings"
)

// Newer reports whether release version a is newer than b, by semantic
// versioning: major, minor and patch compared as numbers, and a version with
// a pre-release, such as 1.2.0-rc.1, older than the same one without. It
// reports false when either is not a version.
func Newer(a, b string) bool {
	va, ok := parseVersion(a)
	if !ok {
		return false
	}
	vb, ok := parseVersion(b)
	if !ok {
		return false
	}
	for i := range 3 {
		if va.core[i] != vb.core[i] {
			return va.core[i] > vb.core[i]
		}
	}
	return comparePre(va.pre, vb.pre) > 0
}

type semver struct {
	core [3]uint64
	pre  []string
}

// ValidVersion reports whether v is a release version, with no leading v:
// such as 1.2.3, or 1.2.3-rc.1 for a pre-release.
func ValidVersion(v string) bool {
	_, ok := parseVersion(v)
	return ok
}

func parseVersion(v string) (semver, bool) {
	var s semver
	v, _, _ = strings.Cut(v, "+")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return s, false
	}
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || (len(p) > 1 && p[0] == '0') {
			return s, false
		}
		s.core[i] = n
	}
	if hasPre {
		if pre == "" {
			return s, false
		}
		s.pre = strings.Split(pre, ".")
		for _, id := range s.pre {
			if id == "" {
				return s, false
			}
		}
	}
	return s, true
}

// comparePre orders pre-release identifiers as semantic versioning does: no
// pre-release is higher than any; numbers compare as numbers and below
// words; a longer list wins a tie.
func comparePre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1
	case len(b) == 0:
		return -1
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		na, errA := strconv.ParseUint(a[i], 10, 64)
		nb, errB := strconv.ParseUint(b[i], 10, 64)
		switch {
		case errA == nil && errB == nil:
			if na != nb {
				if na > nb {
					return 1
				}
				return -1
			}
		case errA == nil:
			return -1
		case errB == nil:
			return 1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(a) > len(b):
		return 1
	case len(a) < len(b):
		return -1
	}
	return 0
}
