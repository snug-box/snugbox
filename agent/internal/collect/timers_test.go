package collect

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
)

func TestTimers(t *testing.T) {
	moved := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	root := fstest.MapFS{
		// The service of the timer's own name.
		"usr/lib/systemd/system/backup.timer":   file("[Unit]\nDescription=Nightly backup\n\n[Timer]\nOnCalendar=daily\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n"),
		"usr/lib/systemd/system/backup.service": file("[Service]\nType=oneshot\n"),
		// Unit= names another service; a comment and a line joined to the
		// next don't confuse it, nor does Unit= outside [Timer].
		"etc/systemd/system/nightly.timer": file("[Unit]\nUnit=wrong.service\n[Timer]\n# Unit=commented.service\n; Unit=commented.service\nOnCalendar=Mon \\\n  02:00\nUnit = dump.service\r\n"),
		// /etc hides the copy in /usr/lib; the /etc one names another unit.
		"etc/systemd/system/index.timer":     file("[Timer]\nOnCalendar=hourly\nUnit=reindex.service\n"),
		"usr/lib/systemd/system/index.timer": file("[Timer]\nOnCalendar=hourly\n"),
		// Masked: a link to /dev/null reads as an empty file, and hides the
		// unit below it.
		"etc/systemd/system/cleanup.timer":     file(""),
		"usr/lib/systemd/system/cleanup.timer": file("[Timer]\nOnCalendar=daily\n"),
		// Two timers start report.service, so moving one wouldn't do.
		"usr/lib/systemd/system/report.timer":       file("[Timer]\nOnCalendar=daily\n"),
		"usr/lib/systemd/system/report-extra.timer": file("[Timer]\nOnCalendar=weekly\nUnit=report.service\n"),
		// A template matches its instances; a unit named with specifiers,
		// in a template or not, is skipped.
		"lib/systemd/system/scrub@.timer":     file("[Timer]\nOnCalendar=monthly\n"),
		"lib/systemd/system/trim@.timer":      file("[Timer]\nOnCalendar=weekly\nUnit=fstrim@%i.service\n"),
		"usr/lib/systemd/system/mirror.timer": file("[Timer]\nOnCalendar=daily\nUnit=mirror-%H.service\n"),
		// A unit made at runtime counts, a file too big to be a unit doesn't,
		// and a drop-in folder is not a timer.
		"run/systemd/system/runtime.timer":       file("[Timer]\nOnCalendar=daily\n"),
		"usr/lib/systemd/system/huge.timer":      file("[Timer]\nOnCalendar=daily\n" + strings.Repeat("#", maxUnitFile)),
		"usr/lib/systemd/system/sync.timer":      file("[Timer]\nOnCalendar=daily\n"),
		"etc/systemd/system/sync.timer.d/x.conf": file("[Timer]\nOnCalendar=\n"),
		// Snugbox moved this one.
		"usr/lib/systemd/system/logs.timer":                           file("[Timer]\nOnCalendar=daily\n"),
		"etc/systemd/system/logs.timer.d/" + helperapi.ScheduleDropIn: &fstest.MapFile{Data: []byte("[Timer]\nOnCalendar=\nOnCalendar=*-*-* 00:15:00\n"), ModTime: moved},
	}
	services := []string{"backup.service", "dump.service", "wrong.service", "commented.service", "reindex.service", "index.service",
		"cleanup.service", "report.service", "scrub@data.service", "fstrim@sda.service", "trim@sda.service", "mirror-box.service",
		"runtime.service", "huge.service", "sync.service", "logs.service", "nginx.service"}
	got := Timers(root, services)
	want := map[string]model.Timer{
		"backup.service":     {Name: "backup.timer", Service: "backup.service"},
		"dump.service":       {Name: "nightly.timer", Service: "dump.service"},
		"reindex.service":    {Name: "index.timer", Service: "reindex.service"},
		"scrub@data.service": {Name: "scrub@data.timer", Service: "scrub@data.service"},
		"runtime.service":    {Name: "runtime.timer", Service: "runtime.service"},
		"sync.service":       {Name: "sync.timer", Service: "sync.service"},
		"logs.service":       {Name: "logs.timer", Service: "logs.service", Moved: moved},
	}
	if !maps.EqualFunc(got, want, func(a, b model.Timer) bool {
		return a.Name == b.Name && a.Service == b.Service && a.Moved.Equal(b.Moved)
	}) {
		keys := slices.Sorted(maps.Keys(got))
		t.Errorf("Timers found %v:\n%+v\nwant\n%+v", keys, got, want)
	}
	if len(Timers(fstest.MapFS{}, services)) != 0 {
		t.Error("timers found on a box without unit folders")
	}
}
