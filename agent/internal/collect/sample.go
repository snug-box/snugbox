package collect

import (
	"math"
	"path"
	"strconv"
	"time"

	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
)

// Sample reads the box-wide files and every workload found by the last
// Discover, and stamps the result with now. A workload whose cgroup has gone
// is left out; the next Discover drops it.
func (c *Collector) Sample(now time.Time) model.Sample {
	s := model.Sample{Time: now, Box: c.sampleBox(), Workloads: make(map[string]model.Usage, len(c.workloads))}
	for id, dir := range c.cgroupDir {
		if u, ok := c.sampleCgroup(dir); ok {
			s.Workloads[id] = u
		}
	}
	for id, pids := range c.groupPIDs {
		if u, ok := c.sampleProcesses(pids); ok {
			s.Workloads[id] = u
		}
	}
	return s
}

func (c *Collector) sampleBox() model.BoxUsage {
	var b model.BoxUsage
	if raw, err := c.read("proc/meminfo"); err != nil {
		c.gap("/proc/meminfo", err)
	} else {
		mi, _ := kernel.ParseMeminfo(raw)
		b.MemTotal = mi.Get("MemTotal")
		b.MemAvailable = mi.Get("MemAvailable")
		b.Slab = mi.Get("Slab")
		b.PageTables = mi.Get("PageTables") + mi.Get("SecPageTables")
		b.KernelStack = mi.Get("KernelStack")
		b.AnonHugePages = mi.Get("AnonHugePages")
		b.SwapTotal = mi.Get("SwapTotal")
		b.SwapFree = mi.Get("SwapFree")
	}
	if raw, err := c.read("proc/vmstat"); err != nil {
		c.gap("/proc/vmstat", err)
	} else {
		vm, _ := kernel.ParseStat(raw)
		b.SwapIns = vm.Get("pswpin")
		b.OOMKills = vm.Get("oom_kill")
	}
	if raw, err := c.read("proc/pressure/memory"); err != nil {
		c.gap("/proc/pressure/memory (Linux 4.20)", err)
	} else if p, err := kernel.ParsePressure(raw); err != nil {
		c.gap("/proc/pressure/memory", err)
	} else {
		b.PressureSome, b.PressureFull, b.HasPressure = p.Some.Total, p.Full.Total, p.HasFull
	}
	if raw, err := c.read("proc/stat"); err != nil {
		c.gap("/proc/stat", err)
	} else if ps, err := kernel.ParseProcStat(raw); err != nil {
		c.gap("/proc/stat", err)
	} else {
		b.CPUBusy, b.CPUTotal = ps.CPU.Busy(), ps.CPU.Total()
	}
	return b
}

func (c *Collector) sampleCgroup(dir string) (model.Usage, bool) {
	var u model.Usage
	raw, err := c.read(path.Join(dir, "memory.current"))
	if err != nil {
		return u, false
	}
	if u.Current, err = kernel.ParseUint(raw); err != nil {
		return u, false
	}
	if raw, err := c.read(path.Join(dir, "memory.stat")); err != nil {
		c.gap("memory.stat", err)
	} else {
		st, _ := kernel.ParseStat(raw)
		u.Anon = st.Get("anon")
		u.Shmem = st.Get("shmem")
		u.ActiveFile = st.Get("active_file")
		u.InactiveFile = st.Get("inactive_file")
		u.KernelStack = st.Get("kernel_stack")
		u.PageTables = st.Get("pagetables") + st.Get("sec_pagetables")
		u.Percpu = st.Get("percpu")
		u.SlabReclaimable = st.Get("slab_reclaimable")
		u.SlabUnreclaimable = st.Get("slab_unreclaimable")
		u.Vmalloc = st.Get("vmalloc")
		u.Sock = st.Get("sock")
		u.Zswap = st.Get("zswap")
		u.RefaultFile = st.Get("workingset_refault_file")
		if !st.Has("workingset_refault_file") {
			u.RefaultFile = st.Get("workingset_refault")
		}
	}
	if raw, err := c.read(path.Join(dir, "memory.events")); err != nil {
		c.gap("memory.events", err)
	} else {
		ev, _ := kernel.ParseStat(raw)
		u.OOMKills = ev.Get("oom_kill")
		u.HighEvents = ev.Get("high")
	}
	if raw, err := c.read(path.Join(dir, "memory.pressure")); err != nil {
		c.gap("memory.pressure per workload (Linux 4.20)", err)
	} else if p, err := kernel.ParsePressure(raw); err == nil {
		u.PressureSome, u.PressureFull = p.Some.Total, p.Full.Total
	}
	if raw, err := c.read(path.Join(dir, "memory.peak")); err != nil {
		c.gap("memory.peak (Linux 5.19)", err)
	} else if n, err := kernel.ParseUint(raw); err == nil {
		u.Peak, u.HasPeak = n, true
	}
	if raw, err := c.read(path.Join(dir, "memory.swap.current")); err == nil {
		if n, err := kernel.ParseUint(raw); err == nil {
			u.Swap = n
		}
	}
	// cpu.stat is there whether or not the cpu controller is on. Only the
	// idle check needs it, so a box without it loses nothing else.
	if raw, err := c.read(path.Join(dir, "cpu.stat")); err == nil {
		if st, _ := kernel.ParseStat(raw); st.Has("usage_usec") {
			u.CPU, u.HasCPU = time.Duration(min(st.Get("usage_usec"), uint64(math.MaxInt64/1000)))*time.Microsecond, true
		}
	}
	return u, true
}

// sampleProcesses measures a group of processes outside any useful cgroup from
// their smaps_rollup files. Proportional set sizes split shared pages between
// the processes that share them, so the sum is not inflated. These files have
// no page cache that isn't mapped, and no pressure or kill counters.
func (c *Collector) sampleProcesses(pids []int) (model.Usage, bool) {
	var u model.Usage
	read := 0
	for _, pid := range pids {
		raw, err := c.read(path.Join("proc", strconv.Itoa(pid), "smaps_rollup"))
		if err != nil {
			c.gap("/proc/PID/smaps_rollup for processes outside cgroups", err)
			continue
		}
		r, err := kernel.ParseSmapsRollup(raw)
		if err != nil {
			continue
		}
		read++
		anon := r.PssAnon
		if !r.HasPssSplit {
			anon = r.Anonymous
		}
		u.Anon += anon
		u.Shmem += r.PssShmem
		u.ActiveFile += r.PssFile
		u.Current += anon + r.PssShmem + r.PssFile
		u.Swap += r.SwapPss
	}
	return u, read > 0
}
