package collect

import (
	"bytes"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
)

// Crash loops, blueprint section 3.7's "idle and stuck workloads": Docker's
// restart policy has started the container again this many times, and it is
// waiting to restart now or started again within CrashLoopWithin.
const (
	CrashLoopRestarts = 3
	CrashLoopWithin   = 10 * time.Minute
)

// CrashLoop is a container Docker keeps restarting.
type CrashLoop struct {
	Name       string
	Restarts   int
	Started    time.Time
	Restarting bool
}

// CrashLoops returns the containers in a crash loop at now, by name, from
// the container runtime's last answer.
func (c *Collector) CrashLoops(now time.Time) []CrashLoop {
	var out []CrashLoop
	for id, m := range c.containerMeta {
		if m.Restarts < CrashLoopRestarts {
			continue
		}
		recent := !m.Started.IsZero() && now.Sub(m.Started) < CrashLoopWithin
		if !m.Restarting && !recent {
			continue
		}
		name := m.Name
		if name == "" && len(id) >= 12 {
			name = id[:12]
		}
		out = append(out, CrashLoop{Name: name, Restarts: m.Restarts, Started: m.Started, Restarting: m.Restarting})
	}
	slices.SortFunc(out, func(a, b CrashLoop) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// browserPrograms are the command names of the browsers that automation
// tools run headless. The kernel keeps 15 bytes of a name, so
// chromium-browser and chrome-headless-shell appear shortened.
var browserPrograms = []string{"chrome", "chromium", "chromium-browse", "headless_shell", "chrome-headless", "firefox"}

// initPrograms are the programs that adopt a process whose parent died:
// PID 1 on the box or in a container, or a user's systemd.
var initPrograms = []string{"systemd", "init", "tini", "dumb-init", "docker-init", "catatonit"}

// describeOrphans finds headless browsers whose parent has gone, left behind
// by automation that crashed or was restarted: a browser process started
// with --headless whose parent is PID 1 or an init, in a workload where
// other programs still run. It counts each such browser with every process
// below it and adds up their proportional set sizes. Only whether the
// command line holds --headless is kept.
func (c *Collector) describeOrphans(w *model.Workload, procs []procInfo) {
	// A browser adopted by an init is only left behind when something else
	// in the workload, the automation that should own it, is still there; a
	// workload that runs nothing but a browser, such as a browser service,
	// is what it looks like.
	owner := slices.ContainsFunc(procs, func(p procInfo) bool {
		return !slices.Contains(browserPrograms, p.name) && !slices.Contains(initPrograms, p.name)
	})
	if !owner {
		return
	}
	byPID := make(map[int]procInfo, len(procs))
	children := make(map[int][]int)
	for _, p := range procs {
		byPID[p.pid] = p
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	for _, p := range procs {
		if !slices.Contains(browserPrograms, p.name) || !c.adopted(p, byPID) || !c.headless(p.pid) {
			continue
		}
		w.OrphanBrowsers++
		w.OrphanProgram = p.name
		tree := []int{p.pid}
		for i := 0; i < len(tree) && len(tree) < maxPIDsPerWorkload; i++ {
			tree = append(tree, children[tree[i]]...)
		}
		w.OrphanProcesses += len(tree)
		for _, pid := range tree {
			raw, err := c.read(path.Join("proc", strconv.Itoa(pid), "smaps_rollup"))
			if err != nil {
				continue
			}
			if r, err := kernel.ParseSmapsRollup(raw); err == nil {
				w.OrphanBytes += r.Pss
			}
		}
	}
}

// adopted reports whether a process's parent is PID 1 or an init.
func (c *Collector) adopted(p procInfo, byPID map[int]procInfo) bool {
	if p.ppid == 1 {
		return true
	}
	if parent, ok := byPID[p.ppid]; ok {
		return slices.Contains(initPrograms, parent.name)
	}
	parent, ok := c.procStatus(p.ppid)
	return ok && slices.Contains(initPrograms, parent.name)
}

// headless reports whether a process's command line holds --headless, as
// --headless or --headless=new.
func (c *Collector) headless(pid int) bool {
	b, err := c.read(path.Join("proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	for arg := range bytes.SplitSeq(bytes.TrimRight(b, "\x00"), []byte{0}) {
		if bytes.Equal(arg, []byte("--headless")) || bytes.HasPrefix(arg, []byte("--headless=")) || bytes.Equal(arg, []byte("-headless")) {
			return true
		}
	}
	return false
}
