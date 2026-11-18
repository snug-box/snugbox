package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

const mib = model.MiB

var t0 = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "snugbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// series makes n samples 10 seconds apart for two workloads whose memory
// moves in a known pattern.
func series(n int) ([]model.Sample, map[string]model.Workload) {
	ws := map[string]model.Workload{
		"/system.slice/app.service": {ID: "/system.slice/app.service", Kind: model.KindService, Name: "app.service", Programs: []string{"node"}},
		"/system.slice/db.service":  {ID: "/system.slice/db.service", Kind: model.KindService, Name: "db.service", Database: "PostgreSQL"},
	}
	out := make([]model.Sample, n)
	for i := range n {
		wave := uint64(i%37) * 3 * mib
		out[i] = model.Sample{
			Time: t0.Add(time.Duration(i) * 10 * time.Second),
			Box: model.BoxUsage{
				MemTotal: 8 << 30, MemAvailable: 6 << 30, Slab: 120 * mib, PageTables: 10 * mib, KernelStack: 4 * mib,
				HasPressure: true, PressureFull: time.Duration(i) * time.Millisecond, SwapIns: uint64(i / 3), OOMKills: uint64(i / 500),
				CPUBusy: uint64(i) * 30, CPUTotal: uint64(i) * 100,
			},
			Workloads: map[string]model.Usage{
				"/system.slice/app.service": {Current: 900*mib + wave, Anon: 700*mib + wave, ActiveFile: 100 * mib, InactiveFile: 100 * mib, Peak: 2 << 30, HasPeak: true, OOMKills: uint64(i / 500)},
				"/system.slice/db.service":  {Current: 600 * mib, Anon: 200 * mib, Shmem: 128 * mib, ActiveFile: 172 * mib, InactiveFile: 100 * mib, RefaultFile: uint64(i), PressureSome: time.Duration(i) * time.Microsecond},
			},
		}
	}
	return out, ws
}

// The summary built from the store matches the exact summary of the same
// samples within the sketches' error bound, and never falls below it.
func TestSummaryMatchesExact(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	samples, ws := series(1080) // three hours
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	points := make([]model.Point, len(samples))
	for i, smp := range samples {
		points[i] = d.Add(smp)
		if err := rec.Add(ctx, smp, points[i], ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	exact := model.Summarize(samples, points, 2, 4096)
	got, gotWs, err := s.Summary(ctx, t0, samples[len(samples)-1].Time)
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != exact.Samples {
		t.Errorf("samples %d, want %d", got.Samples, exact.Samples)
	}
	if got.Gaps != 0 || got.Start != t0 {
		t.Errorf("start %v, gaps %v", got.Start, got.Gaps)
	}
	for name, pair := range map[string][2]uint64{
		"hard max":      {got.HardTotalMax, exact.HardTotalMax},
		"rise":          {got.MaxRise, exact.MaxRise},
		"cur sum max":   {got.CurrentSumMax, exact.CurrentSumMax},
		"peak cur max":  {got.PeakCurrentMax, exact.PeakCurrentMax},
		"swap-in bytes": {got.SwapInBytes, exact.SwapInBytes},
		"oom kills":     {got.OOMKills, exact.OOMKills},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: store %d, exact %d", name, pair[0], pair[1])
		}
	}
	within := func(name string, store, exact uint64) {
		t.Helper()
		if store < exact || float64(store) > float64(exact)*(1+3*SketchAccuracy) {
			t.Errorf("%s: store %d, exact %d; want at or above exact, within the error bound", name, store, exact)
		}
	}
	within("total p99.9", got.TotalQ999, exact.TotalQ999)
	within("hot p99", got.HotFileQ99, exact.HotFileQ99)
	app, exApp := got.Workloads["/system.slice/app.service"], exact.Workloads["/system.slice/app.service"]
	within("app p99.9", app.WorkingSetQ999, exApp.WorkingSetQ999)
	if app.WorkingSetMax != exApp.WorkingSetMax || app.HardMax != exApp.HardMax || !app.HasPeak || app.Peak != 2<<30 || app.OOMKills != exApp.OOMKills {
		t.Errorf("app summary %+v, exact %+v", app, exApp)
	}
	db := got.Workloads["/system.slice/db.service"]
	if !db.Pressured || db.ShmemMax != 128*mib {
		t.Errorf("db summary %+v", db)
	}
	if got.SwapInIntervals != exact.SwapInIntervals || got.Intervals != exact.Intervals {
		t.Errorf("swap-in intervals %d of %d, exact %d of %d", got.SwapInIntervals, got.Intervals, exact.SwapInIntervals, exact.Intervals)
	}
	if math.Abs(got.CPUQ95-exact.CPUQ95) > 0.05 {
		t.Errorf("cpu p95 %v, exact %v", got.CPUQ95, exact.CPUQ95)
	}
	if gotWs["/system.slice/db.service"].Database != "PostgreSQL" || gotWs["/system.slice/app.service"].Programs[0] != "node" {
		t.Errorf("workloads %+v", gotWs)
	}
	e1, e2 := model.Size(got, model.History{Now: got.End}), model.Size(exact, model.History{Now: got.End})
	if e1.Snug < e2.Snug {
		t.Errorf("snug from the store %d is below the exact %d", e1.Snug, e2.Snug)
	}
}

func TestGapsAndRestart(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	samples, ws := series(400)
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	for i, smp := range samples {
		if i == 200 {
			if err := rec.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			rec = s.NewRecorder(2, 4096)
		}
		if i >= 200 {
			smp.Time = smp.Time.Add(2 * time.Hour)
		}
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Summary(ctx, t0, t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Samples != 400 {
		t.Errorf("samples %d, want 400 across the restart", got.Samples)
	}
	if got.Gaps < 110*time.Minute || got.Gaps > 125*time.Minute {
		t.Errorf("gaps %v, want about 2 hours", got.Gaps)
	}
}

func TestSnapshotsEventsMeta(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	e := model.Estimate{Need: 2 << 30, Headroom: 256 * mib, Cache: 256 * mib, Snug: 2560 * mib, Confidence: model.ConfidenceLow, Window: time.Hour}
	if added, err := s.AddSnapshot(ctx, t0, e); err != nil || !added {
		t.Fatalf("first snapshot: %v, %v", added, err)
	}
	if added, err := s.AddSnapshot(ctx, t0.Add(5*time.Minute), e); err != nil || added {
		t.Errorf("an unchanged estimate was stored again: %v, %v", added, err)
	}
	e.Confidence = model.ConfidenceMedium
	if added, err := s.AddSnapshot(ctx, t0.Add(7*24*time.Hour), e); err != nil || !added {
		t.Errorf("a new confidence was not stored: %v, %v", added, err)
	}
	snaps, err := s.Snapshots(ctx, t0)
	if err != nil || len(snaps) != 2 || snaps[1].Confidence != "medium" || snaps[0].Snug != 2560*mib {
		t.Errorf("snapshots %+v, %v", snaps, err)
	}
	for i, kind := range []string{"oom_kill", "warning"} {
		if err := s.AddEvent(ctx, Event{Time: t0.Add(time.Duration(i) * time.Minute), Kind: kind, Workload: "app.service", Detail: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.Events(ctx, t0, 10)
	if err != nil || len(evs) != 2 || evs[0].Kind != "warning" {
		t.Errorf("events %+v, %v", evs, err)
	}
	if err := s.SetMeta(ctx, "paused", "1"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Meta(ctx, "paused"); err != nil || v != "1" {
		t.Errorf("meta %q, %v", v, err)
	}
	if v, err := s.Meta(ctx, "missing"); err != nil || v != "" {
		t.Errorf("missing meta %q, %v", v, err)
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	samples, ws := series(60)
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	for _, smp := range samples {
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, t0.Add(7*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var fine, wlFine, minutes int
	for q, dst := range map[string]*int{"SELECT COUNT(*) FROM box_10s": &fine, "SELECT COUNT(*) FROM wl_10s": &wlFine, "SELECT COUNT(*) FROM box_1m": &minutes} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if fine != 60 || wlFine != 0 || minutes != 10 {
		t.Errorf("after 7 hours: %d box points (keep 48 h), %d workload points (keep 6 h), %d minutes", fine, wlFine, minutes)
	}
	if err := s.Prune(ctx, t0.Add(31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM box_1m").Scan(&minutes); err != nil || minutes != 0 {
		t.Errorf("minutes after 31 days: %d, %v", minutes, err)
	}
	if size, err := s.Size(ctx); err != nil || size <= 0 || size > MaxBytes {
		t.Errorf("size %d, %v", size, err)
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snugbox.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := s.Meta(ctx, "k"); err != nil || v != "v" {
		t.Errorf("after reopening: %q, %v", v, err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err == nil {
		t.Error("a database from a newer release was accepted")
	}
}

// The CPU time a workload used adds up across the hours it was recorded in,
// matching the exact summary, and a workload whose cgroup never gave a CPU
// figure has none. Observed is how long it was there.
func TestCPUAndObserved(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	samples, ws := series(1080)
	for i := range samples {
		u := samples[i].Workloads["/system.slice/app.service"]
		u.CPU, u.HasCPU = time.Duration(i)*100*time.Millisecond, true
		samples[i].Workloads["/system.slice/app.service"] = u
	}
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	points := make([]model.Point, len(samples))
	for i, smp := range samples {
		points[i] = d.Add(smp)
		if err := rec.Add(ctx, smp, points[i], ws); err != nil {
			t.Fatal(err)
		}
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	end := samples[len(samples)-1].Time
	got, _, err := s.Summary(ctx, t0, end)
	if err != nil {
		t.Fatal(err)
	}
	exact := model.Summarize(samples, points, 2, 4096)
	app, want := got.Workloads["/system.slice/app.service"], exact.Workloads["/system.slice/app.service"]
	if !app.HasCPU || app.CPU != want.CPU || want.CPU != 1079*100*time.Millisecond {
		t.Errorf("app CPU %v (has %v) from the store, %v exact; want 1m47.9s", app.CPU, app.HasCPU, want.CPU)
	}
	if db := got.Workloads["/system.slice/db.service"]; db.HasCPU || exact.Workloads["/system.slice/db.service"].HasCPU {
		t.Error("a workload without a CPU figure has one")
	}
	// The store knows when a workload was last seen to within a minute.
	if diff := want.Observed - app.Observed; want.Observed != end.Sub(t0) || diff < 0 || diff > time.Minute {
		t.Errorf("observed %v in the store, %v exact; want %v, the store at most a minute short", app.Observed, want.Observed, end.Sub(t0))
	}
}
