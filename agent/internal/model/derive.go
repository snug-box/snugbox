package model

import "time"

// RefaultWindow is how far back refaults count toward a workload's hot file
// cache.
const RefaultWindow = 60 * time.Second

// WorkloadPoint is a workload's derived memory at one moment.
type WorkloadPoint struct {
	Hard       uint64
	HotFile    uint64
	WorkingSet uint64
	Current    uint64
	// Pressured is true when the workload stalled on memory or refaulted file
	// pages since the previous sample, which is when the kernel's file lists
	// say something about what is hot.
	Pressured bool
}

// Point is a box's derived memory at one moment: the series hard_total(t) and
// total(t) of blueprint section 3.6, with their parts.
type Point struct {
	Time        time.Time
	HardTotal   uint64
	Total       uint64
	HotFile     uint64
	KernelOther uint64
	CurrentSum  uint64
	// StartupGrowth is the part of this step's increase due to workloads
	// first seen in the last minute. Their memory still counts in Total.
	StartupGrowth uint64
	Workloads     map[string]WorkloadPoint
}

type refaultMark struct {
	at      time.Time
	refault uint64
}

type workloadState struct {
	marks      []refaultMark
	pressure   time.Duration
	refault    uint64
	started    time.Time
	workingSet uint64
}

// Deriver turns samples into points. It remembers the last minute of refault
// counters for each workload, so samples must arrive in time order.
type Deriver struct {
	pageSize uint64
	state    map[string]*workloadState
	previous time.Time
}

// NewDeriver returns a Deriver for a kernel with the given page size in bytes.
func NewDeriver(pageSize uint64) *Deriver {
	if pageSize == 0 {
		pageSize = 4 * KiB
	}
	return &Deriver{pageSize: pageSize, state: make(map[string]*workloadState)}
}

// Add derives the point for s.
//
// A workload's hot file cache is its active file cache plus the file pages it
// refaulted in the last minute, capped at its inactive file cache. Refaulted
// pages may already sit on the active list, so this can count some pages
// twice; the error is on the side of a bigger need.
func (d *Deriver) Add(s Sample) Point {
	p := Point{Time: s.Time, Workloads: make(map[string]WorkloadPoint, len(s.Workloads))}
	var charged uint64
	for id, u := range s.Workloads {
		st := d.state[id]
		if st == nil {
			st = &workloadState{pressure: u.PressureSome, refault: u.RefaultFile}
			if !d.previous.IsZero() && s.Time.Sub(d.previous) <= RiseWindow {
				st.started = s.Time
			}
			d.state[id] = st
		}
		refaulted := d.refaultedBytes(st, s.Time, u.RefaultFile)
		hot := u.ActiveFile + min(u.InactiveFile, refaulted)
		wp := WorkloadPoint{
			Hard:      u.Hard(),
			HotFile:   hot,
			Current:   u.Current,
			Pressured: u.PressureSome > st.pressure || u.RefaultFile > st.refault,
		}
		wp.WorkingSet = wp.Hard + wp.HotFile
		if !st.started.IsZero() && s.Time.Sub(st.started) < RiseWindow && wp.WorkingSet > st.workingSet {
			p.StartupGrowth += wp.WorkingSet - st.workingSet
		}
		st.workingSet = wp.WorkingSet
		st.pressure, st.refault = u.PressureSome, u.RefaultFile

		p.Workloads[id] = wp
		p.HardTotal += wp.Hard
		p.Total += wp.WorkingSet
		p.HotFile += wp.HotFile
		p.CurrentSum += wp.Current
		charged += u.chargedKernel()
	}
	for id := range d.state {
		if _, ok := s.Workloads[id]; !ok {
			delete(d.state, id)
		}
	}
	kernel := s.Box.Slab + s.Box.PageTables + s.Box.KernelStack
	if kernel > charged {
		p.KernelOther = kernel - charged
	}
	p.HardTotal += p.KernelOther
	p.Total += p.KernelOther
	d.previous = s.Time
	return p
}

// refaultedBytes returns the bytes refaulted within RefaultWindow before at,
// and records the new counter value. A counter that went down belongs to a
// recreated cgroup, whose whole count is recent.
func (d *Deriver) refaultedBytes(st *workloadState, at time.Time, counter uint64) uint64 {
	cutoff := at.Add(-RefaultWindow)
	keep := st.marks[:0]
	for _, m := range st.marks {
		if !m.at.Before(cutoff) && m.refault <= counter {
			keep = append(keep, m)
		}
	}
	st.marks = append(keep, refaultMark{at: at, refault: counter})
	oldest := st.marks[0].refault
	if len(st.marks) == 1 && counter < st.refault {
		oldest = 0
	}
	return (counter - oldest) * d.pageSize
}
