package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/fix"
)

func TestScheduleWords(t *testing.T) {
	dump := fix.Shift{Timer: "dump.timer", Service: "dump.service", Minutes: 20, With: "backup.service", Days: 3, Peak: 900 * mib}
	index := fix.Shift{Timer: "index.timer", Service: "index.service", Minutes: 1, With: "backup.service", Days: 2, Peak: 1500 * mib}
	one := fix.Proposal{Kind: fix.KindSchedule, Risk: fix.RiskLow, Shifts: []fix.Shift{dump}}
	two := fix.Proposal{Kind: fix.KindSchedule, Risk: fix.RiskLow, Shifts: []fix.Shift{dump, index}}
	for p, want := range map[*fix.Proposal]string{&one: "Move dump.timer 20 minutes earlier", &two: "Move 2 timers earlier"} {
		if got := FixTitle(*p); got != want {
			t.Errorf("FixTitle = %q, want %q", got, want)
		}
	}
	for s, want := range map[fix.Shift]string{
		dump:  "dump.timer: 20 minutes earlier. dump.service started within a minute of backup.service on 3 of the last 7 days, and their peaks add up to 900 MiB.",
		index: "index.timer: 1 minute earlier. index.service started within a minute of backup.service on 2 of the last 7 days, and their peaks add up to 1.46 GiB.",
	} {
		if got := ShiftLine(s); got != want {
			t.Errorf("ShiftLine = %q\nwant %q", got, want)
		}
	}

	r := shopResult(t)
	r.Fixes = append(r.Fixes, two)
	n := len(r.Fixes)
	var md bytes.Buffer
	if err := Markdown(&md, r, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{fmt.Sprintf("| %d | Move 2 timers earlier | low |", n), "- " + ShiftLine(dump), "- " + ShiftLine(index)} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("the Markdown report has no %q", want)
		}
	}

	var js bytes.Buffer
	if err := JSON(&js, r, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Fixes []struct {
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Shifts []struct {
				Timer, Service string
				With           string `json:"collides_with"`
				Minutes        int    `json:"minutes_earlier"`
				Days           int
				PeakBytes      uint64 `json:"peak_bytes"`
			} `json:"shifts"`
		} `json:"fixes"`
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	last := doc.Fixes[len(doc.Fixes)-1]
	if last.Kind != "schedule" || last.Title != "Move 2 timers earlier" || len(last.Shifts) != 2 {
		t.Fatalf("JSON fix %+v", last)
	}
	if s := last.Shifts[0]; s.Timer != "dump.timer" || s.Service != "dump.service" || s.With != "backup.service" || s.Minutes != 20 || s.Days != 3 || s.PeakBytes != 900*mib {
		t.Errorf("JSON shift %+v", s)
	}
}

func TestCronWords(t *testing.T) {
	dump := fix.Shift{Timer: "dump.timer", Service: "dump.service", Minutes: 20, With: "backup.service", Days: 3, Peak: 900 * mib}
	at := time.Date(2026, 10, 30, 2, 0, 0, 0, time.FixedZone("IST", 19800))
	rotate := fix.CronMove{File: "/etc/cron.d/shop", Line: 2, Program: "logrotate", Schedule: "0 2 * * *", Suggested: "25 2 * * *", Minutes: 25,
		With: "backup.sh", At: at, Days: 3, Rise: 400 * mib}
	sync := fix.CronMove{File: "/var/spool/cron/crontabs/ana", Line: 1, Program: "sync.sh", Schedule: "*/90 2 * * *", Minutes: 15,
		With: "backup.sh", At: at, Days: 2, Rise: 300 * mib}
	for p, want := range map[*fix.Proposal]string{
		{Kind: fix.KindSchedule, Cron: []fix.CronMove{rotate}}:                            "Start logrotate 25 minutes later",
		{Kind: fix.KindSchedule, Cron: []fix.CronMove{rotate, sync}}:                      "Start 2 cron jobs later",
		{Kind: fix.KindSchedule, Shifts: []fix.Shift{dump}, Cron: []fix.CronMove{rotate}}: "Move dump.timer 20 minutes earlier, and start logrotate 25 minutes later",
	} {
		if got := FixTitle(*p); got != want {
			t.Errorf("FixTitle = %q, want %q", got, want)
		}
	}

	r := shopResult(t)
	r.Fixes = append(r.Fixes, fix.Proposal{Kind: fix.KindSchedule, Risk: fix.RiskLow, Cron: []fix.CronMove{rotate, sync}})
	var md bytes.Buffer
	if err := Markdown(&md, r, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []fix.CronMove{rotate, sync} {
		if want := "- " + mdEscape(fix.CronAdvice(c)); !strings.Contains(md.String(), want) {
			t.Errorf("the Markdown report has no %q", want)
		}
	}
	var js bytes.Buffer
	if err := JSON(&js, r, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Fixes []struct {
			Cron []struct {
				File, Program, Schedule string
				Line                    int
				Suggested               string    `json:"suggested_schedule"`
				Minutes                 int       `json:"minutes_later"`
				With                    string    `json:"collides_with"`
				At                      time.Time `json:"collided_at"`
				Days                    int
				RiseBytes               uint64 `json:"rise_bytes"`
			} `json:"cron"`
		} `json:"fixes"`
	}
	if err := json.Unmarshal(js.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	got := doc.Fixes[len(doc.Fixes)-1].Cron
	if len(got) != 2 || got[0].Program != "logrotate" || got[0].Suggested != "25 2 * * *" || got[0].Minutes != 25 || !got[0].At.Equal(at) ||
		got[0].RiseBytes != 400*mib || got[1].Suggested != "" || got[1].Line != 1 {
		t.Errorf("JSON cron %+v", got)
	}
}
