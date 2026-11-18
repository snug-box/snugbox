package audit

import (
	"testing"
	"testing/fstest"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/cron"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
)

// The watcher's runs and the box's timer units together give the schedule
// fix; the timer that starts a job is found even under another name.
func TestScheduleFixFromRunsAndTimers(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var runs []model.Run
	for day := 1; day <= 2; day++ {
		start := now.Truncate(24*time.Hour).AddDate(0, 0, -day).Add(2 * time.Hour)
		runs = append(runs,
			model.Run{Workload: "/system.slice/backup.service", Name: "backup.service", Start: start, End: start.Add(12 * time.Minute), Peak: 500 * mib},
			model.Run{Workload: "/system.slice/dump.service", Name: "dump.service", Start: start, End: start.Add(4 * time.Minute), Peak: 400 * mib})
	}
	r := &Result{Machine: collect.Machine{MemTotal: 8 << 30}, Ended: now, Runs: runs}
	root := fstest.MapFS{
		"usr/lib/systemd/system/backup.timer":   {Data: []byte("[Timer]\nOnCalendar=02:00\n")},
		"etc/systemd/system/dump-nightly.timer": {Data: []byte("[Timer]\nOnCalendar=02:00\nUnit=dump.service\n")},
	}
	in := r.fixInput(root)
	if len(in.Timers) != 2 || in.Timers["dump.service"].Name != "dump-nightly.timer" || in.Timers["backup.service"].Name != "backup.timer" {
		t.Fatalf("timers %+v", in.Timers)
	}
	var shifts []fix.Shift
	for _, p := range fix.Propose(in) {
		if p.Kind == fix.KindSchedule {
			shifts = p.Shifts
		}
	}
	if len(shifts) != 1 || shifts[0].Timer != "dump-nightly.timer" || shifts[0].Service != "dump.service" || shifts[0].Minutes != 10 {
		t.Errorf("schedule fix %+v, want dump-nightly.timer 10 minutes earlier", shifts)
	}

	// A one-off audit has no runs, so it reads no timers.
	r.Runs = nil
	if in := r.fixInput(root); in.Timers != nil {
		t.Errorf("timers read without runs: %+v", in.Timers)
	}
}

// The watcher's crontabs and minute history give the cron advice of the
// schedule fix, read in the box's own time zone.
func TestCronAdviceFromTheWatchersHistory(t *testing.T) {
	now := time.Date(2026, 11, 1, 6, 0, 0, 0, time.Local)
	r := &Result{Machine: collect.Machine{MemTotal: 2 << 30}, Ended: now,
		Cron: cron.Parse("/etc/cron.d/shop", []byte("0 2 * * * root /usr/local/bin/backup.sh\n0 2 * * * root /usr/sbin/logrotate /etc/logrotate.conf\n"), true)}
	for t0 := now.Add(-fix.ScheduleWindow - time.Hour); !t0.After(now); t0 = t0.Add(time.Minute) {
		m := model.MinuteTotal{Time: t0, Total: 800 * mib}
		if l := t0.In(time.Local); l.Day() != now.Day() && l.Hour() == 2 && l.Minute() < 15 {
			m.Total += 500 * mib
		}
		r.Minutes = append(r.Minutes, m)
	}
	Assemble(r, nil)
	var moves []fix.CronMove
	for _, p := range r.Fixes {
		if p.Kind == fix.KindSchedule {
			moves = p.Cron
		}
	}
	if len(moves) != 1 || moves[0].Program != "logrotate" || moves[0].Suggested != "20 2 * * *" || moves[0].Days < 2 {
		t.Errorf("cron advice %+v", moves)
	}
}
