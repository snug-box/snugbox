package helper

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

func scheduleFix(shifts ...*helperv1.TimerShift) *helperv1.Fix {
	return &helperv1.Fix{Kind: &helperv1.Fix_Schedule{Schedule: &helperv1.ScheduleFix{Shifts: shifts}}}
}

func shift(timer, service string, minutes uint32) *helperv1.TimerShift {
	return &helperv1.TimerShift{Timer: timer, Service: service, Minutes: minutes}
}

func TestCalendarTrigger(t *testing.T) {
	for v, want := range map[string][2]string{
		"{ OnCalendar=*-*-* 02:00:00 ; next_elapse=Sun 2026-11-01 02:00:00 UTC }": {"*-*-* 02:00:00", "Sun 2026-11-01 02:00:00 UTC"},
		"{ OnCalendar=Mon..Fri *-*-* 02:30:00 Europe/Berlin ; next_elapse=Mon 2026-11-02 01:30:00 UTC }": {
			"Mon..Fri *-*-* 02:30:00 Europe/Berlin", "Mon 2026-11-02 01:30:00 UTC",
		},
		"{ OnCalendar=*-*-* 00,12:05:00 ; next_elapse=n/a }":      {"*-*-* 00,12:05:00", ""},
		"{ OnCalendar=2020-01-01 00:00:00 ; next_elapse=(null) }": {"2020-01-01 00:00:00", ""},
		"{ OnCalendar=*-*-* 04:00:00 }":                           {"*-*-* 04:00:00", ""},
		"{ OnBootUSec=15min ; next_elapse=16min 3.308796s }":      {"", ""},
		"{ OnCalendar= ; next_elapse=n/a }":                       {"", ""},
		"":                                                        {"", ""},
	} {
		spec, next, ok := calendarTrigger(v)
		if spec != want[0] || next != want[1] || ok != (want[0] != "") {
			t.Errorf("calendarTrigger(%q) = %q, %q, %v; want %q, %q", v, spec, next, ok, want[0], want[1])
		}
	}
}

func TestShiftCalendar(t *testing.T) {
	cases := []struct {
		spec    string
		minutes int
		want    string
		refusal string
	}{
		{"*-*-* 02:15:00", 15, "*-*-* 02:00:00", ""},
		{"*-*-* 03:15:00", 25, "*-*-* 02:50:00", ""},
		// A calendar for every day may move back past midnight.
		{"*-*-* 00:00:00", 20, "*-*-* 23:40:00", ""},
		{"*-*-* 00:00:00 Europe/Berlin", 120, "*-*-* 22:00:00 Europe/Berlin", ""},
		{"Mon..Fri *-*-* 02:30:00 Europe/Berlin", 20, "Mon..Fri *-*-* 02:10:00 Europe/Berlin", ""},
		{"Mon *-*-* 02:00:00", 120, "Mon *-*-* 00:00:00", ""},
		// One for some days may not: Monday's run would move to Sunday.
		{"Mon..Fri *-*-* 00:10:00", 20, "", "back past midnight, onto other days"},
		{"*-*-01 00:10:00", 20, "", "back past midnight, onto other days"},
		{"*-*-* *:20:00", 20, "*-*-* *:00:00", ""},
		// Every hour may move back past the hour, by less than an hour.
		{"*-*-* *:00:00", 20, "*-*-* *:40:00", ""},
		{"*-*-* *:00:00", 60, "", "less than an hour"},
		{"Mon *-*-* *:00:00", 20, "", "back past the hour, onto other days"},
		{"*-*-* 00,12:25:00", 15, "*-*-* 00,12:10:00", ""},
		{"*-*-* 00,12:05:00", 10, "", "back past the hour, onto other hours"},
		{"*-*-* *:00/15:00", 5, "", "minutes and seconds are not single fixed values"},
		{"*-*-* 08..17:30:00", 5, "", "hours are not a fixed value, a list or *"},
		{"*-*-* 02:00:00.500000", 5, "", "minutes and seconds are not single fixed values"},
		{"*-*-* 02:00", 5, "", "minutes and seconds are not single fixed values"},
		{"*-*-* 24:00:00", 5, "", "not a time of day"},
		{"*-*-* 02:60:00", 5, "", "not a time of day"},
		{"*-*-* 02:00:61", 5, "", "not a time of day"},
		{"*-*-* 02:00:00 03:00:00", 5, "", "more than one time"},
		{"*-*-*", 5, "", "no time of day"},
		{"*-*-* 02:00:00", 0, "", "out of range"},
		{"*-*-* 02:00:00", 24*60 + 1, "", "out of range"},
	}
	for _, c := range cases {
		got, err := shiftCalendar(c.spec, c.minutes)
		switch {
		case c.refusal == "" && (err != nil || got != c.want):
			t.Errorf("shiftCalendar(%q, %d) = %q, %v; want %q", c.spec, c.minutes, got, err, c.want)
		case c.refusal != "" && (err == nil || !strings.Contains(err.Error(), c.refusal)):
			t.Errorf("shiftCalendar(%q, %d) = %q, %v; want a refusal saying %q", c.spec, c.minutes, got, err, c.refusal)
		}
	}
}

func TestScheduleApplyAndUndo(t *testing.T) {
	h := newHarness(t)
	h.box.timer("backup.timer", "backup.service", "*-*-* 02:15:00")
	drop := path.Join(UnitDir, "backup.timer.d", helperapi.ScheduleDropIn)
	fix := scheduleFix(shift("backup.timer", "backup.service", 15))

	dry, _ := h.do(applyReq(fix, true), Root)
	changes := dry.GetApply().GetChanges()
	if len(changes) != 1 || changes[0].GetTarget() != drop || !strings.HasSuffix(changes[0].GetNewValue(), "OnCalendar=*-*-* 02:00:00\n") {
		t.Fatalf("dry run: %v", dry)
	}
	if _, ok := h.box.files[drop]; ok {
		t.Fatal("a dry run wrote the drop-in")
	}

	h.box.calls = nil
	tx := h.mustApply(fix).GetTransaction()
	want := "[Timer]\nOnCalendar=\nOnCalendar=*-*-* 02:00:00\n"
	if got := h.box.files[drop]; !strings.HasPrefix(got, "# Made by Snugbox for transaction "+tx+".") || !strings.HasSuffix(got, want) ||
		!strings.Contains(got, "https://snugbox.dev/docs/fixes/schedule/") {
		t.Errorf("drop-in:\n%s", got)
	}
	if got, _, _ := calendarTrigger(h.box.units["backup.timer"]["TimersCalendar"]); got != "*-*-* 02:00:00" {
		t.Errorf("the timer fires at %q after the fix", got)
	}
	// A reload is all it takes; stopping the timer would change nothing, as
	// systemd keeps its last run.
	if !slices.Contains(h.box.calls, "systemctl daemon-reload") || slices.ContainsFunc(h.box.calls, func(c string) bool {
		return strings.HasPrefix(c, "systemctl stop") || strings.HasPrefix(c, "systemctl start")
	}) {
		t.Errorf("calls %q", h.box.calls)
	}
	if h.state(tx) != helperv1.State_STATE_VERIFYING {
		t.Errorf("transaction %s", h.state(tx))
	}

	resp, _ := h.do(undoReq(tx, ""), Root)
	if resp.GetUndo() == nil || len(resp.GetUndo().GetNotes()) != 0 {
		t.Fatalf("undo: %v", resp)
	}
	if _, ok := h.box.files[drop]; ok {
		t.Error("undo left the drop-in")
	}
	if got, _, _ := calendarTrigger(h.box.units["backup.timer"]["TimersCalendar"]); got != "*-*-* 02:15:00" {
		t.Errorf("the timer fires at %q after the undo", got)
	}
}

// Moved earlier within the time left before the job is due, the new time
// would have passed and systemd would start the job at once.
func TestScheduleRefusesAJobDueSoon(t *testing.T) {
	h := newHarness(t)
	h.box.timer("backup.timer", "backup.service", "*-*-* 10:15:00")
	h.box.nexts["backup.timer"] = "Sat 2026-10-31 10:15:00 UTC"
	h.box.units["backup.timer"]["TimersCalendar"] = calendarProp([]string{"*-*-* 10:15:00"}, h.box.nexts["backup.timer"])
	due := time.Date(2026, 10, 31, 10, 15, 0, 0, time.UTC)
	// It is 10:00. Moved 15 minutes the job would be due now, and moved 14
	// within the minute the move is given to take hold.
	for _, minutes := range []uint32{15, 14} {
		resp, _ := h.do(applyReq(scheduleFix(shift("backup.timer", "backup.service", minutes)), false), Root)
		wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, fmt.Sprintf(
			"backup.timer starts backup.service next at %s; moved %d minutes earlier now, it would start it at once. Apply this after %s", clock(due), minutes, clock(due)))
	}
	if slices.Contains(h.box.calls, "systemctl daemon-reload") {
		t.Error("a refused move reloaded systemd")
	}
	h.mustApply(scheduleFix(shift("backup.timer", "backup.service", 13)))
	if got, _, _ := calendarTrigger(h.box.units["backup.timer"]["TimersCalendar"]); got != "*-*-* 10:02:00" {
		t.Errorf("the timer fires at %q after the fix", got)
	}
}

// A move that fails part way leaves no drop-in behind.
func TestScheduleFailedReloadIsPutBack(t *testing.T) {
	h := newHarness(t)
	h.box.timer("backup.timer", "backup.service", "*-*-* 02:15:00")
	h.box.failOnce = map[string]error{"systemctl daemon-reload": errors.New("Failed to reload daemon: Connection timed out")}
	resp, _ := h.do(applyReq(scheduleFix(shift("backup.timer", "backup.service", 15)), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, "Connection timed out")
	if _, ok := h.box.files[path.Join(UnitDir, "backup.timer.d", helperapi.ScheduleDropIn)]; ok {
		t.Error("the failed move left its drop-in")
	}
	if got, _, _ := calendarTrigger(h.box.units["backup.timer"]["TimersCalendar"]); got != "*-*-* 02:15:00" {
		t.Errorf("after the failed move the timer fires at %q", got)
	}
}

// A person's own drop-in that sorts after Snugbox's wins, so the timer would
// still fire at the old time; confirm sees that and the fix is put back.
func TestScheduleThatDoesNotTakeIsPutBack(t *testing.T) {
	h := newHarness(t)
	h.box.timer("backup.timer", "backup.service", "*-*-* 02:15:00")
	h.box.noApply = true
	resp, _ := h.do(applyReq(scheduleFix(shift("backup.timer", "backup.service", 15)), false), Root)
	wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_FAILED, `backup.timer fires at "*-*-* 02:15:00" after the reload, not "*-*-* 02:00:00"`)
	if _, ok := h.box.files[path.Join(UnitDir, "backup.timer.d", helperapi.ScheduleDropIn)]; ok {
		t.Error("the drop-in is still there")
	}
}

// An undo puts the later, old time back, and systemd counts from the last
// run: a job that ran at its earlier time runs once more, and the undo says
// when.
func TestScheduleUndoSaysWhenTheJobRunsAgain(t *testing.T) {
	ranEarly := time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC)
	old := time.Date(2026, 11, 1, 2, 15, 0, 0, time.UTC)
	cases := []struct {
		name string
		// now is when the undo runs, last when the job last ran, and next
		// when systemd says it runs next once the old time is back.
		now, last, next time.Time
		// fires makes systemd start the job as the reload ends, as it does
		// when the next run has passed already.
		fires bool
		want  string
	}{
		{"before the old time", ranEarly.Add(5 * time.Minute), ranEarly, old, false,
			"backup.timer starts backup.service again at " + clock(old) + ", its old time, though it ran at " + clock(ranEarly) + ": systemd counts from that run."},
		{"after the old time, read before it fires", old.Add(5 * time.Minute), ranEarly, old, false,
			"systemd started backup.service at once when the move was put back, because the old time of backup.timer had passed since its last run."},
		{"after the old time, read after it fired", old.Add(5 * time.Minute), ranEarly, old.AddDate(0, 0, 1), true,
			"systemd started backup.service at once when the move was put back, because the old time of backup.timer had passed since its last run."},
		// systemctl prints the run to the second, so it reads as a little
		// before an undo that began 0.4 seconds into the same second.
		{"fired in the second the undo began", old.Add(5*time.Minute + 400*time.Millisecond), ranEarly, old.AddDate(0, 0, 1), true,
			"systemd started backup.service at once when the move was put back, because the old time of backup.timer had passed since its last run."},
		{"the job last ran at its old time", time.Date(2026, 10, 31, 10, 30, 0, 0, time.UTC), old.AddDate(0, 0, -1), old, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.box.timer("backup.timer", "backup.service", "*-*-* 02:15:00")
			tx := h.mustApply(scheduleFix(shift("backup.timer", "backup.service", 15))).GetTransaction()
			h.box.now = c.now
			h.box.units["backup.timer"]["LastTriggerUSec"] = c.last.Format(systemdUTC)
			h.box.nexts["backup.timer"] = c.next.Format(systemdUTC)
			if c.fires {
				h.box.onCall = func(call string) {
					if call == "systemctl daemon-reload" {
						h.box.units["backup.timer"]["LastTriggerUSec"] = c.now.Format(systemdUTC)
					}
				}
			}
			resp, _ := h.do(undoReq(tx, ""), Root)
			if resp.GetUndo() == nil {
				t.Fatalf("undo: %v", resp)
			}
			if got := strings.Join(resp.GetUndo().GetNotes(), " "); got != c.want {
				t.Errorf("notes %q\nwant  %q", got, c.want)
			}
		})
	}
}

func TestSchedulePreconditions(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeBox)
		shift *helperv1.TimerShift
		want  string
	}{
		{"no such timer", func(*fakeBox) {}, shift("backup.timer", "backup.service", 15), "backup.timer is not a timer on this box"},
		{"stopped", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "*-*-* 02:00:00")
			b.units["backup.timer"]["ActiveState"] = "inactive"
		}, shift("backup.timer", "backup.service", 15), "not running"},
		{"starts a protected unit", func(b *fakeBox) { b.timer("backup.timer", "sshd.service", "*-*-* 02:00:00") },
			shift("backup.timer", "backup.service", 15), "starts sshd.service, which no fix may touch"},
		{"starts another unit", func(b *fakeBox) { b.timer("backup.timer", "dump.service", "*-*-* 02:00:00") },
			shift("backup.timer", "backup.service", 15), "starts dump.service now, not backup.service"},
		{"also monotonic", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "*-*-* 02:00:00")
			b.units["backup.timer"]["TimersMonotonic"] = "{ OnBootUSec=15min ; next_elapse=16min }"
		}, shift("backup.timer", "backup.service", 15), "after boot or after its last run"},
		{"two calendars", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "Mon *-*-* 03:00:00", "Thu *-*-* 04:00:00")
		},
			shift("backup.timer", "backup.service", 15), "has 2 calendar triggers"},
		{"no calendar", func(b *fakeBox) { b.timer("backup.timer", "backup.service") },
			shift("backup.timer", "backup.service", 15), "has 0 calendar triggers"},
		{"unreadable calendar", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "*-*-* 02:00:00")
			b.units["backup.timer"]["TimersCalendar"] = "{ something new }"
		}, shift("backup.timer", "backup.service", 15), "can't read the calendar of backup.timer"},
		{"onto other days", func(b *fakeBox) { b.timer("backup.timer", "backup.service", "Mon..Fri *-*-* 00:10:00") },
			shift("backup.timer", "backup.service", 15),
			"fires at Mon..Fri *-*-* 00:10:00, which Snugbox won't move 15 minutes earlier: the move would carry it back past midnight, onto other days"},
		{"repeating", func(b *fakeBox) { b.timer("backup.timer", "backup.service", "*-*-* *:00/15:00") },
			shift("backup.timer", "backup.service", 5), "RandomizedDelaySec"},
		{"never again", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "2020-01-01 02:00:00")
			b.units["backup.timer"]["TimersCalendar"] = "{ OnCalendar=2020-01-01 02:00:00 ; next_elapse=(null) }"
		}, shift("backup.timer", "backup.service", 15), "backup.timer never fires again"},
		{"unreadable next run", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "*-*-* 02:00:00")
			b.units["backup.timer"]["TimersCalendar"] = "{ OnCalendar=*-*-* 02:00:00 ; next_elapse=Sun 2026-11-01 07:30:00 IST }"
		}, shift("backup.timer", "backup.service", 15), `can't read when backup.timer fires next from "Sun 2026-11-01 07:30:00 IST"`},
		{"moved already", func(b *fakeBox) {
			b.timer("backup.timer", "backup.service", "*-*-* 02:00:00")
			b.files[path.Join(UnitDir, "backup.timer.d", helperapi.ScheduleDropIn)] = "[Timer]\n"
		}, shift("backup.timer", "backup.service", 15), helperapi.ScheduleDropIn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.setup(h.box)
			resp, _ := h.do(applyReq(scheduleFix(c.shift), false), Root)
			wantError(t, resp, helperv1.ErrorCode_ERROR_CODE_PRECONDITION, c.want)
			if slices.Contains(h.box.calls, "systemctl daemon-reload") {
				t.Error("a refused fix reloaded systemd")
			}
		})
	}
}

// Two timers move in one fix and come back together.
func TestScheduleMovesSeveral(t *testing.T) {
	h := newHarness(t)
	h.box.timer("backup.timer", "backup.service", "*-*-* 02:15:00")
	h.box.timer("report.timer", "report.service", "*-*-* *:40:00")
	h.box.nexts["report.timer"] = "Sat 2026-10-31 10:40:00 UTC"
	tx := h.mustApply(scheduleFix(shift("backup.timer", "backup.service", 15), shift("report.timer", "report.service", 10))).GetTransaction()
	for timer, want := range map[string]string{"backup.timer": "*-*-* 02:00:00", "report.timer": "*-*-* *:30:00"} {
		if got, _, _ := calendarTrigger(h.box.units[timer]["TimersCalendar"]); got != want {
			t.Errorf("%s fires at %q, want %q", timer, got, want)
		}
	}
	if resp, _ := h.do(undoReq(tx, ""), Root); resp.GetUndo() == nil {
		t.Fatalf("undo: %v", resp)
	}
	for _, timer := range []string{"backup.timer", "report.timer"} {
		if _, ok := h.box.files[path.Join(UnitDir, timer+".d", helperapi.ScheduleDropIn)]; ok {
			t.Errorf("undo left the drop-in of %s", timer)
		}
	}
}
