package model

import (
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"
)

// genSeries draws a gap-free series of samples for up to four workloads.
func genSeries(t *rapid.T, n int, step time.Duration) []Sample {
	ids := []string{"/a.service", "/b.service", "/system.slice/docker-1.scope", "/d.scope"}
	nw := rapid.IntRange(1, len(ids)).Draw(t, "workloads")
	out := make([]Sample, n)
	var refault [4]uint64
	var pressure [4]time.Duration
	for i := range n {
		s := Sample{Time: t0.Add(time.Duration(i) * step), Workloads: make(map[string]Usage, nw)}
		s.Box = BoxUsage{
			Slab:        rapid.Uint64Range(0, 512*MiB).Draw(t, "slab"),
			PageTables:  rapid.Uint64Range(0, 64*MiB).Draw(t, "pt"),
			KernelStack: rapid.Uint64Range(0, 16*MiB).Draw(t, "ks"),
		}
		for w := range nw {
			refault[w] += rapid.Uint64Range(0, 5000).Draw(t, "refault")
			pressure[w] += time.Duration(rapid.Int64Range(0, 1000).Draw(t, "pressure")) * time.Microsecond
			u := Usage{
				Anon:              rapid.Uint64Range(0, 8*GiB).Draw(t, "anon"),
				Shmem:             rapid.Uint64Range(0, GiB).Draw(t, "shmem"),
				ActiveFile:        rapid.Uint64Range(0, 4*GiB).Draw(t, "active"),
				InactiveFile:      rapid.Uint64Range(0, 4*GiB).Draw(t, "inactive"),
				SlabUnreclaimable: rapid.Uint64Range(0, 64*MiB).Draw(t, "sunreclaim"),
				Sock:              rapid.Uint64Range(0, 256*MiB).Draw(t, "sock"),
				RefaultFile:       refault[w],
				PressureSome:      pressure[w],
			}
			u.Current = u.Hard() + u.ActiveFile + u.InactiveFile
			s.Workloads[ids[w]] = u
		}
		out[i] = s
	}
	return out
}

func derive(samples []Sample) []Point {
	d := NewDeriver(4096)
	pts := make([]Point, len(samples))
	for i, s := range samples {
		pts[i] = d.Add(s)
	}
	return pts
}

// The snug size never falls below what the box can't give back: the highest
// hard memory total with the minimum headroom and cache on top, and the 99.9th
// percentile of the working set.
func TestPropertySnugCoversHardPeak(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		samples := genSeries(t, rapid.IntRange(1, 60).Draw(t, "n"), 2*time.Second)
		pts := derive(samples)
		sum := Summarize(samples, pts, 2, 4096)
		e := Size(sum, History{Now: t0})
		var hardMax uint64
		totals := make([]uint64, len(pts))
		for i, p := range pts {
			hardMax = max(hardMax, p.HardTotal)
			totals[i] = p.Total
		}
		if e.Snug < hardMax+MinHeadroom+MinCache {
			t.Fatalf("snug %d below hard peak %d plus minimums", e.Snug, hardMax)
		}
		if e.Snug < Quantile(totals, NeedQuantile) {
			t.Fatalf("snug %d below the 99.9th percentile of the working set", e.Snug)
		}
		if e.Snug != e.Need+e.Headroom+e.Cache {
			t.Fatalf("snug %d is not need %d + headroom %d + cache %d", e.Snug, e.Need, e.Headroom, e.Cache)
		}
	})
}

// Extending a gap-free series with more gap-free samples never lowers the
// confidence that comes from the window. The cache rule can still lower the
// final level by one when an unpressured workload's cache grows, which is
// checked as a separate rule.
func TestPropertyMoreDataNeverLowersConfidence(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		step := time.Duration(rapid.IntRange(1, 48).Draw(t, "hours")) * time.Hour
		n := rapid.IntRange(2, 40).Draw(t, "n")
		extra := rapid.IntRange(1, 40).Draw(t, "extra")
		samples := genSeries(t, n+extra, step)
		est := func(s []Sample) Estimate {
			return Size(Summarize(s, derive(s), 2, 4096), History{Now: s[len(s)-1].Time})
		}
		short, long := est(samples[:n]), est(samples)
		ws, wl := windowConfidence(short.Window, short.Coverage), windowConfidence(long.Window, long.Coverage)
		if wl < ws {
			t.Fatalf("window confidence fell from %v to %v with more data", ws, wl)
		}
		for _, e := range []Estimate{short, long} {
			want := windowConfidence(e.Window, e.Coverage)
			if e.CacheOverestimate && want > ConfidenceLow {
				want--
			}
			if e.Confidence != want {
				t.Fatalf("confidence %v, want %v from the window and the cache rule", e.Confidence, want)
			}
		}
	})
}

// Quantiles stay inside the data and grow with q.
func TestPropertyQuantile(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		vals := rapid.SliceOfN(rapid.Uint64(), 1, 200).Draw(t, "vals")
		qa := rapid.Float64Range(0, 1).Draw(t, "qa")
		qb := rapid.Float64Range(qa, 1).Draw(t, "qb")
		a, b := Quantile(vals, qa), Quantile(vals, qb)
		if a > b || a < slices.Min(vals) || b > slices.Max(vals) {
			t.Fatalf("Quantile(%v)=%d, Quantile(%v)=%d over [%d, %d]", qa, a, qb, b, slices.Min(vals), slices.Max(vals))
		}
	})
}

// The largest 60-second rise is never more than the spread of the series and
// matches a brute-force search.
func TestPropertyMaxRise(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		totals := rapid.SliceOfN(rapid.Uint64Range(0, 1<<40), 1, 80).Draw(t, "totals")
		step := time.Duration(rapid.IntRange(1, 30).Draw(t, "step")) * time.Second
		ps := make([]Point, len(totals))
		for i, v := range totals {
			ps[i] = Point{Time: t0.Add(time.Duration(i) * step), Total: v}
		}
		var want uint64
		for i := range ps {
			for j := i + 1; j < len(ps) && ps[j].Time.Sub(ps[i].Time) <= RiseWindow; j++ {
				if ps[j].Total > ps[i].Total {
					want = max(want, ps[j].Total-ps[i].Total)
				}
			}
		}
		if got := maxRise(ps); got != want {
			t.Fatalf("maxRise = %d, brute force = %d", got, want)
		}
	})
}
