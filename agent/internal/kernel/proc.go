package kernel

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SmapsRollup holds the totals from /proc/PID/smaps_rollup, in bytes.
type SmapsRollup struct {
	Rss       uint64
	Pss       uint64
	PssAnon   uint64
	PssFile   uint64
	PssShmem  uint64
	Anonymous uint64
	Swap      uint64
	SwapPss   uint64
	// HasPssSplit is false on kernels before 5.8, which print Pss without the
	// Pss_Anon, Pss_File and Pss_Shmem breakdown.
	HasPssSplit bool
}

// ParseSmapsRollup parses /proc/PID/smaps_rollup. The first line names the
// address range and is ignored.
func ParseSmapsRollup(b []byte) (SmapsRollup, error) {
	var r SmapsRollup
	found := 0
	for line := range strings.Lines(string(b)) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok || strings.ContainsAny(key, " \t") {
			continue
		}
		n, err := parseSize(strings.Fields(rest))
		if err != nil {
			return SmapsRollup{}, fmt.Errorf("smaps_rollup %s: %w", key, err)
		}
		switch key {
		case "Rss":
			r.Rss = n
		case "Pss":
			r.Pss = n
		case "Pss_Anon":
			r.PssAnon, r.HasPssSplit = n, true
		case "Pss_File":
			r.PssFile = n
		case "Pss_Shmem":
			r.PssShmem = n
		case "Anonymous":
			r.Anonymous = n
		case "Swap":
			r.Swap = n
		case "SwapPss":
			r.SwapPss = n
		default:
			continue
		}
		found++
	}
	if found == 0 {
		return SmapsRollup{}, errors.New("smaps_rollup: no known fields")
	}
	return r, nil
}

// ProcStatus holds the fields Snugbox reads from /proc/PID/status. Sizes are in
// bytes.
type ProcStatus struct {
	Name     string
	Tgid     int
	PPid     int
	UID      int
	VmRSS    uint64
	RssAnon  uint64
	RssFile  uint64
	RssShmem uint64
	VmSwap   uint64
	// Threads is the process's thread count.
	Threads int
	// Kernel is true for kernel threads, which own no user memory and print no
	// VmRSS line.
	Kernel bool
}

// ParseProcStatus parses /proc/PID/status.
func ParseProcStatus(b []byte) (ProcStatus, error) {
	var st ProcStatus
	hasName, hasRSS := false, false
	for line := range strings.Lines(string(b)) {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val := strings.TrimSpace(rest)
		var err error
		switch key {
		case "Name":
			st.Name, hasName = val, true
		case "Tgid":
			st.Tgid, err = strconv.Atoi(val)
		case "PPid":
			st.PPid, err = strconv.Atoi(val)
		case "Uid":
			f := strings.Fields(val)
			if len(f) == 0 {
				return ProcStatus{}, errors.New("status Uid: empty")
			}
			st.UID, err = strconv.Atoi(f[0])
		case "VmRSS":
			st.VmRSS, err = parseSize(strings.Fields(val))
			hasRSS = true
		case "RssAnon":
			st.RssAnon, err = parseSize(strings.Fields(val))
		case "RssFile":
			st.RssFile, err = parseSize(strings.Fields(val))
		case "RssShmem":
			st.RssShmem, err = parseSize(strings.Fields(val))
		case "VmSwap":
			st.VmSwap, err = parseSize(strings.Fields(val))
		case "Threads":
			st.Threads, err = strconv.Atoi(val)
			if err == nil && st.Threads < 0 {
				err = errors.New("negative")
			}
		}
		if err != nil {
			return ProcStatus{}, fmt.Errorf("status %s: %w", key, err)
		}
	}
	if !hasName {
		return ProcStatus{}, errors.New("status: no Name line")
	}
	st.Kernel = !hasRSS
	return st, nil
}

// ParseProcStart returns a process's start time from /proc/PID/stat, in
// clock ticks after boot: field 22 of proc_pid_stat(5). The command name in
// field 2 may hold spaces and parentheses, so fields are counted from the
// last ")".
func ParseProcStart(b []byte) (uint64, error) {
	s := string(b)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return 0, errors.New("stat: no command name")
	}
	f := strings.Fields(s[end+1:])
	const startField = 22 - 3
	if len(f) <= startField {
		return 0, fmt.Errorf("stat: %d fields after the command name", len(f))
	}
	n, err := strconv.ParseUint(f[startField], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stat starttime: %w", err)
	}
	return n, nil
}

// ParseProcCgroup returns the cgroup v2 path from /proc/PID/cgroup: the line
// that starts with "0::". It returns false on a host that mounts cgroup v1
// only.
func ParseProcCgroup(b []byte) (string, bool) {
	for line := range strings.Lines(string(b)) {
		if path, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "0::"); ok && strings.HasPrefix(path, "/") {
			return path, true
		}
	}
	return "", false
}
