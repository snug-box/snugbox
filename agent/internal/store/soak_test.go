package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

// soakWorkloads is the number of workloads blueprint section 9.4 sizes the
// budgets for. They are named like test/budget's services, so a database
// this test keeps can stand in for their history.
const soakWorkloads = 50

// TestThirtyDaysOfFiftyWorkloads fills a store the way the watcher does over
// 30 days on a box with 50 workloads, pruning every hour as it does: a
// sample a minute for the first 28 days and every 10 seconds for the last
// two, which is all the 10-second rows the store keeps. Then the store must
// be under its cap (blueprint section 5.3), and a 30-day summary must be
// small enough for the watcher's memory budget. It takes minutes, so it
// runs only with SNUGBOX_SOAK=1; SNUGBOX_SOAK_DB=<path> keeps the database,
// with history ending now, for test/budget on a full history.
func TestThirtyDaysOfFiftyWorkloads(t *testing.T) {
	if os.Getenv("SNUGBOX_SOAK") == "" {
		t.Skip("set SNUGBOX_SOAK=1 to fill 30 days of history; it takes minutes")
	}
	ctx := context.Background()
	path := os.Getenv("SNUGBOX_SOAK_DB")
	if path == "" {
		path = filepath.Join(t.TempDir(), "snugbox.db")
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})

	ws := make(map[string]model.Workload, soakWorkloads)
	for i := 1; i <= soakWorkloads; i++ {
		id := fmt.Sprintf("/system.slice/budget-load-%d.service", i)
		ws[id] = model.Workload{ID: id, Kind: model.KindService, Name: fmt.Sprintf("budget-load-%d.service", i), Programs: []string{"sleep"}}
	}
	end := time.Now().UTC().Truncate(10 * time.Second)
	start := end.Add(-30 * 24 * time.Hour)
	rec := s.NewRecorder(2, 4096)
	d := model.NewDeriver(4096)
	began, n := time.Now(), 0
	lastPrune := start
	for at := start; !at.After(end); n++ {
		smp := soakSample(at, n)
		if err := rec.Add(ctx, smp, d.Add(smp), ws); err != nil {
			t.Fatal(err)
		}
		if at.Sub(lastPrune) >= time.Hour {
			if err := s.Prune(ctx, at); err != nil {
				t.Fatal(err)
			}
			lastPrune = at
		}
		step := time.Minute
		if end.Sub(at) <= BoxFineRetention {
			step = 10 * time.Second
		}
		at = at.Add(step)
	}
	if err := rec.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(ctx, end); err != nil {
		t.Fatal(err)
	}
	size, err := s.Size(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d samples of %d workloads in %s; the store holds %.1f MiB", n, soakWorkloads, time.Since(began).Round(time.Second), float64(size)/(1<<20))
	if size > MaxBytes {
		t.Errorf("the store holds %d bytes, over its %d cap", size, MaxBytes)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	took := time.Now()
	sum, wls, err := s.Summary(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(took)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	runtime.GC()
	runtime.ReadMemStats(&after)
	kept := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(sum)
	runtime.KeepAlive(wls)
	t.Logf("a 30-day summary took %s, allocated %.1f MiB on the way and keeps %.2f MiB", elapsed.Round(time.Millisecond),
		float64(allocated)/(1<<20), float64(kept)/(1<<20))
	if len(sum.Workloads) != soakWorkloads {
		t.Errorf("the summary covers %d workloads, want %d", len(sum.Workloads), soakWorkloads)
	}
	if got := sum.Window(); got < 29*24*time.Hour {
		t.Errorf("the summary covers %s, want about 30 days", got)
	}
	// snugboxd runs with GOMEMLIMIT=20MiB; what a summary keeps has to fit
	// beside everything else in it with room to spare.
	if kept > 4<<20 {
		t.Errorf("a 30-day summary keeps %.1f MiB, over 4 MiB", float64(kept)/(1<<20))
	}
}

// soakSample is a box with 50 sleeping services whose memory moves slowly
// over the day, as test/budget's do, with a little of everything the
// recorder keeps.
func soakSample(at time.Time, n int) model.Sample {
	const mib = model.MiB
	day := uint64(at.Hour())
	smp := model.Sample{
		Time: at,
		Box: model.BoxUsage{
			MemTotal: 1 << 30, MemAvailable: 600 * mib, Slab: 60 * mib, PageTables: 6 * mib, KernelStack: 3 * mib,
			HasPressure: true, PressureFull: time.Duration(n) * time.Millisecond, PressureSome: time.Duration(n) * 2 * time.Millisecond,
			CPUBusy: uint64(n) * 20, CPUTotal: uint64(n) * 1000,
		},
		Workloads: make(map[string]model.Usage, soakWorkloads),
	}
	for i := 1; i <= soakWorkloads; i++ {
		wave := (uint64(i)*7 + day) % 24 * 64 << 10
		smp.Workloads[fmt.Sprintf("/system.slice/budget-load-%d.service", i)] = model.Usage{
			Current: 2*mib + wave, Anon: mib + wave, ActiveFile: 256 << 10, InactiveFile: 512 << 10, Peak: 4 * mib, HasPeak: true,
			CPU: time.Duration(n) * time.Millisecond, HasCPU: true,
		}
	}
	return smp
}
