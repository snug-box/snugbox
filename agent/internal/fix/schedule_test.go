package fix

import (
	"slices"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/model"
)

var scheduleNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// daily returns a run of service on each of the days given, counted back
// from scheduleNow, at the time of day given, lasting d and peaking at
// peak.
func daily(service string, at, d time.Duration, peak uint64, daysAgo ...int) []model.Run {
	var out []model.Run
	midnight := scheduleNow.Truncate(24 * time.Hour)
	for _, n := range daysAgo {
		start := midnight.AddDate(0, 0, -n).Add(at)
		out = append(out, model.Run{Workload: "/system.slice/" + service, Name: service, Start: start, End: start.Add(d), Peak: peak})
	}
	return out
}

func timers(services ...string) map[string]model.Timer {
	out := make(map[string]model.Timer)
	for _, s := range services {
		out[s] = model.Timer{Name: s[:len(s)-len(".service")] + ".timer", Service: s}
	}
	return out
}

func scheduleInput(runs []model.Run, ts map[string]model.Timer) Input {
	return Input{Machine: collect.Machine{MemTotal: 8 * gib}, Runs: runs, Now: scheduleNow, Timers: ts}
}

func TestScheduleShifts(t *testing.T) {
	two := 2 * time.Hour
	backup := daily("backup.service", two, 12*time.Minute, 500*mib, 1, 2, 3)
	dump := daily("dump.service", two+20*time.Second, 4*time.Minute, 400*mib, 1, 2, 3)
	got := scheduleShifts(scheduleInput(slices.Concat(backup, dump), timers("backup.service", "dump.service")))
	// backup.service runs longer, so it keeps its time, and dump.service
	// starts before it: its 4 minutes rounded up to a step of 5, and one
	// more step.
	want := []Shift{{Timer: "dump.timer", Service: "dump.service", With: "backup.service", Minutes: 10, Days: 3, Peak: 900 * mib}}
	if !slices.Equal(got, want) {
		t.Fatalf("shifts %+v, want %+v", got, want)
	}
	ps := Propose(scheduleInput(slices.Concat(backup, dump), timers("backup.service", "dump.service")))
	if p, ok := find(ps, KindSchedule); !ok || p.Risk != RiskLow || !slices.Equal(p.Shifts, want) {
		t.Errorf("proposal %+v", ps)
	}
}

func TestScheduleShiftsNeedARealCollision(t *testing.T) {
	two := 2 * time.Hour
	both := timers("backup.service", "dump.service")
	cases := []struct {
		name string
		runs []model.Run
	}{
		{"one day only", slices.Concat(daily("backup.service", two, time.Minute, 500*mib, 1), daily("dump.service", two, time.Minute, 400*mib, 1))},
		{"light together", slices.Concat(daily("backup.service", two, time.Minute, 500*mib, 1, 2), daily("dump.service", two, time.Minute, 300*mib, 1, 2))},
		{"two minutes apart", slices.Concat(daily("backup.service", two, time.Minute, 500*mib, 1, 2), daily("dump.service", two+2*time.Minute, time.Minute, 400*mib, 1, 2))},
		{"one day too long ago", slices.Concat(daily("backup.service", two, time.Minute, 500*mib, 1, 8), daily("dump.service", two, time.Minute, 400*mib, 1, 8))},
		{"not a job", slices.Concat(daily("backup.service", two, 7*time.Hour, 500*mib, 1, 2), daily("dump.service", two, 7*time.Hour, 400*mib, 1, 2))},
		{"a scope", slices.Concat(daily("backup.service", two, time.Minute, 500*mib, 1, 2), daily("session-4.scope", two, time.Minute, 400*mib, 1, 2))},
		{"the same service twice", slices.Concat(daily("backup.service", two, time.Minute, 500*mib, 1, 2), daily("backup.service", two+10*time.Second, time.Minute, 400*mib, 1, 2))},
	}
	for _, c := range cases {
		if got := scheduleShifts(scheduleInput(c.runs, both)); len(got) != 0 {
			t.Errorf("%s: shifts %+v", c.name, got)
		}
	}
}

func TestScheduleShiftsChooseWhatCanMove(t *testing.T) {
	two := 2 * time.Hour
	runs := slices.Concat(daily("backup.service", two, 12*time.Minute, 500*mib, 1, 2), daily("dump.service", two, 4*time.Minute, 400*mib, 1, 2))
	moved := timers("backup.service", "dump.service")
	moved["dump.service"] = model.Timer{Name: "dump.timer", Service: "dump.service", Moved: scheduleNow.Add(-time.Hour)}
	cases := []struct {
		name string
		ts   map[string]model.Timer
		want string
	}{
		{"both have timers", timers("backup.service", "dump.service"), "dump.timer"},
		{"only the longer has a timer", timers("backup.service"), "backup.timer"},
		{"neither has a timer", nil, ""},
		// The collisions are from before dump.timer moved, so they are over.
		{"moved already", moved, ""},
	}
	for _, c := range cases {
		got := scheduleShifts(scheduleInput(runs, c.ts))
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s: shifts %+v", c.name, got)
		case c.want != "" && (len(got) != 1 || got[0].Timer != c.want):
			t.Errorf("%s: shifts %+v, want %s moved", c.name, got, c.want)
		}
	}

	// A unit no fix may touch stays, and the other ends before it starts.
	runs = slices.Concat(daily("backup.service", two, 3*time.Minute, 500*mib, 1, 2), daily("systemd-tmpfiles-clean.service", two, time.Minute, 400*mib, 1, 2))
	got := scheduleShifts(scheduleInput(runs, timers("backup.service", "systemd-tmpfiles-clean.service")))
	if len(got) != 1 || got[0].Timer != "backup.timer" || got[0].Minutes != 10 {
		t.Errorf("next to a protected unit: %+v", got)
	}

	// On a tie the one later in the alphabet moves.
	runs = slices.Concat(daily("alpha.service", two, time.Minute, 450*mib, 1, 2), daily("beta.service", two, time.Minute, 450*mib, 1, 2))
	if got := scheduleShifts(scheduleInput(runs, timers("alpha.service", "beta.service"))); len(got) != 1 || got[0].Timer != "beta.timer" {
		t.Errorf("tie: %+v", got)
	}
}

// A timer moved already stays where it is; collisions since then move the
// other job, which must end before the moved one starts.
func TestScheduleShiftsAfterAMove(t *testing.T) {
	two := 2 * time.Hour
	runs := slices.Concat(daily("backup.service", two, 12*time.Minute, 500*mib, 1, 2, 5, 6), daily("dump.service", two, 4*time.Minute, 400*mib, 1, 2, 5, 6))
	ts := timers("backup.service", "dump.service")
	ts["dump.service"] = model.Timer{Name: "dump.timer", Service: "dump.service", Moved: scheduleNow.AddDate(0, 0, -4)}
	got := scheduleShifts(scheduleInput(runs, ts))
	if len(got) != 1 || got[0].Timer != "backup.timer" || got[0].Days != 2 || got[0].Minutes != 20 {
		t.Errorf("shifts %+v, want backup.timer moved 20 minutes for 2 days", got)
	}
}

func TestScheduleShiftsLayOutAGroup(t *testing.T) {
	two := 2 * time.Hour
	runs := slices.Concat(
		daily("backup.service", two, 40*time.Minute, 900*mib, 1, 2),
		daily("dump.service", two, 4*time.Minute, 600*mib, 1, 2),
		daily("index.service", two+30*time.Second, 25*time.Minute, 700*mib, 1, 2),
	)
	got := scheduleShifts(scheduleInput(runs, timers("backup.service", "dump.service", "index.service")))
	// backup.service runs longest and keeps its time. index.service, the
	// heavier of the others, ends a step before backup starts, and
	// dump.service a step before index starts, so no two of them start
	// together again.
	want := []Shift{
		{Timer: "dump.timer", Service: "dump.service", Minutes: 40, With: "backup.service", Days: 2, Peak: 1500 * mib},
		{Timer: "index.timer", Service: "index.service", Minutes: 30, With: "backup.service", Days: 2, Peak: 1600 * mib},
	}
	if !slices.Equal(got, want) {
		t.Errorf("shifts %+v, want %+v", got, want)
	}
}

// A job that would move more than two hours stays where it is.
func TestScheduleShiftsStopAtTwoHours(t *testing.T) {
	two := 2 * time.Hour
	var runs []model.Run
	for i, name := range []string{"a.service", "b.service", "c.service", "d.service"} {
		runs = append(runs, daily(name, two, 50*time.Minute, uint64(900-100*i)*mib, 1, 2)...)
	}
	got := scheduleShifts(scheduleInput(runs, timers("a.service", "b.service", "c.service", "d.service")))
	want := []Shift{
		{Timer: "b.timer", Service: "b.service", Minutes: 55, With: "a.service", Days: 2, Peak: 1700 * mib},
		{Timer: "c.timer", Service: "c.service", Minutes: 110, With: "a.service", Days: 2, Peak: 1600 * mib},
	}
	if !slices.Equal(got, want) {
		t.Errorf("shifts %+v, want %+v", got, want)
	}
}

// A job that runs every hour moves by less than an hour, or not at all.
func TestScheduleShiftsMoveAnHourlyJobLessThanAnHour(t *testing.T) {
	hourly := func(d time.Duration) []model.Run {
		var out []model.Run
		for h := range 4 * 24 {
			start := scheduleNow.Add(-time.Duration(h+1) * time.Hour).Truncate(time.Hour)
			out = append(out, model.Run{Name: "report.service", Start: start, End: start.Add(d), Peak: 300 * mib})
		}
		return out
	}
	backup := slices.Concat(daily("backup.service", 2*time.Hour, 3*time.Hour, 600*mib, 1, 2), daily("backup.service", 2*time.Hour, 50*time.Minute, 600*mib, 3))
	ts := timers("backup.service", "report.service")
	// The backup runs longest and stays; the report's 8 minutes put it a
	// quarter of an hour before.
	got := scheduleShifts(scheduleInput(slices.Concat(hourly(8*time.Minute), backup), ts))
	if len(got) != 1 || got[0].Timer != "report.timer" || got[0].Minutes != 15 || got[0].Days != 3 {
		t.Errorf("shifts %+v, want report.timer moved 15 minutes, from 3 days", got)
	}
	// A report that runs 55 minutes would have to move a whole hour.
	if got := scheduleShifts(scheduleInput(slices.Concat(hourly(55*time.Minute), backup), ts)); len(got) != 0 {
		t.Errorf("shifts %+v, want none", got)
	}
}

// A job moved earlier doesn't start together with another heavy job there:
// it moves a step further.
func TestScheduleShiftsDoNotLandOnAnotherJob(t *testing.T) {
	two := 2 * time.Hour
	collide := slices.Concat(daily("backup.service", two, 12*time.Minute, 500*mib, 1, 2, 3), daily("dump.service", two, 4*time.Minute, 400*mib, 1, 2, 3))
	ts := timers("backup.service", "dump.service", "index.service")
	for peak, want := range map[uint64]int{450 * mib: 15, 100 * mib: 10} {
		// index.service runs alone at 01:50, where dump.service would go.
		index := daily("index.service", two-10*time.Minute, 3*time.Minute, peak, 1, 2, 3)
		got := scheduleShifts(scheduleInput(slices.Concat(collide, index), ts))
		if len(got) != 1 || got[0].Timer != "dump.timer" || got[0].Minutes != want {
			t.Errorf("next to a job of %d MiB at 01:50: shifts %+v, want dump.timer %d minutes earlier", peak/mib, got, want)
		}
	}
}

func TestShiftFor(t *testing.T) {
	for d, want := range map[time.Duration]int{
		0:                5,
		time.Second:      10,
		5 * time.Minute:  10,
		12 * time.Minute: 20,
		time.Hour:        65,
		3 * time.Hour:    120,
	} {
		if got := shiftFor(d); got != want {
			t.Errorf("shiftFor(%s) = %d, want %d", d, got, want)
		}
	}
}

func TestPeriod(t *testing.T) {
	day := 24 * time.Hour
	at := func(hours ...float64) []time.Time {
		var out []time.Time
		for _, h := range hours {
			out = append(out, scheduleNow.Add(time.Duration(h*float64(time.Hour))))
		}
		return out
	}
	cases := []struct {
		starts []time.Time
		want   time.Duration
	}{
		{nil, ScheduleWindow},
		{at(0), ScheduleWindow},
		{at(0, 24, 48, 72), day},
		// A run by hand at hour 30 doesn't make it a six-hour job.
		{at(0, 24, 30, 48, 72, 96), day},
		{at(0, 1, 2, 3), time.Hour},
	}
	for _, c := range cases {
		if got := period(c.starts); got != c.want {
			t.Errorf("period(%v) = %s, want %s", c.starts, got, c.want)
		}
	}
}
