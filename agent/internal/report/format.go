// Package report turns an audit result into the terminal report, the
// Markdown report, the versioned JSON document and the redacted share card.
// The section order is blueprint section 8.2: the first three lines of the
// summary answer the question on their own.
package report

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

// GiB formats bytes as GiB with up to two decimals and trailing zeros
// trimmed: "3.38 GiB", "4 GiB", "0.5 GiB".
func GiB(b uint64) string { return trimmed(float64(b)/float64(model.GiB), 2) + " GiB" }

// MiB formats bytes as MiB with thousands separators: "1,240 MiB". Below
// 10 MiB it keeps one decimal, so a small workload doesn't read as zero.
func MiB(b uint64) string {
	v := float64(b) / float64(model.MiB)
	if v < 10 && b > 0 {
		return trimmed(v, 1) + " MiB"
	}
	return thousands(uint64(math.Round(v))) + " MiB"
}

// Size picks GiB for a gigabyte and above, MiB below it.
func Size(b uint64) string {
	if b >= model.GiB {
		return GiB(b)
	}
	return MiB(b)
}

// GiBNumber formats bytes as a bare GiB figure for sums such as
// "2.6 need + 0.48 headroom".
func GiBNumber(b uint64) string { return trimmed(float64(b)/float64(model.GiB), 2) }

func trimmed(v float64, decimals int) string {
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func thousands(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Span describes a duration the way the report says it: "60 seconds",
// "12 minutes", "5 hours", "14 days".
func Span(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return plural(int(math.Round(d.Seconds())), "second")
	case d < 2*time.Hour:
		return plural(int(math.Round(d.Minutes())), "minute")
	case d < 48*time.Hour:
		return plural(int(math.Round(d.Hours())), "hour")
	default:
		return plural(int(math.Round(d.Hours()/24)), "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// Stamp formats a time in the box's local zone with its offset, as blueprint
// section 8.1 asks: "Sat 31 Oct 2026 09:00 +05:30".
func Stamp(t time.Time) string { return t.Format("Mon 2 Jan 2006 15:04 -07:00") }

// Percent formats a share from 0 to 1 as a percentage with sensible
// precision: "0.04%", "1.6%", "12%".
func Percent(share float64) string {
	v := share * 100
	switch {
	case v < 0.1:
		return trimmed(v, 2) + "%"
	case v < 10:
		return trimmed(v, 1) + "%"
	default:
		return strconv.Itoa(int(math.Round(v))) + "%"
	}
}

// List joins names as prose: "a", "a and b", "a, b and c".
func List(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// Count writes "1 kill" or "3 kills".
func Count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
