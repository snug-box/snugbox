package model

import (
	"math"
	"slices"
	"time"
)

// RiseWindow is the span over which bursts are measured for headroom.
const RiseWindow = 60 * time.Second

// WorkloadSummary describes one workload over an observation window.
type WorkloadSummary struct {
	Samples        int
	WorkingSetQ95  uint64
	WorkingSetQ99  uint64
	WorkingSetQ999 uint64
	WorkingSetMax  uint64
	HardMax        uint64
	CurrentMax     uint64
	ShmemMax       uint64
	SockMax        uint64
	// Pressured is true when the workload stalled on memory or refaulted file
	// pages at least once in the window.
	Pressured bool
	// HotFileQ99 is the 99th percentile of the workload's hot file cache.
	HotFileQ99 uint64
	// OOMKills counts kills inside the window, from memory.events.
	OOMKills uint64
	// Peak is the latest memory.peak reading, when the kernel has the file.
	Peak    uint64
	HasPeak bool
	// PeakAtEnd is true when this cgroup still reported a peak at the end
	// of the window. Retired instances keep their own workload history.
	PeakAtEnd bool
	// CPU is the CPU time the workload used inside the window, HasCPU
	// whether its cgroup told, and Observed how much of the window the
	// workload was there for.
	CPU      time.Duration
	HasCPU   bool
	Observed time.Duration
}

// Summary describes a box over an observation window. It is everything the
// sizing rules need, so a long window can be summarised from stored aggregates
// and a short one from raw samples, and both go through Size.
type Summary struct {
	Start, End time.Time
	Samples    int
	// Gaps is the time inside the window with no samples, counting only
	// silences longer than three sampling intervals.
	Gaps time.Duration

	HardTotalMax  uint64
	TotalQ999     uint64
	TotalMax      uint64
	MaxRise       uint64
	HotFileQ99    uint64
	CurrentSumMax uint64
	// PeakCurrentMax is the highest sampled sum of memory.current over the
	// workloads that report memory.peak, the figure the peak check compares
	// the sum of their peaks with.
	PeakCurrentMax uint64
	KernelOther    WorkloadSummary

	// UncalibratedHotQ99 is the 99th percentile of hot file cache held by
	// workloads that never came under pressure, whose whole active file cache
	// was counted as hot.
	UncalibratedHotQ99 uint64

	// PressureFull is the box's full stall time inside the window.
	PressureFull time.Duration
	HasPressure  bool
	// SwapInIntervals counts sampling intervals in which pages came back from
	// swap, out of Intervals.
	SwapInIntervals int
	Intervals       int
	SwapInBytes     uint64
	// OOMKills counts box-wide kills inside the window, from /proc/vmstat.
	OOMKills uint64
	// CPUQ95 is the 95th percentile of CPU in use, in cores.
	CPUQ95 float64

	Workloads map[string]WorkloadSummary
}

// Window is the time between the first and the last sample.
func (s Summary) Window() time.Duration { return s.End.Sub(s.Start) }

// Coverage is the share of the window that has samples, from 0 to 1.
func (s Summary) Coverage() float64 {
	w := s.Window()
	if w <= 0 {
		return 0
	}
	return 1 - float64(s.Gaps)/float64(w)
}

// Summarize computes the summary of points derived from samples, exactly. The
// samples and points must be in the same order, oldest first. cpus is the
// number of online CPUs, used to turn CPU ticks into cores; pageSize is the
// kernel page size.
func Summarize(samples []Sample, points []Point, cpus int, pageSize uint64) Summary {
	var s Summary
	if len(points) == 0 || len(samples) != len(points) {
		return s
	}
	s.Start, s.End = points[0].Time, points[len(points)-1].Time
	s.Samples = len(points)
	s.Gaps = gaps(points)

	totals := make([]uint64, len(points))
	hot := make([]uint64, len(points))
	uncal := make([]uint64, len(points))
	kother := make([]uint64, len(points))
	for i, p := range points {
		totals[i], hot[i], kother[i] = p.Total, p.HotFile, p.KernelOther
		s.HardTotalMax = max(s.HardTotalMax, p.HardTotal)
		s.CurrentSumMax = max(s.CurrentSumMax, p.CurrentSum)
	}
	s.TotalQ999 = Quantile(totals, 0.999)
	s.TotalMax = slices.Max(totals)
	s.HotFileQ99 = Quantile(hot, 0.99)
	s.MaxRise = maxRise(points)
	s.KernelOther = WorkloadSummary{
		Samples:        len(points),
		WorkingSetQ95:  Quantile(kother, 0.95),
		WorkingSetQ99:  Quantile(kother, 0.99),
		WorkingSetQ999: Quantile(kother, 0.999),
		WorkingSetMax:  slices.Max(kother),
		HardMax:        slices.Max(kother),
	}

	s.Workloads = summarizeWorkloads(samples, points)
	for i, p := range points {
		var peaked uint64
		for id, wp := range p.Workloads {
			if !s.Workloads[id].Pressured {
				uncal[i] += wp.HotFile
			}
			if samples[i].Workloads[id].HasPeak {
				peaked += wp.Current
			}
		}
		s.PeakCurrentMax = max(s.PeakCurrentMax, peaked)
	}
	s.UncalibratedHotQ99 = Quantile(uncal, 0.99)
	s.boxSignals(samples, cpus, pageSize)
	return s
}

func (s *Summary) boxSignals(samples []Sample, cpus int, pageSize uint64) {
	first, last := samples[0].Box, samples[len(samples)-1].Box
	s.HasPressure = first.HasPressure && last.HasPressure
	if s.HasPressure && last.PressureFull >= first.PressureFull {
		s.PressureFull = last.PressureFull - first.PressureFull
	}
	cores := make([]float64, 0, len(samples))
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1].Box, samples[i].Box
		s.Intervals++
		s.OOMKills += counterDelta(a.OOMKills, b.OOMKills)
		if b.SwapIns > a.SwapIns {
			s.SwapInIntervals++
			s.SwapInBytes += (b.SwapIns - a.SwapIns) * pageSize
		}
		if b.CPUTotal > a.CPUTotal && b.CPUBusy >= a.CPUBusy {
			share := float64(b.CPUBusy-a.CPUBusy) / float64(b.CPUTotal-a.CPUTotal)
			cores = append(cores, share*float64(cpus))
		}
	}
	if len(cores) > 0 {
		slices.Sort(cores)
		s.CPUQ95 = cores[rankIndex(len(cores), 0.95)]
	}
}

func summarizeWorkloads(samples []Sample, points []Point) map[string]WorkloadSummary {
	type acc struct {
		ws, hot           []uint64
		hardMax, curMax   uint64
		shmemMax, sockMax uint64
		pressured         bool
		kills, prevKills  uint64
		peak              uint64
		hasPeak           bool
		cpu, prevCPU      time.Duration
		hasCPU            bool
		first, last       time.Time
	}
	accs := make(map[string]*acc)
	for i, p := range points {
		for id, wp := range p.Workloads {
			u := samples[i].Workloads[id]
			a := accs[id]
			if a == nil {
				a = &acc{prevKills: u.OOMKills, prevCPU: u.CPU, hasCPU: u.HasCPU, first: p.Time}
				accs[id] = a
			}
			a.ws = append(a.ws, wp.WorkingSet)
			a.hot = append(a.hot, wp.HotFile)
			a.hardMax = max(a.hardMax, wp.Hard)
			a.curMax = max(a.curMax, wp.Current)
			a.shmemMax = max(a.shmemMax, u.Shmem)
			a.sockMax = max(a.sockMax, u.Sock)
			a.pressured = a.pressured || wp.Pressured
			a.kills += counterDelta(a.prevKills, u.OOMKills)
			a.prevKills = u.OOMKills
			if u.HasPeak {
				a.peak, a.hasPeak = u.Peak, true
			}
			a.hasCPU = a.hasCPU && u.HasCPU
			if u.CPU >= a.prevCPU {
				a.cpu += u.CPU - a.prevCPU
			} else {
				a.cpu += u.CPU
			}
			a.prevCPU, a.last = u.CPU, p.Time
		}
	}
	out := make(map[string]WorkloadSummary, len(accs))
	for id, a := range accs {
		out[id] = WorkloadSummary{
			Samples:        len(a.ws),
			WorkingSetQ95:  Quantile(a.ws, 0.95),
			WorkingSetQ99:  Quantile(a.ws, 0.99),
			WorkingSetQ999: Quantile(a.ws, 0.999),
			WorkingSetMax:  slices.Max(a.ws),
			HardMax:        a.hardMax,
			CurrentMax:     a.curMax,
			ShmemMax:       a.shmemMax,
			SockMax:        a.sockMax,
			Pressured:      a.pressured,
			HotFileQ99:     Quantile(a.hot, 0.99),
			OOMKills:       a.kills,
			Peak:           a.peak,
			HasPeak:        a.hasPeak,
			PeakAtEnd:      samples[len(samples)-1].Workloads[id].HasPeak,
			CPU:            a.cpu,
			HasCPU:         a.hasCPU,
			Observed:       a.last.Sub(a.first),
		}
	}
	return out
}

// counterDelta is how much a cumulative counter grew from prev to cur. A
// smaller cur means the counter restarted from zero, so all of cur is new.
func counterDelta(prev, cur uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}

// gaps sums the silences between consecutive points that last longer than
// three times the typical interval, the median spacing of the series.
func gaps(points []Point) time.Duration {
	if len(points) < 3 {
		return 0
	}
	steps := make([]time.Duration, len(points)-1)
	for i := 1; i < len(points); i++ {
		steps[i-1] = points[i].Time.Sub(points[i-1].Time)
	}
	sorted := slices.Clone(steps)
	slices.Sort(sorted)
	typical := sorted[len(sorted)/2]
	var g time.Duration
	for _, st := range steps {
		if st > 3*typical {
			g += st - typical
		}
	}
	return g
}

// RiseTracker measures net growth over the last minute, discounting only
// the growth attributed to startup on each step. A concurrent increase in
// another workload still counts.
type RiseTracker struct {
	bases []riseBase
}

type riseBase struct {
	at    time.Time
	total uint64
}

// Add returns the largest rise ending at this sample. Adding startup to
// each earlier baseline excludes it from every interval that spans it.
func (r *RiseTracker) Add(at time.Time, total, startup uint64) uint64 {
	var best uint64
	kept := r.bases[:0]
	for _, base := range r.bases {
		if elapsed := at.Sub(base.at); elapsed <= 0 || elapsed > RiseWindow {
			continue
		}
		base.total += min(startup, math.MaxUint64-base.total)
		if total > base.total {
			best = max(best, total-base.total)
		}
		kept = append(kept, base)
	}
	r.bases = append(kept, riseBase{at, total})
	return best
}

func maxRise(points []Point) uint64 {
	var best uint64
	var tracker RiseTracker
	for _, p := range points {
		best = max(best, tracker.Add(p.Time, p.Total, p.StartupGrowth))
	}
	return best
}

// Quantile returns the q-quantile of vals by the nearest-rank method: the
// smallest value with at least q of the values at or below it. It returns zero
// for no values and does not change vals.
func Quantile(vals []uint64, q float64) uint64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := slices.Clone(vals)
	slices.Sort(sorted)
	return sorted[rankIndex(len(sorted), q)]
}

func rankIndex(n int, q float64) int {
	i := int(math.Ceil(q*float64(n))) - 1
	return min(max(i, 0), n-1)
}
