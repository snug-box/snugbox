//go:build linux

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// madvCold is MADV_COLD, from Linux 5.4, which package syscall lacks.
const madvCold = 20

// adviseRandomReads tells the kernel not to read ahead in snugboxd's own
// binary. The watcher runs a few MiB scattered through 14 MiB of code and
// tables. On each page fault the kernel's default read-around brings in the
// 128 KiB around the page, and once memory pressure has the watcher read its
// binary back in, those pages are charged to it and count in its working set
// (blueprint section 9.4). With MADV_RANDOM only the pages it uses are read.
func adviseRandomReads() error { return adviseOwnBinary(syscall.MADV_RANDOM) }

// adviseCold tells the kernel that snugboxd's own binary pages are cold. The
// code that ran at start was read in with read-around, before main could turn
// it off, and the multi-generational LRU marks every page it faults in as
// active, so on a box with memory to spare those pages would count in the
// watcher's working set for as long as it runs. Under pressure the kernel
// finds again the pages the watcher keeps using and reclaims the rest first.
func adviseCold() error { return adviseOwnBinary(madvCold) }

func adviseOwnBinary(advice int) error {
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		return err
	}
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// A line of the file: 00400000-00b11000 r-xp 00000000 08:01 1234 /usr/libexec/snugbox/snugboxd
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || strings.Join(fields[5:], " ") != exe {
			continue
		}
		lo, hi, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		start, errLo := strconv.ParseUint(lo, 16, 64)
		end, errHi := strconv.ParseUint(hi, 16, 64)
		if errLo != nil || errHi != nil || end <= start {
			continue
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_MADVISE, uintptr(start), uintptr(end-start), uintptr(advice)); errno != 0 {
			return errno
		}
	}
	return sc.Err()
}
