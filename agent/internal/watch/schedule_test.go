package watch

import (
	"context"
	"slices"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/testbox"
)

// Two timer jobs that start together on two nights are seen as runs, and
// the analysis proposes starting one of them earlier, so that it ends
// before the other starts.
func TestScheduleFixFromTheWatcher(t *testing.T) {
	b, _ := shop()
	b.Files = map[string]string{
		"usr/lib/systemd/system/backup.timer": "[Timer]\nOnCalendar=02:00\n",
		"usr/lib/systemd/system/dump.timer":   "[Timer]\nOnCalendar=02:00\n",
	}
	clk := &fakeClock{now: time.Date(2026, 10, 29, 1, 58, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	ctx := context.Background()
	backup := &testbox.Workload{Path: "system.slice/backup.service", Programs: []string{"restic"}, Anon: 600 * mib}
	dump := &testbox.Workload{Path: "system.slice/dump.service", Programs: []string{"pg_dump"}, Anon: 400 * mib}
	discover := func() {
		t.Helper()
		if err := d.deps.Collector.Discover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for night := range 2 {
		clk.now = time.Date(2026, 10, 29+night, 1, 58, 0, 0, time.UTC)
		run(t, d, clk, 2*time.Minute, nil)
		b.Add(backup)
		b.Add(dump)
		discover()
		run(t, d, clk, 12*time.Minute, nil)
		b.Update(func(b *testbox.Box) {
			b.Workloads = slices.DeleteFunc(b.Workloads, func(w *testbox.Workload) bool { return w == backup || w == dump })
		})
		discover()
		run(t, d, clk, 3*time.Minute, nil)
	}
	if err := d.analyseNow(ctx); err != nil {
		t.Fatal(err)
	}
	r := d.Result()
	var shifts []fix.Shift
	for _, p := range r.Fixes {
		if p.Kind == fix.KindSchedule {
			shifts = p.Shifts
		}
	}
	// The two ran for the same time, just under 12 minutes as the watcher
	// saw it, so backup.service, first by name, keeps its time, and
	// dump.service starts three steps of 5 minutes, and one more, before it.
	if len(shifts) != 1 || shifts[0].Timer != "dump.timer" || shifts[0].With != "backup.service" || shifts[0].Days != 2 || shifts[0].Minutes != 20 {
		t.Errorf("schedule fix %+v, want dump.timer 20 minutes earlier from 2 nights", shifts)
	}
}
