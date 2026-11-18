package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"

	"snugbox.dev/snugbox/agent/internal/model"
)

// refSummary is Summary as it was before it moved its sums and maxima into
// SQL and kept whole days in a cache: every minute row and every hourly row
// read one at a time. It stays here as the reference the faster one must
// match exactly.
func (s *Store) refSummary(ctx context.Context, from, to time.Time) (model.Summary, map[string]model.Workload, error) {
	var sum model.Summary
	if err := s.refBoxMinutes(ctx, &sum, from, to); err != nil {
		return sum, nil, err
	}
	if sum.Samples == 0 {
		return sum, map[string]model.Workload{}, nil
	}
	hourFrom := from.Truncate(time.Hour).Unix()
	if err := s.refBoxHours(ctx, &sum, hourFrom, to.Unix()); err != nil {
		return sum, nil, err
	}
	ws, err := s.refWorkloadHours(ctx, &sum, hourFrom, to.Unix())
	if err != nil {
		return sum, nil, err
	}
	return sum, ws, nil
}

func (s *Store) refBoxHours(ctx context.Context, sum *model.Summary, from, to int64) error {
	rows, err := s.db.QueryContext(ctx, "SELECT total, hot, uncal, kother, cpu FROM box_1h WHERE t >= ? AND t <= ?", from, to)
	if err != nil {
		return err
	}
	defer rows.Close()
	m := merged{newSketch(), newSketch(), newSketch(), newSketch(), newSketch()}
	for rows.Next() {
		var total, hot, uncal, kother, cpu []byte
		if err := rows.Scan(&total, &hot, &uncal, &kother, &cpu); err != nil {
			return err
		}
		for _, p := range []struct {
			s *ddsketch.DDSketch
			b []byte
		}{{m.total, total}, {m.hot, hot}, {m.uncal, uncal}, {m.kother, kother}, {m.cpu, cpu}} {
			if err := mergeStored(p.s, p.b); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sum.TotalQ999 = upperQuantile(m.total, model.NeedQuantile)
	sum.HotFileQ99 = upperQuantile(m.hot, model.HotFileQuantile)
	sum.UncalibratedHotQ99 = upperQuantile(m.uncal, model.HotFileQuantile)
	sum.CPUQ95 = upperQuantileFloat(m.cpu, 0.95)
	sum.KernelOther = model.WorkloadSummary{
		Samples:        int(m.kother.GetCount()),
		WorkingSetQ95:  upperQuantile(m.kother, 0.95),
		WorkingSetQ99:  upperQuantile(m.kother, 0.99),
		WorkingSetQ999: upperQuantile(m.kother, 0.999),
		WorkingSetMax:  upperQuantile(m.kother, 1),
		HardMax:        upperQuantile(m.kother, 1),
	}
	return nil
}

func (s *Store) refBoxMinutes(ctx context.Context, sum *model.Summary, from, to time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT t, samples, hard_max, total_max, cur_sum_max, peak_cur_max, rise_max, psi_full_us, has_psi,
		swapin_intervals, intervals, swapin_bytes, oom_kills FROM box_1m WHERE t >= ? AND t <= ? ORDER BY t`, from.Unix(), to.Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	var prev int64
	first := true
	for rows.Next() {
		var t, samples, psi, hasPSI, swInt, intervals int64
		var hard, total, cur, peak, rise, swBytes, kills uint64
		if err := rows.Scan(&t, &samples, &hard, &total, &cur, &peak, &rise, &psi, &hasPSI, &swInt, &intervals, &swBytes, &kills); err != nil {
			return err
		}
		if first {
			sum.Start, sum.HasPressure, first = time.Unix(t, 0).UTC(), hasPSI == 1, false
		} else if gap := time.Duration(t-prev) * time.Second; gap > 3*time.Minute {
			sum.Gaps += gap - time.Minute
		}
		prev = t
		sum.End = time.Unix(t, 0).UTC().Add(time.Minute)
		sum.Samples += int(samples)
		sum.HardTotalMax = max(sum.HardTotalMax, hard)
		sum.TotalMax = max(sum.TotalMax, total)
		sum.CurrentSumMax = max(sum.CurrentSumMax, cur)
		sum.PeakCurrentMax = max(sum.PeakCurrentMax, peak)
		sum.MaxRise = max(sum.MaxRise, rise)
		sum.PressureFull += time.Duration(psi) * time.Microsecond
		sum.HasPressure = sum.HasPressure && hasPSI == 1
		sum.SwapInIntervals += int(swInt)
		sum.Intervals += int(intervals)
		sum.SwapInBytes += swBytes
		sum.OOMKills += kills
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if sum.End.After(to) {
		sum.End = to
	}
	return nil
}

// refMerge is one workload's sums in refWorkloadHours.
type refMerge struct {
	w       model.Workload
	ws, hot *ddsketch.DDSketch
	sum     model.WorkloadSummary
	noCPU   bool
}

func (s *Store) refWorkloadHours(ctx context.Context, sum *model.Summary, from, to int64) (map[string]model.Workload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.key, w.kind, w.name, w.info, w.peak, w.first_seen, w.last_seen, h.samples, h.ws, h.hot, h.ws_max, h.hard_max, h.cur_max,
			h.shmem_max, h.sock_max, h.pressured, h.oom_kills, h.cpu_us,
			EXISTS (SELECT 1 FROM wl_10s p WHERE p.wid = w.id AND p.t = (SELECT MAX(t) FROM box_10s WHERE t BETWEEN ? AND ?))
		FROM wl_1h h JOIN workload w ON w.id = h.wid WHERE h.t >= ? AND h.t <= ?`, from, to, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	acc := make(map[string]*refMerge)
	for rows.Next() {
		var key, kind, name, info string
		var peak, cpuUS *int64
		var firstSeen, lastSeen int64
		var samples, pressured int
		var ws, hot []byte
		var wsMax, hardMax, curMax, shmemMax, sockMax, kills uint64
		var present bool
		if err := rows.Scan(&key, &kind, &name, &info, &peak, &firstSeen, &lastSeen, &samples, &ws, &hot, &wsMax, &hardMax, &curMax, &shmemMax, &sockMax, &pressured, &kills, &cpuUS, &present); err != nil {
			return nil, err
		}
		a := acc[key]
		if a == nil {
			w, err := decodeWorkload(key, kind, name, info)
			if err != nil {
				return nil, err
			}
			a = &refMerge{w: w, ws: newSketch(), hot: newSketch()}
			if peak != nil && *peak >= 0 {
				a.sum.Peak, a.sum.HasPeak = uint64(*peak), true
				a.sum.PeakAtEnd = present && lastSeen <= to
			}
			a.sum.Observed = time.Duration(max(min(to, lastSeen)-max(from, firstSeen), 0)) * time.Second
			acc[key] = a
		}
		if err := mergeStored(a.ws, ws); err != nil {
			return nil, err
		}
		if err := mergeStored(a.hot, hot); err != nil {
			return nil, err
		}
		a.sum.Samples += samples
		a.sum.WorkingSetMax = max(a.sum.WorkingSetMax, wsMax)
		a.sum.HardMax = max(a.sum.HardMax, hardMax)
		a.sum.CurrentMax = max(a.sum.CurrentMax, curMax)
		a.sum.ShmemMax = max(a.sum.ShmemMax, shmemMax)
		a.sum.SockMax = max(a.sum.SockMax, sockMax)
		a.sum.Pressured = a.sum.Pressured || pressured == 1
		a.sum.OOMKills += kills
		if cpuUS != nil && *cpuUS >= 0 {
			a.sum.CPU += time.Duration(*cpuUS) * time.Microsecond
		} else {
			a.noCPU = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sum.Workloads = make(map[string]model.WorkloadSummary, len(acc))
	ws := make(map[string]model.Workload, len(acc))
	for key, a := range acc {
		a.sum.WorkingSetQ95 = min(upperQuantile(a.ws, 0.95), a.sum.WorkingSetMax)
		a.sum.WorkingSetQ99 = min(upperQuantile(a.ws, 0.99), a.sum.WorkingSetMax)
		a.sum.WorkingSetQ999 = min(upperQuantile(a.ws, 0.999), a.sum.WorkingSetMax)
		a.sum.HotFileQ99 = upperQuantile(a.hot, model.HotFileQuantile)
		a.sum.HasCPU = !a.noCPU
		sum.Workloads[key] = a.sum
		ws[key] = a.w
	}
	return ws, nil
}

// Over several days the summary comes partly from the day cache: it still
// gives exactly what the reference gives, for windows that cut through
// days, start at midnight, end just after midnight before the day before
// has been cached, and for a gap that spans a midnight. A second summary of
// the same window, read from the cache, gives the same again.
func TestSummaryMatchesReferenceOverDays(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	start := time.Date(2026, 10, 27, 5, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 30, 13, 0, 0, 0, time.UTC)
	app, early, late := "/system.slice/app.service", "/system.slice/early.service", "/system.slice/late.service"
	ws := map[string]model.Workload{
		app:   {ID: app, Kind: model.KindService, Name: "app.service"},
		early: {ID: early, Kind: model.KindService, Name: "early.service"},
		late:  {ID: late, Kind: model.KindService, Name: "late.service"},
	}
	gapFrom, gapTo := time.Date(2026, 10, 28, 23, 50, 0, 0, time.UTC), time.Date(2026, 10, 29, 0, 20, 0, 0, time.UTC)
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	n := 0
	for at := start; !at.After(end); at = at.Add(2 * time.Minute) {
		n++
		if !at.Before(gapFrom) && at.Before(gapTo) {
			continue
		}
		wave := uint64(n%29) * 2 * mib
		smp := model.Sample{
			Time: at,
			Box: model.BoxUsage{MemTotal: 4 << 30, MemAvailable: 3 << 30, Slab: 50 * mib, HasPressure: at.Hour() != 3,
				PressureFull: time.Duration(n) * time.Millisecond, SwapIns: uint64(n / 7), OOMKills: uint64(n / 900), CPUBusy: uint64(n) * 40, CPUTotal: uint64(n) * 100},
			Workloads: map[string]model.Usage{
				app: {Current: 300*mib + wave, Anon: 200*mib + wave, ActiveFile: 30 * mib, Peak: 1 << 30, HasPeak: true, CPU: time.Duration(n) * time.Millisecond, HasCPU: true},
			},
		}
		if at.Before(time.Date(2026, 10, 28, 6, 0, 0, 0, time.UTC)) {
			smp.Workloads[early] = model.Usage{Current: 80 * mib, Anon: 60 * mib, CPU: time.Duration(n) * time.Millisecond, HasCPU: true}
		}
		if at.After(time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)) {
			smp.Workloads[late] = model.Usage{Current: 120*mib + wave, Anon: 100 * mib, OOMKills: uint64(n / 1500), CPU: time.Duration(n) * time.Millisecond, HasCPU: true}
		}
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// An hour inside a cached day that has no CPU figure, as an hour
	// recorded before version 3 has.
	if _, err := s.db.ExecContext(ctx, `UPDATE wl_1h SET cpu_us = NULL WHERE t = ?
		AND wid = (SELECT id FROM workload WHERE key = ?)`, time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC).Unix(), app); err != nil {
		t.Fatal(err)
	}
	day := func(dd, hh, mm int) time.Time { return time.Date(2026, 10, dd, hh, mm, 0, 0, time.UTC) }
	for _, w := range []struct{ from, to time.Time }{
		{start, end},
		{day(27, 12, 7), day(30, 12, 31)},
		{day(28, 0, 0), day(30, 13, 0)},
		{day(27, 9, 0), day(30, 0, 30)},
		{day(27, 9, 0), day(30, 1, 30)},
		{day(28, 22, 0), day(30, 4, 0)},
		{day(28, 3, 0), day(29, 3, 0)},
	} {
		t.Run(w.from.Format("02T15:04")+"-"+w.to.Format("02T15:04"), func(t *testing.T) {
			want, wantWS, err := s.refSummary(ctx, w.from, w.to)
			if err != nil {
				t.Fatal(err)
			}
			for pass := range 2 {
				got, gotWS, err := s.Summary(ctx, w.from, w.to)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("pass %d: summary\n%+v\nwant\n%+v", pass, got, want)
				}
				if !reflect.DeepEqual(gotWS, wantWS) {
					t.Errorf("pass %d: workloads\n%+v\nwant\n%+v", pass, gotWS, wantWS)
				}
			}
		})
	}
	// The fixture has what the windows are meant to cover.
	full, _, err := s.refSummary(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	if full.Gaps == 0 || full.HasPressure || full.Workloads[app].HasCPU || !full.Workloads[late].HasCPU {
		t.Errorf("the fixture lacks a case: gaps %v, pressure %v, app %+v", full.Gaps, full.HasPressure, full.Workloads[app])
	}
	// Rows a trim takes from a cached day are gone from the summary once
	// the cache is cleared, as Prune clears it after a trim.
	from, to := day(27, 9, 0), day(30, 4, 0)
	if _, _, err := s.Summary(ctx, from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM wl_1h WHERE t < ?", day(28, 12, 0).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM box_1m WHERE t < ?", day(28, 12, 0).Unix()); err != nil {
		t.Fatal(err)
	}
	s.days.clear()
	want, wantWS, err := s.refSummary(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	got, gotWS, err := s.Summary(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotWS, wantWS) {
		t.Errorf("after a trim and a cleared cache\n%+v\nwant\n%+v", got, want)
	}
}

// Summary gives exactly what the row-by-row reference gives, on a history
// with gaps, minutes without pressure figures, a workload that came late and
// left early, a workload without memory.peak, an hour without CPU time, and
// windows that start and end in the middle of an hour.
func TestSummaryMatchesReference(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	samples, ws := series(1080)
	cron := "/system.slice/cron-job.service"
	ws[cron] = model.Workload{ID: cron, Kind: model.KindService, Name: "cron-job.service", Programs: []string{"python3"}}
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	for i, smp := range samples {
		if i >= 400 && i < 450 {
			continue // a gap of over eight minutes
		}
		if i >= 800 && i < 820 {
			smp.Box.HasPressure = false
		}
		if i >= 300 && i < 700 {
			smp.Workloads[cron] = model.Usage{Current: uint64(50+i%20) * mib, Anon: 40 * mib, ActiveFile: 5 * mib, CPU: time.Duration(i) * time.Millisecond, HasCPU: true}
		}
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE wl_1h SET cpu_us = NULL WHERE t = (SELECT MIN(t) FROM wl_1h)
		AND wid = (SELECT id FROM workload WHERE key = '/system.slice/db.service')`); err != nil {
		t.Fatal(err)
	}
	end := samples[len(samples)-1].Time
	for _, w := range []struct{ from, to time.Time }{
		{t0, end},
		{t0.Add(25 * time.Minute), t0.Add(2*time.Hour + 10*time.Minute)},
		{t0.Add(70 * time.Minute), t0.Add(80 * time.Minute)},
		{end.Add(time.Hour), end.Add(2 * time.Hour)},
	} {
		t.Run(fmt.Sprintf("%s-%s", w.from.Format("15:04"), w.to.Format("15:04")), func(t *testing.T) {
			want, wantWS, err := s.refSummary(ctx, w.from, w.to)
			if err != nil {
				t.Fatal(err)
			}
			got, gotWS, err := s.Summary(ctx, w.from, w.to)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("summary\n%+v\nwant\n%+v", got, want)
			}
			if !reflect.DeepEqual(gotWS, wantWS) {
				t.Errorf("workloads\n%+v\nwant\n%+v", gotWS, wantWS)
			}
		})
	}
	// The fixture has what the subtests are meant to cover.
	full, _, err := s.refSummary(ctx, t0, end)
	if err != nil {
		t.Fatal(err)
	}
	if full.Gaps == 0 || full.HasPressure || full.Workloads["/system.slice/db.service"].HasCPU || !full.Workloads[cron].HasCPU ||
		full.Workloads["/system.slice/db.service"].HasPeak || full.Workloads[cron].Observed >= full.Window() {
		t.Errorf("the fixture lacks a case: %+v", full)
	}
}
