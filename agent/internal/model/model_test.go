package model

import (
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

func gib(x float64) uint64 { return uint64(math.Round(x * float64(GiB))) }

// The worked example of blueprint section 3.6.
func TestSizeWorkedExample(t *testing.T) {
	s := Summary{
		Start:        t0.Add(-14 * 24 * time.Hour),
		End:          t0,
		Samples:      120960,
		HardTotalMax: gib(2.1),
		TotalQ999:    gib(2.6),
		MaxRise:      gib(0.4),
		HotFileQ99:   gib(0.6),
	}
	e := Size(s, History{Now: t0})
	if e.Need != gib(2.6) || e.NeedFromHard {
		t.Errorf("need = %d (from hard %v), want 2.6 GiB from the working set", e.Need, e.NeedFromHard)
	}
	if e.HeadroomFrom != HeadroomBurst || e.Headroom != scale(gib(0.4), 1.2) {
		t.Errorf("headroom = %d from %s, want 1.2 x 0.4 GiB from the burst rule", e.Headroom, e.HeadroomFrom)
	}
	if e.Cache != gib(0.3) {
		t.Errorf("cache = %d, want 0.3 GiB", e.Cache)
	}
	if d := math.Abs(float64(e.Snug) - float64(gib(3.38))); d > 2 {
		t.Errorf("snug = %d, want 3.38 GiB", e.Snug)
	}
	if e.Confidence != ConfidenceHigh {
		t.Errorf("confidence = %v, want high", e.Confidence)
	}
	if e.Upward.Any() {
		t.Errorf("upward signs on a healthy box: %+v", e.Upward)
	}
	if usable := 0.93 * 4 * float64(GiB); float64(e.Snug) > usable {
		t.Errorf("a 4 GiB plan with %v usable bytes should fit %d", usable, e.Snug)
	}
	if usable := 0.93 * 2 * float64(GiB); float64(e.Snug) <= usable {
		t.Errorf("a 2 GiB plan should not fit %d", e.Snug)
	}
}

func TestSizeHeadroomRules(t *testing.T) {
	small := Size(Summary{HardTotalMax: 100 * MiB, TotalQ999: 120 * MiB}, History{})
	if small.HeadroomFrom != HeadroomMinimum || small.Headroom != MinHeadroom || small.Cache != MinCache {
		t.Errorf("small box: headroom %d from %s, cache %d", small.Headroom, small.HeadroomFrom, small.Cache)
	}
	big := Size(Summary{HardTotalMax: 20 * GiB, TotalQ999: 30 * GiB, MaxRise: 100 * MiB}, History{})
	if big.HeadroomFrom != HeadroomShareOfNeed || big.Headroom != 3*GiB {
		t.Errorf("big box: headroom %d from %s, want 3 GiB from the share rule", big.Headroom, big.HeadroomFrom)
	}
	if !big.NeedFromHard && big.Need != 30*GiB {
		t.Errorf("need = %d", big.Need)
	}
}

func TestPeakCheck(t *testing.T) {
	s := Summary{
		HardTotalMax:   GiB,
		TotalQ999:      GiB,
		PeakCurrentMax: 2 * GiB,
		Workloads: map[string]WorkloadSummary{
			"/a.service": {Peak: 2 * GiB, HasPeak: true, PeakAtEnd: true, CurrentMax: 2 * GiB},
			"/b.service": {Peak: GiB, HasPeak: true, PeakAtEnd: true, CurrentMax: GiB},
			"/retired":   {Peak: 13 * GiB, HasPeak: true, CurrentMax: 13 * GiB},
			"/c":         {Peak: 5 * GiB},
		},
	}
	e := Size(s, History{})
	if e.PeakSum != 3*GiB || e.PeakAddition != GiB || e.Headroom != MinHeadroom+GiB {
		t.Errorf("peak sum %d, addition %d, headroom %d", e.PeakSum, e.PeakAddition, e.Headroom)
	}
	s.PeakCurrentMax = 2600 * MiB
	if e := Size(s, History{}); e.PeakAddition != 0 {
		t.Errorf("a 15%% difference added %d", e.PeakAddition)
	}
}

func TestSizingAcrossContainerReplacement(t *testing.T) {
	d := NewDeriver(4096)
	var samples []Sample
	var ps []Point
	for i := range 10 {
		id, memory := "old", 12*GiB
		if i >= 2 {
			id, memory = "new", 10*GiB
			if i >= 3 {
				memory += 2 * GiB
			}
			if i >= 9 {
				memory += 512 * MiB
			}
		}
		app := GiB
		if i >= 2 {
			app += 128 * MiB
		}
		s := Sample{Time: t0.Add(time.Duration(i) * 10 * time.Second), Workloads: map[string]Usage{
			"app": {Anon: app, Current: app, Peak: app, HasPeak: true},
		}}
		if i != 1 {
			s.Workloads[id] = Usage{Anon: memory, Current: memory, Peak: memory, HasPeak: true}
		}
		samples = append(samples, s)
		ps = append(ps, d.Add(s))
	}
	s := Summarize(samples, ps, 1, 4096)
	if s.MaxRise != 512*MiB || ps[2].StartupGrowth != 10*GiB || maxRise(ps[:4]) != 128*MiB {
		t.Fatalf("startup or concurrent growth miscounted: rise %d, opening %d, early rise %d", s.MaxRise, ps[2].StartupGrowth, maxRise(ps[:4]))
	}
	e := Size(s, History{})
	if s.Workloads["old"].PeakAtEnd || !s.Workloads["old"].HasPeak || e.PeakAddition != 0 || e.PeakSum != ps[9].CurrentSum || e.Need != ps[9].Total {
		t.Fatalf("replacement estimate %+v, old workload %+v", e, s.Workloads["old"])
	}
}

func TestConfidence(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		window time.Duration
		gaps   time.Duration
		uncal  uint64
		want   Confidence
	}{
		{time.Minute, 0, 0, ConfidenceLow},
		{6 * day, 0, 0, ConfidenceLow},
		{7 * day, 0, 0, ConfidenceMedium},
		{7 * day, day, 0, ConfidenceLow},
		{14 * day, 0, 0, ConfidenceHigh},
		{14 * day, 0, 600 * MiB, ConfidenceMedium},
		{20 * day, day, 0, ConfidenceHigh},
	}
	for _, c := range cases {
		s := Summary{Start: t0, End: t0.Add(c.window), Gaps: c.gaps, HardTotalMax: GiB, TotalQ999: 2 * GiB, UncalibratedHotQ99: c.uncal}
		if got := Size(s, History{}).Confidence; got != c.want {
			t.Errorf("window %v, gaps %v, uncalibrated %d MiB: confidence %v, want %v", c.window, c.gaps, c.uncal/MiB, got, c.want)
		}
	}
}

func TestUpward(t *testing.T) {
	week := KillLookback
	t.Run("pressure since boot on a short audit", func(t *testing.T) {
		h := History{Now: t0, Uptime: 100 * time.Hour, HasPressure: true, PressureFull: 2 * time.Hour}
		u := Size(Summary{Start: t0.Add(-time.Minute), End: t0, HasPressure: true}, h).Upward
		if !u.Pressure() || !u.PressureSinceBoot {
			t.Errorf("2%% full pressure since boot not flagged: %+v", u)
		}
	})
	t.Run("pressure over a long window", func(t *testing.T) {
		s := Summary{Start: t0.Add(-48 * time.Hour), End: t0, HasPressure: true, PressureFull: 10 * time.Minute}
		h := History{Now: t0, Uptime: 1000 * time.Hour, HasPressure: true, PressureFull: 100 * time.Hour}
		u := Size(s, h).Upward
		if u.Pressure() || u.PressureSinceBoot {
			t.Errorf("0.35%% over the window should win over the boot total: %+v", u)
		}
	})
	t.Run("dated kills", func(t *testing.T) {
		h := History{Now: t0, Uptime: 30 * 24 * time.Hour, OOMKills: 2, LogRead: true, LogSince: t0.Add(-20 * 24 * time.Hour),
			Kills: []time.Time{t0.Add(-10 * 24 * time.Hour), t0.Add(-time.Hour)}}
		u := Size(Summary{}, h).Upward
		if u.RecentKills != 1 || u.UndatedKills != 0 {
			t.Errorf("recent %d undated %d, want 1 and 0", u.RecentKills, u.UndatedKills)
		}
	})
	t.Run("kills lost from a short log", func(t *testing.T) {
		h := History{Now: t0, Uptime: 30 * 24 * time.Hour, OOMKills: 3, LogRead: true, LogSince: t0.Add(-2 * 24 * time.Hour)}
		if u := Size(Summary{}, h).Upward; u.UndatedKills != 3 || !u.Kills() {
			t.Errorf("undated = %d, want 3", u.UndatedKills)
		}
	})
	t.Run("kills older than a long log", func(t *testing.T) {
		h := History{Now: t0, Uptime: 30 * 24 * time.Hour, OOMKills: 3, LogRead: true, LogSince: t0.Add(-week - time.Hour)}
		if u := Size(Summary{}, h).Upward; u.Kills() {
			t.Errorf("kills before the log began counted as recent: %+v", u)
		}
	})
	t.Run("short uptime", func(t *testing.T) {
		h := History{Now: t0, Uptime: 3 * 24 * time.Hour, OOMKills: 1}
		if u := Size(Summary{}, h).Upward; u.RecentKills != 1 {
			t.Errorf("recent = %d, want 1", u.RecentKills)
		}
	})
	t.Run("swap-ins", func(t *testing.T) {
		if u := Size(Summary{Intervals: 30, SwapInIntervals: 15}, History{}).Upward; !u.Swapping() {
			t.Error("swap-ins in half the intervals not flagged")
		}
		if u := Size(Summary{Intervals: 30, SwapInIntervals: 3}, History{}).Upward; u.Swapping() {
			t.Error("occasional swap-ins flagged")
		}
	})
}

func usage(anon, shmem, active, inactive uint64) Usage {
	return Usage{Current: anon + shmem + active + inactive, Anon: anon, Shmem: shmem, ActiveFile: active, InactiveFile: inactive}
}

func TestDeriverHotFile(t *testing.T) {
	d := NewDeriver(4096)
	u := usage(100*MiB, 0, 50*MiB, 400*MiB)
	p := d.Add(Sample{Time: t0, Workloads: map[string]Usage{"/w": u}})
	if w := p.Workloads["/w"]; w.HotFile != 50*MiB || w.WorkingSet != 150*MiB || w.Pressured {
		t.Fatalf("first point %+v", w)
	}
	u.RefaultFile = 1000
	u.PressureSome = time.Second
	p = d.Add(Sample{Time: t0.Add(10 * time.Second), Workloads: map[string]Usage{"/w": u}})
	if w := p.Workloads["/w"]; w.HotFile != 50*MiB+1000*4096 || !w.Pressured {
		t.Errorf("after refaults %+v", w)
	}
	p = d.Add(Sample{Time: t0.Add(75 * time.Second), Workloads: map[string]Usage{"/w": u}})
	if w := p.Workloads["/w"]; w.HotFile != 50*MiB || w.Pressured {
		t.Errorf("refaults older than a minute still counted: %+v", w)
	}
	u.RefaultFile = 1 << 40
	p = d.Add(Sample{Time: t0.Add(80 * time.Second), Workloads: map[string]Usage{"/w": u}})
	if w := p.Workloads["/w"]; w.HotFile != 50*MiB+400*MiB {
		t.Errorf("refaults not capped at inactive file: %+v", w)
	}
	u.RefaultFile = 10
	p = d.Add(Sample{Time: t0.Add(90 * time.Second), Workloads: map[string]Usage{"/w": u}})
	if w := p.Workloads["/w"]; w.HotFile != 50*MiB+10*4096 {
		t.Errorf("recreated cgroup: %+v", w)
	}
}

func TestDeriverKernelOther(t *testing.T) {
	d := NewDeriver(4096)
	w := usage(GiB, 0, 0, 0)
	w.SlabReclaimable, w.SlabUnreclaimable, w.PageTables, w.KernelStack = 10*MiB, 20*MiB, 5*MiB, MiB
	box := BoxUsage{Slab: 100 * MiB, PageTables: 15 * MiB, KernelStack: 6 * MiB}
	p := d.Add(Sample{Time: t0, Box: box, Workloads: map[string]Usage{"/w": w}})
	if p.KernelOther != 85*MiB {
		t.Errorf("kernel other = %d MiB, want 85", p.KernelOther/MiB)
	}
	wantHard := GiB + 20*MiB + 5*MiB + MiB + 85*MiB
	if p.HardTotal != wantHard || p.Total != wantHard {
		t.Errorf("hard total %d, total %d, want %d", p.HardTotal, p.Total, wantHard)
	}
	p = d.Add(Sample{Time: t0.Add(time.Second), Box: BoxUsage{Slab: MiB}, Workloads: map[string]Usage{"/w": w}})
	if p.KernelOther != 0 {
		t.Errorf("kernel other went negative: %d", p.KernelOther)
	}
}

func points(totals ...uint64) []Point {
	ps := make([]Point, len(totals))
	for i, v := range totals {
		ps[i] = Point{Time: t0.Add(time.Duration(i) * 10 * time.Second), Total: v}
	}
	return ps
}

func TestMaxRise(t *testing.T) {
	cases := []struct {
		totals []uint64
		want   uint64
	}{
		{[]uint64{5, 5, 5}, 0},
		{[]uint64{10, 3, 9, 4, 12}, 9},
		{[]uint64{1, 2, 3, 4, 5, 6, 7, 8}, 6},
		{[]uint64{9, 8, 7}, 0},
	}
	for _, c := range cases {
		if got := maxRise(points(c.totals...)); got != c.want {
			t.Errorf("maxRise(%v) = %d, want %d", c.totals, got, c.want)
		}
	}
}

func TestGaps(t *testing.T) {
	ps := points(1, 1, 1, 1, 1)
	if g := gaps(ps); g != 0 {
		t.Errorf("regular series has gaps %v", g)
	}
	ps[4].Time = ps[3].Time.Add(10 * time.Minute)
	if g := gaps(ps); g != 10*time.Minute-10*time.Second {
		t.Errorf("gap = %v", g)
	}
}

func TestQuantile(t *testing.T) {
	vals := []uint64{5, 1, 4, 2, 3}
	for q, want := range map[float64]uint64{0: 1, 0.2: 1, 0.5: 3, 0.99: 5, 1: 5} {
		if got := Quantile(vals, q); got != want {
			t.Errorf("Quantile(%v) = %d, want %d", q, got, want)
		}
	}
	if vals[0] != 5 {
		t.Error("Quantile reordered its input")
	}
	if Quantile(nil, 0.5) != 0 {
		t.Error("Quantile of nothing is not zero")
	}
}

func TestSummarize(t *testing.T) {
	d := NewDeriver(4096)
	var samples []Sample
	var pts []Point
	for i := range 31 {
		u := usage(GiB+uint64(i)*MiB, 0, 100*MiB, 100*MiB)
		u.OOMKills = uint64(i / 10)
		u.Peak, u.HasPeak = 3*GiB, true
		s := Sample{
			Time:      t0.Add(time.Duration(i) * 2 * time.Second),
			Box:       BoxUsage{CPUBusy: uint64(i) * 100, CPUTotal: uint64(i) * 400, OOMKills: uint64(i / 10), HasPressure: true},
			Workloads: map[string]Usage{"/app.service": u},
		}
		samples = append(samples, s)
		pts = append(pts, d.Add(s))
	}
	sum := Summarize(samples, pts, 4, 4096)
	w := sum.Workloads["/app.service"]
	if sum.Samples != 31 || sum.Window() != time.Minute || sum.Coverage() != 1 {
		t.Errorf("samples %d, window %v, coverage %v", sum.Samples, sum.Window(), sum.Coverage())
	}
	if w.HardMax != GiB+30*MiB || w.WorkingSetMax != GiB+130*MiB || w.OOMKills != 3 || sum.OOMKills != 3 {
		t.Errorf("workload %+v, box kills %d", w, sum.OOMKills)
	}
	if sum.CPUQ95 != 1 {
		t.Errorf("CPU = %v cores, want 1", sum.CPUQ95)
	}
	if sum.PeakCurrentMax != GiB+30*MiB+200*MiB {
		t.Errorf("peak current max = %d", sum.PeakCurrentMax)
	}
	if sum.UncalibratedHotQ99 != 100*MiB {
		t.Errorf("uncalibrated hot = %d MiB", sum.UncalibratedHotQ99/MiB)
	}
}
