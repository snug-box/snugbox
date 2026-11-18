package collect

import (
	"bytes"
	"debug/buildinfo"
	"go/version"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
)

// heapFlagPrefixes are the only command-line arguments Snugbox keeps. Any
// other argument may hold a password and is never read past this filter.
var heapFlagPrefixes = map[string][]string{
	"java": {"-Xmx", "-Xms", "-XX:MaxRAMPercentage=", "-XX:InitialRAMPercentage=", "-XX:MaxRAM=", "-XX:-UseContainerSupport"},
	"node": {"--max-old-space-size=", "--max-semi-space-size="},
}

// runtimeOf names the runtime a program belongs to, for the heap cap fix.
func runtimeOf(program string) string {
	switch {
	case program == "java":
		return "java"
	case program == "node", program == "nodejs":
		return "node"
	case strings.HasPrefix(program, "php-fpm"):
		return "php-fpm"
	case strings.HasPrefix(program, "python"), program == "gunicorn", program == "uwsgi", program == "celery":
		return "python"
	case program == "ruby", program == "puma", program == "unicorn", program == "sidekiq":
		return "ruby"
	}
	return ""
}

// describeRuntime sets the runtime of the workload's largest program that has
// one, and the heap flags of that program's largest process. A service or a
// container whose programs name no runtime may still be a Go program, which
// only its executable tells.
func (c *Collector) describeRuntime(w *model.Workload, procs []procInfo) {
	var best procInfo
	for _, p := range procs {
		rt := runtimeOf(p.name)
		if rt == "" {
			continue
		}
		if w.Runtime == "" || (rt == w.Runtime && p.rss > best.rss) {
			w.Runtime, best = rt, p
		}
	}
	if w.Runtime == "" && goCandidate(w) {
		for _, p := range procs {
			if p.rss > best.rss || best.pid == 0 {
				best = p
			}
		}
		if best.pid != 0 && c.goProgram(best.pid) {
			w.Runtime = "go"
		}
	}
	if w.Runtime == "" {
		return
	}
	w.Threads = best.threads
	if w.Runtime == "php-fpm" {
		c.describeFPM(w, procs)
	}
	prefixes := heapFlagPrefixes[w.Runtime]
	if len(prefixes) == 0 {
		return
	}
	b, err := c.read(path.Join("proc", strconv.Itoa(best.pid), "cmdline"))
	if err != nil {
		return
	}
	w.HeapFlags = heapFlags(b, prefixes)
}

// goCandidate reports whether a workload's executable is worth reading for
// Go: system services and containers, where a heap cap can follow. A user's
// own session and services are left alone, so nobody can make the watcher
// parse a file of their choosing.
func goCandidate(w *model.Workload) bool {
	switch w.Kind {
	case model.KindContainer:
		return true
	case model.KindService:
		return !strings.HasPrefix(w.ID, "/user.slice/")
	}
	return false
}

// goMemLimitSince is the first Go release whose runtime reads GOMEMLIMIT.
const goMemLimitSince = "go1.19"

// goExe is what Snugbox learned about one process's executable, kept while
// the file's size and time stay the same.
type goExe struct {
	size int64
	mod  time.Time
	isGo bool
}

// goProgram reports whether a process runs a Go program built with Go 1.19
// or newer. It reads the executable's headers and the build information Go
// writes into it, nothing else, and each executable only once while its
// process lives.
func (c *Collector) goProgram(pid int) bool {
	name := path.Join("proc", strconv.Itoa(pid), "exe")
	fi, err := fs.Stat(c.fsys, name)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if c.goSeen == nil {
		c.goSeen = make(map[int]goExe)
	}
	if e, ok := c.goExes[pid]; ok && e.size == fi.Size() && e.mod.Equal(fi.ModTime()) {
		c.goSeen[pid] = e
		return e.isGo
	}
	e := goExe{size: fi.Size(), mod: fi.ModTime(), isGo: c.builtWithGo(name)}
	c.goSeen[pid] = e
	return e.isGo
}

// builtWithGo reads an executable's build information. A file debug/elf
// can't parse, or that makes it panic, is not a Go program as far as
// Snugbox is concerned.
func (c *Collector) builtWithGo(name string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	f, err := c.fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	ra, isReaderAt := f.(io.ReaderAt)
	if !isReaderAt {
		return false
	}
	bi, err := buildinfo.Read(ra)
	if err != nil {
		return false
	}
	return version.IsValid(bi.GoVersion) && version.Compare(bi.GoVersion, goMemLimitSince) >= 0
}

// heapFlags returns the arguments of a NUL-separated command line that start
// with one of prefixes.
func heapFlags(cmdline []byte, prefixes []string) []string {
	var out []string
	for arg := range bytes.SplitSeq(bytes.TrimRight(cmdline, "\x00"), []byte{0}) {
		for _, p := range prefixes {
			if bytes.HasPrefix(arg, []byte(p)) && len(arg) <= 64 {
				out = append(out, string(arg))
				break
			}
		}
	}
	return out
}

// describeFPM counts PHP-FPM's workers, the php-fpm processes whose parent is
// another php-fpm process of the workload, and keeps the largest
// proportional set size any of them has had. Proportional sizes split the
// pages workers share, OPcache's among them, so one worker's is what one
// more worker costs.
func (c *Collector) describeFPM(w *model.Workload, procs []procInfo) {
	fpm := make(map[int]bool, len(procs))
	for _, p := range procs {
		if runtimeOf(p.name) == "php-fpm" {
			fpm[p.pid] = true
		}
	}
	largest := c.fpmLargest[w.ID]
	for _, p := range procs {
		if !fpm[p.pid] || !fpm[p.ppid] {
			continue
		}
		w.Workers++
		raw, err := c.read(path.Join("proc", strconv.Itoa(p.pid), "smaps_rollup"))
		if err != nil {
			c.gap("/proc/PID/smaps_rollup for PHP-FPM workers", err)
			continue
		}
		if r, err := kernel.ParseSmapsRollup(raw); err == nil {
			largest = max(largest, r.Pss)
		}
	}
	if w.Workers == 0 || largest == 0 {
		return
	}
	w.WorkerBytes = largest
	if c.fpmSeen == nil {
		c.fpmSeen = make(map[string]uint64)
	}
	c.fpmSeen[w.ID] = largest
}
