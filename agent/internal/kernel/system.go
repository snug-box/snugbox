package kernel

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Swap is one active swap area from /proc/swaps.
type Swap struct {
	Filename string
	Type     string
	Size     uint64
	Used     uint64
	Priority int
}

// ParseSwaps parses /proc/swaps. The first line is a header. The kernel escapes
// spaces, tabs, newlines and backslashes in file names as octal, which is
// undone here.
func ParseSwaps(b []byte) ([]Swap, error) {
	var out []Swap
	first := true
	for line := range strings.Lines(string(b)) {
		if first {
			first = false
			if strings.HasPrefix(line, "Filename") {
				continue
			}
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) != 5 {
			return nil, fmt.Errorf("swaps: %d fields in %q", len(f), strings.TrimSpace(line))
		}
		size, err := strconv.ParseUint(f[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("swaps size: %w", err)
		}
		used, err := strconv.ParseUint(f[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("swaps used: %w", err)
		}
		prio, err := strconv.Atoi(f[4])
		if err != nil {
			return nil, fmt.Errorf("swaps priority: %w", err)
		}
		if size, err = kibToBytes(size); err != nil {
			return nil, fmt.Errorf("swaps size: %w", err)
		}
		if used, err = kibToBytes(used); err != nil {
			return nil, fmt.Errorf("swaps used: %w", err)
		}
		out = append(out, Swap{Filename: unescapeOctal(f[0]), Type: f[1], Size: size, Used: used, Priority: prio})
	}
	return out, nil
}

// unescapeOctal turns the kernel's \ooo escapes back into bytes. A sequence
// that is not three octal digits is kept as written.
func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] <= '3' && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			sb.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

// Release is a kernel release string such as "6.8.0-45-generic".
type Release struct {
	Major, Minor, Patch int
	Raw                 string
}

// ParseRelease parses the output of uname -r or /proc/sys/kernel/osrelease.
// Only the leading numbers matter; distribution suffixes are kept in Raw.
func ParseRelease(s string) (Release, error) {
	s = strings.TrimSpace(s)
	r := Release{Raw: s}
	nums := s
	if i := strings.IndexFunc(s, func(c rune) bool { return (c < '0' || c > '9') && c != '.' }); i >= 0 {
		nums = s[:i]
	}
	parts := strings.Split(nums, ".")
	if len(parts) < 2 {
		return Release{}, fmt.Errorf("kernel release %q: want at least major.minor", s)
	}
	var err error
	if r.Major, err = strconv.Atoi(parts[0]); err != nil {
		return Release{}, fmt.Errorf("kernel release %q: %w", s, err)
	}
	if r.Minor, err = strconv.Atoi(parts[1]); err != nil {
		return Release{}, fmt.Errorf("kernel release %q: %w", s, err)
	}
	if len(parts) > 2 && parts[2] != "" {
		if r.Patch, err = strconv.Atoi(parts[2]); err != nil {
			return Release{}, fmt.Errorf("kernel release %q: %w", s, err)
		}
	}
	return r, nil
}

// AtLeast reports whether the release is major.minor or newer.
func (r Release) AtLeast(major, minor int) bool {
	if r.Major != major {
		return r.Major > major
	}
	return r.Minor >= minor
}

// String returns the release as major.minor, the form the reports use.
func (r Release) String() string { return fmt.Sprintf("%d.%d", r.Major, r.Minor) }

// ParseOSRelease parses /etc/os-release or /usr/lib/os-release as described in
// os-release(5): KEY=value lines, values optionally quoted, # for comments.
func ParseOSRelease(b []byte) map[string]string {
	out := make(map[string]string)
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = unquoteShell(v)
	}
	return out
}

func unquoteShell(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		q := v[0]
		v = v[1 : len(v)-1]
		if q == '"' {
			var sb strings.Builder
			for i := 0; i < len(v); i++ {
				if v[i] == '\\' && i+1 < len(v) && strings.IndexByte("\"\\$`", v[i+1]) >= 0 {
					i++
				}
				sb.WriteByte(v[i])
			}
			return sb.String()
		}
	}
	return v
}

// Mount is one line of /proc/PID/mountinfo.
type Mount struct {
	// Device is the major:minor number of the filesystem's device.
	Device     string
	MountPoint string
	FSType     string
	Source     string
	Options    string
}

// ParseMountinfo parses /proc/PID/mountinfo as described in proc_pid_mountinfo(5).
func ParseMountinfo(b []byte) ([]Mount, error) {
	var out []Mount
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if len(f) < 10 || sep < 0 || sep+2 >= len(f) {
			return nil, fmt.Errorf("mountinfo: malformed line %q", strings.TrimSpace(line))
		}
		out = append(out, Mount{
			Device:     f[2],
			MountPoint: unescapeOctal(f[4]),
			Options:    f[5],
			FSType:     f[sep+1],
			Source:     unescapeOctal(f[sep+2]),
		})
	}
	return out, nil
}

// CPUTimes are the aggregate CPU counters from the first line of /proc/stat, in
// clock ticks (USER_HZ).
type CPUTimes struct {
	User, Nice, System, Idle, IOWait, IRQ, SoftIRQ, Steal uint64
}

// Busy returns the ticks spent doing work, including time stolen by the
// hypervisor, which is CPU the box asked for and did not get.
func (c CPUTimes) Busy() uint64 {
	return c.User + c.Nice + c.System + c.IRQ + c.SoftIRQ + c.Steal
}

// Total returns all ticks, busy and idle.
func (c CPUTimes) Total() uint64 { return c.Busy() + c.Idle + c.IOWait }

// ProcStat holds what Snugbox reads from /proc/stat.
type ProcStat struct {
	CPU      CPUTimes
	BootTime time.Time
}

// ParseProcStat parses the "cpu" and "btime" lines of /proc/stat.
func ParseProcStat(b []byte) (ProcStat, error) {
	var ps ProcStat
	hasCPU, hasBoot := false, false
	for line := range strings.Lines(string(b)) {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "cpu":
			if len(f) < 9 {
				return ProcStat{}, fmt.Errorf("stat cpu line: %d fields", len(f))
			}
			var v [8]uint64
			for i := range v {
				n, err := strconv.ParseUint(f[i+1], 10, 64)
				if err != nil {
					return ProcStat{}, fmt.Errorf("stat cpu field %d: %w", i+1, err)
				}
				v[i] = n
			}
			ps.CPU = CPUTimes{User: v[0], Nice: v[1], System: v[2], Idle: v[3], IOWait: v[4], IRQ: v[5], SoftIRQ: v[6], Steal: v[7]}
			hasCPU = true
		case "btime":
			if len(f) != 2 {
				return ProcStat{}, errors.New("stat btime: malformed")
			}
			sec, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return ProcStat{}, fmt.Errorf("stat btime: %w", err)
			}
			ps.BootTime, hasBoot = time.Unix(sec, 0).UTC(), true
		}
	}
	if !hasCPU || !hasBoot {
		return ProcStat{}, errors.New("stat: missing cpu or btime line")
	}
	return ps, nil
}

// NetCounters are the byte counters of one interface in /proc/net/dev.
type NetCounters struct {
	RxBytes, TxBytes uint64
}

// ParseNetDev parses /proc/net/dev. The two header lines are skipped.
func ParseNetDev(b []byte) (map[string]NetCounters, error) {
	out := make(map[string]NetCounters)
	for line := range strings.Lines(string(b)) {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) != 16 {
			return nil, fmt.Errorf("net/dev %s: %d counters", strings.TrimSpace(name), len(f))
		}
		rx, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("net/dev rx: %w", err)
		}
		tx, err := strconv.ParseUint(f[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("net/dev tx: %w", err)
		}
		out[strings.TrimSpace(name)] = NetCounters{RxBytes: rx, TxBytes: tx}
	}
	return out, nil
}

// maxCPUID is far above any CPU number Linux supports, since NR_CPUS tops out
// at 8192, and keeps a corrupt list from overflowing the count.
const maxCPUID = 1 << 16

// ParseCPUList counts the CPUs in a list such as "0-3,6,8-9", the format of
// /sys/devices/system/cpu/online.
func ParseCPUList(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("cpu list: empty")
	}
	n := 0
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil || a < 0 || a > maxCPUID {
			return 0, fmt.Errorf("cpu list %q: bad entry %q", s, part)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a || b > maxCPUID {
				return 0, fmt.Errorf("cpu list %q: bad range %q", s, part)
			}
		}
		n += b - a + 1
		if n > maxCPUID {
			return 0, fmt.Errorf("cpu list %q: more CPUs than Linux supports", s)
		}
	}
	return n, nil
}
