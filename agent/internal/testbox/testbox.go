// Package testbox builds the /proc, sysfs and cgroup files of a made-up box,
// for tests of everything that reads a box. Change a field and the next read
// sees it, so a test can play a box forward in time.
package testbox

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing/fstest"
	"time"
)

// Workload is one cgroup and its processes.
type Workload struct {
	// Path is the cgroup below the root, such as "system.slice/nginx.service".
	Path     string
	Programs []string
	// Cmdline is the NUL-separated command line of the first process.
	Cmdline      string
	Anon         uint64
	Shmem        uint64
	ActiveFile   uint64
	InactiveFile uint64
	Sock         uint64
	SlabReclaim  uint64
	SlabUnrecl   uint64
	Refault      uint64
	OOMKills     uint64
	// HighEvents is memory.events' high count: times the cgroup went over
	// memory.high.
	HighEvents   uint64
	PressureSome time.Duration
	PressureFull time.Duration
	Peak         uint64
	// Max is memory.max in bytes; zero writes "max".
	Max uint64
	// Started is when the workload's processes started, in clock ticks after
	// boot; raising it plays a restart.
	Started uint64
	pid     int
}

// Box is a made-up server.
type Box struct {
	mu sync.Mutex

	Hostname     string
	Kernel       string
	OSPretty     string
	CPUs         int
	MemTotal     uint64
	Slab         uint64
	PageTables   uint64
	KernelStack  uint64
	SwapBytes    uint64
	PressureSome time.Duration
	PressureFull time.Duration
	OOMKills     uint64
	SwapIns      uint64
	CPUBusy      uint64
	CPUIdle      uint64
	Uptime       time.Duration
	BootUnix     int64
	Vendor       string
	Virt         string
	Zswap        bool
	OOMD         bool
	Earlyoom     bool
	Workloads    []*Workload
	// Files are more files for the tree, by their path below the root,
	// such as a systemd timer unit.
	Files map[string]string
}

// New returns an 8 GiB, 4 CPU Ubuntu box on kernel 6.8 with nothing running.
func New() *Box {
	return &Box{
		Hostname: "web-1", Kernel: "6.8.0-45-generic", OSPretty: "Ubuntu 24.04.1 LTS", CPUs: 4,
		MemTotal: 8148000 << 10, Slab: 150 << 20, PageTables: 20 << 20, KernelStack: 8 << 20,
		Uptime: 10 * 24 * time.Hour, BootUnix: 1793000000, Vendor: "DigitalOcean", CPUBusy: 1000, CPUIdle: 9000,
	}
}

// Add adds a workload and gives it a process ID.
func (b *Box) Add(w *Workload) *Workload {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.pid = 1000 + 10*len(b.Workloads)
	b.Workloads = append(b.Workloads, w)
	return w
}

// Update changes the box under its lock, for a test that plays it forward.
func (b *Box) Update(f func(*Box)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f(b)
}

// Open implements fs.FS, rendering the tree as it is now.
func (b *Box) Open(name string) (fs.File, error) { return b.Snapshot().Open(name) }

// ReadFile implements fs.ReadFileFS.
func (b *Box) ReadFile(name string) ([]byte, error) { return b.Snapshot().ReadFile(name) }

// ReadDir implements fs.ReadDirFS.
func (b *Box) ReadDir(name string) ([]fs.DirEntry, error) { return b.Snapshot().ReadDir(name) }

// Stat implements fs.StatFS.
func (b *Box) Stat(name string) (fs.FileInfo, error) { return b.Snapshot().Stat(name) }

func f(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }

// Snapshot renders the box's files.
func (b *Box) Snapshot() fstest.MapFS {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := fstest.MapFS{
		"proc/meminfo": f(fmt.Sprintf("MemTotal: %d kB\nMemAvailable: %d kB\nSlab: %d kB\nPageTables: %d kB\nKernelStack: %d kB\nSwapTotal: %d kB\nSwapFree: %d kB\nAnonHugePages: 0 kB\n",
			b.MemTotal>>10, b.MemTotal>>11, b.Slab>>10, b.PageTables>>10, b.KernelStack>>10, b.SwapBytes>>10, b.SwapBytes>>10)),
		"proc/vmstat":                                f(fmt.Sprintf("pswpin %d\noom_kill %d\n", b.SwapIns, b.OOMKills)),
		"proc/pressure/memory":                       f(psi(b.PressureSome, b.PressureFull)),
		"proc/stat":                                  f(fmt.Sprintf("cpu  %d 0 0 %d 0 0 0 0 0 0\nbtime %d\n", b.CPUBusy, b.CPUIdle, b.BootUnix)),
		"proc/self/mountinfo":                        f("22 1 0:21 / /sys/fs/cgroup rw - cgroup2 cgroup2 rw\n23 1 252:1 / / rw - ext4 /dev/vda1 rw\n"),
		"proc/sys/kernel/hostname":                   f(b.Hostname + "\n"),
		"proc/sys/kernel/osrelease":                  f(b.Kernel + "\n"),
		"proc/sys/vm/overcommit_memory":              f("0\n"),
		"proc/sys/vm/swappiness":                     f("60\n"),
		"proc/uptime":                                f(fmt.Sprintf("%.2f 0.00\n", b.Uptime.Seconds())),
		"etc/os-release":                             f(fmt.Sprintf("PRETTY_NAME=%q\nID=ubuntu\nVERSION_ID=\"24.04\"\n", b.OSPretty)),
		"sys/devices/system/cpu/online":              f(fmt.Sprintf("0-%d\n", b.CPUs-1)),
		"sys/class/dmi/id/sys_vendor":                f(b.Vendor + "\n"),
		"sys/kernel/mm/transparent_hugepage/enabled": f("always [madvise] never\n"),
		"sys/fs/cgroup/cgroup.controllers":           f("cpuset cpu io memory pids\n"),
		"sys/fs/cgroup/cgroup.procs":                 f(""),
		"sys/fs/cgroup/memory.reclaim":               f(""),
		"run/systemd/system/.keep":                   f(""),
	}
	swaps := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n"
	if b.SwapBytes > 0 {
		swaps += fmt.Sprintf("/swap.img file %d 0 -2\n", b.SwapBytes>>10)
	}
	m["proc/swaps"] = f(swaps)
	if b.Zswap {
		m["sys/module/zswap/parameters/enabled"] = f("N\n")
		m["sys/module/zswap/parameters/compressor"] = f("lzo\n")
		m["sys/module/zswap/parameters/max_pool_percent"] = f("20\n")
		m["sys/module/zswap/parameters/shrinker_enabled"] = f("N\n")
	}
	if b.Virt != "" {
		m["run/systemd/container"] = f(b.Virt + "\n")
	}
	if b.OOMD {
		m["usr/lib/systemd/systemd-oomd"] = f("")
	}
	if b.Earlyoom {
		m["usr/bin/earlyoom"] = f("")
	}
	for _, w := range b.Workloads {
		dir := "sys/fs/cgroup/" + w.Path
		var pids []string
		for i, prog := range w.Programs {
			pid := w.pid + i
			pids = append(pids, fmt.Sprint(pid))
			rss := (w.Anon + w.ActiveFile) >> 10
			m[fmt.Sprintf("proc/%d/status", pid)] = f(fmt.Sprintf("Name:\t%s\nTgid:\t%d\nPPid:\t1\nUid:\t0\t0\t0\t0\nVmRSS:\t%d kB\n", prog, pid, rss/uint64(i+1)))
			m[fmt.Sprintf("proc/%d/stat", pid)] = f(fmt.Sprintf("%d (%s) S 1 %d %d 0 -1 4194560 1 0 0 0 1 1 0 0 20 0 1 0 %d 1000 10\n", pid, prog, pid, pid, w.Started+uint64(i)))
			if i == 0 && w.Cmdline != "" {
				m[fmt.Sprintf("proc/%d/cmdline", pid)] = f(w.Cmdline)
			}
		}
		file := w.Shmem + w.ActiveFile + w.InactiveFile
		cur := w.Anon + file + w.Sock + w.SlabReclaim + w.SlabUnrecl
		m[dir+"/cgroup.procs"] = f(strings.Join(pids, "\n") + "\n")
		m[dir+"/memory.current"] = f(fmt.Sprintf("%d\n", cur))
		m[dir+"/memory.stat"] = f(fmt.Sprintf("anon %d\nfile %d\nshmem %d\nactive_file %d\ninactive_file %d\nsock %d\nslab_reclaimable %d\nslab_unreclaimable %d\nslab %d\nkernel_stack 0\npagetables 0\nworkingset_refault_file %d\n",
			w.Anon, file, w.Shmem, w.ActiveFile, w.InactiveFile, w.Sock, w.SlabReclaim, w.SlabUnrecl, w.SlabReclaim+w.SlabUnrecl, w.Refault))
		m[dir+"/memory.events"] = f(fmt.Sprintf("low 0\nhigh %d\nmax 0\noom %d\noom_kill %d\noom_group_kill 0\n", w.HighEvents, w.OOMKills, w.OOMKills))
		m[dir+"/memory.pressure"] = f(psi(w.PressureSome, w.PressureFull))
		m[dir+"/memory.peak"] = f(fmt.Sprintf("%d\n", max(w.Peak, cur)))
		m[dir+"/memory.high"] = f("max\n")
		if w.Max > 0 {
			m[dir+"/memory.max"] = f(fmt.Sprintf("%d\n", w.Max))
		} else {
			m[dir+"/memory.max"] = f("max\n")
		}
	}
	for p, s := range b.Files {
		m[p] = f(s)
	}
	return m
}

func psi(some, full time.Duration) string {
	return fmt.Sprintf("some avg10=0.00 avg60=0.00 avg300=0.00 total=%d\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=%d\n",
		some.Microseconds(), full.Microseconds())
}
