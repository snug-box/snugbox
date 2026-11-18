// Package collect reads a box's memory signals from /proc, sysfs and the
// cgroup v2 filesystem, and finds the workloads that use the memory.
//
// Everything is read through an fs.FS rooted at "/", so a recorded tree of
// kernel files replays exactly like a live box. Reads that fail are not fatal:
// each one becomes a Gap, which the report's data quality section lists.
package collect

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
)

// Gap is something Snugbox could not read, and why.
type Gap struct {
	What string
	Why  string
}

// ContainerMeta is what the container runtime says about one container.
type ContainerMeta struct {
	Name        string
	Image       string
	Compose     model.Compose
	Reservation uint64
	Restarts    int
	// Started is when the container last started, and Restarting is true
	// while Docker waits to start it again after it exited.
	Started    time.Time
	Restarting bool
}

// ContainerSource names containers. ids are the container IDs the cgroup
// tree shows, so a source that keeps an earlier answer can tell when to ask
// again. The one-off audit reads Docker through it; the watcher asks
// snugbox-helper; tests replace it.
type ContainerSource interface {
	Containers(ctx context.Context, ids []string) (map[string]ContainerMeta, error)
}

// Options configure a Collector.
type Options struct {
	// FS is the filesystem to read, rooted at "/". It defaults to the live
	// root.
	FS fs.FS
	// PageSize is the kernel page size in bytes. It defaults to the running
	// kernel's.
	PageSize uint64
	// Containers names Docker containers. Nil leaves them under their IDs.
	Containers ContainerSource
}

// Collector reads one box. Discover finds the workloads; Sample reads them.
// A Collector is safe for use by one goroutine at a time.
type Collector struct {
	fsys       fs.FS
	pageSize   uint64
	containers ContainerSource
	cgroupRoot string

	workloads map[string]model.Workload
	cgroupDir map[string]string
	groupPIDs map[string][]int
	// goExes holds what the last Discover learned about executables, by
	// process, and goSeen what this one has looked at, so a process that
	// is gone is forgotten.
	goExes, goSeen map[int]goExe
	// fpmLargest holds the largest PHP-FPM worker each workload has had, by
	// workload ID, kept the same way.
	fpmLargest, fpmSeen map[string]uint64
	// containerMeta is the container runtime's last answer, for every
	// container it listed, running or waiting to restart.
	containerMeta map[string]ContainerMeta

	mu   sync.Mutex
	gaps map[string]Gap
}

// New returns a Collector. It fails only when the filesystem has no /proc.
func New(opts Options) (*Collector, error) {
	c := &Collector{
		fsys:       opts.FS,
		pageSize:   opts.PageSize,
		containers: opts.Containers,
		workloads:  make(map[string]model.Workload),
		cgroupDir:  make(map[string]string),
		groupPIDs:  make(map[string][]int),
		gaps:       make(map[string]Gap),
	}
	if c.fsys == nil {
		c.fsys = os.DirFS("/")
	}
	if c.pageSize == 0 {
		if ps := os.Getpagesize(); ps > 0 {
			c.pageSize = uint64(ps)
		}
	}
	if _, err := fs.Stat(c.fsys, "proc/meminfo"); err != nil {
		return nil, errors.New("no /proc/meminfo: this does not look like a Linux system")
	}
	c.cgroupRoot = c.findCgroupRoot()
	return c, nil
}

// PageSize returns the kernel page size the collector assumes.
func (c *Collector) PageSize() uint64 { return c.pageSize }

// CgroupRoot returns the path of the cgroup v2 mount without its leading
// slash, or "" when the box has no cgroup v2.
func (c *Collector) CgroupRoot() string { return c.cgroupRoot }

// Workloads returns the workloads found by the last Discover, keyed by ID.
func (c *Collector) Workloads() map[string]model.Workload { return maps.Clone(c.workloads) }

// OldestStart returns when the longest-running process of a workload
// started, in clock ticks after boot. When it moves later, every process the
// workload had is gone: the workload restarted. False means no process of
// it could be read.
func (c *Collector) OldestStart(id string) (uint64, bool) {
	pids := c.groupPIDs[id]
	if dir, ok := c.cgroupDir[id]; ok {
		pids = c.treePIDs(dir, maxPIDsPerWorkload)
	}
	var oldest uint64
	found := false
	for _, pid := range pids {
		b, err := c.read(path.Join("proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			continue
		}
		if start, err := kernel.ParseProcStart(b); err == nil && (!found || start < oldest) {
			oldest, found = start, true
		}
	}
	return oldest, found
}

// Gaps returns everything that could not be read so far, sorted by What.
func (c *Collector) Gaps() []Gap {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := slices.Collect(maps.Values(c.gaps))
	slices.SortFunc(out, func(a, b Gap) int { return strings.Compare(a.What, b.What) })
	return out
}

func (c *Collector) gap(what string, err error) {
	why := "missing"
	switch {
	case errors.Is(err, fs.ErrPermission):
		why = "permission denied; run with sudo for a full audit"
	case errors.Is(err, fs.ErrNotExist):
		why = "not present on this kernel"
	case err != nil:
		why = err.Error()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.gaps[what]; !ok {
		c.gaps[what] = Gap{What: what, Why: why}
	}
}

func (c *Collector) read(name string) ([]byte, error) { return fs.ReadFile(c.fsys, name) }

// findCgroupRoot locates the cgroup v2 mount from the mount table, falling
// back to the usual places when the table can't be read.
func (c *Collector) findCgroupRoot() string {
	if b, err := c.read("proc/self/mountinfo"); err == nil {
		if mounts, err := kernel.ParseMountinfo(b); err == nil {
			for _, m := range mounts {
				if m.FSType == "cgroup2" {
					root := strings.TrimPrefix(path.Clean(m.MountPoint), "/")
					if c.exists(path.Join(root, "cgroup.controllers")) {
						return root
					}
				}
			}
		}
	}
	for _, root := range []string{"sys/fs/cgroup", "sys/fs/cgroup/unified"} {
		if c.exists(path.Join(root, "cgroup.controllers")) {
			return root
		}
	}
	return ""
}

func (c *Collector) exists(name string) bool {
	_, err := fs.Stat(c.fsys, name)
	return err == nil
}

func (c *Collector) readPIDs(name string) []int {
	b, err := c.read(name)
	if err != nil {
		return nil
	}
	var pids []int
	for f := range strings.FieldsSeq(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// treePIDs collects the processes of a cgroup and of the cgroups below it,
// such as the init.scope inside user@1000.service, up to limit.
func (c *Collector) treePIDs(dir string, limit int) []int {
	pids := c.readPIDs(path.Join(dir, "cgroup.procs"))
	if len(pids) >= limit {
		return pids[:limit]
	}
	entries, err := fs.ReadDir(c.fsys, dir)
	if err != nil {
		return pids
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pids = append(pids, c.treePIDs(path.Join(dir, e.Name()), limit-len(pids))...)
		if len(pids) >= limit {
			return pids[:limit]
		}
	}
	return pids
}
