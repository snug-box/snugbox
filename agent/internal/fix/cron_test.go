package fix

import (
	"slices"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/cron"
	"snugbox.dev/snugbox/agent/internal/model"
)

const cronTab = `0 2 * * * root /usr/local/bin/backup.sh --to /srv/backup
0 2 * * * root /usr/sbin/logrotate /etc/logrotate.conf
*/5 * * * * root /usr/local/bin/health
30 4 * * * root /usr/local/bin/report
`

var cronNow = time.Date(2026, 11, 1, 6, 0, 0, 0, time.UTC)

// minutes is a week of the box's highest working set per minute: 800 MiB,
// and high MiB more for each spike, which starts at its time and lasts for
// long minutes.
func minutes(spikes map[time.Time]int, high uint64) []model.MinuteTotal {
	var out []model.MinuteTotal
	for t := cronNow.Add(-ScheduleWindow - time.Hour); !t.After(cronNow); t = t.Add(time.Minute) {
		m := model.MinuteTotal{Time: t, Total: 800 * model.MiB}
		for at, long := range spikes {
			if !t.Before(at) && t.Before(at.Add(time.Duration(long)*time.Minute)) {
				m.Total += high
			}
		}
		out = append(out, m)
	}
	return out
}

func cronInput(spikes map[time.Time]int, high uint64, runs ...model.Run) Input {
	return Input{
		Machine: collect.Machine{MemTotal: 2 * model.GiB}, Now: cronNow, Zone: time.UTC,
		Cron:    cron.Parse("/etc/cron.d/shop", []byte(cronTab), true),
		Minutes: minutes(spikes, high), Runs: runs,
	}
}

// at2 is 02:00 UTC on a day before cronNow.
func at2(daysAgo int) time.Time {
	return time.Date(2026, 11, 1, 2, 0, 0, 0, time.UTC).AddDate(0, 0, -daysAgo)
}

func TestCronJobsThatStartTogetherAreSpread(t *testing.T) {
	in := cronInput(map[time.Time]int{at2(1): 20, at2(3): 20, at2(5): 12}, 400*model.MiB)
	got := cronMoves(in)
	if len(got) != 1 {
		t.Fatalf("moves %+v", got)
	}
	c := got[0]
	want := CronMove{File: "/etc/cron.d/shop", Line: 2, Program: "logrotate", Schedule: "0 2 * * *", Minutes: 25, Suggested: "25 2 * * *",
		With: "backup.sh", At: at2(3), Days: 3, Rise: 400 * model.MiB}
	if c != want {
		t.Errorf("move\n got %+v\nwant %+v", c, want)
	}
	advice := CronAdvice(c)
	for _, part := range []string{`logrotate, line 2 of /etc/cron.d/shop: change its schedule from "0 2 * * *" to "25 2 * * *", 25 minutes later.`,
		"started at 02:00 together with backup.sh on 3 of the last 7 days", "rose 400 MiB"} {
		if !strings.Contains(advice, part) {
			t.Errorf("advice %q has no %q", advice, part)
		}
	}
	if strings.Contains(advice, "/srv/backup") || strings.Contains(advice, "logrotate.conf") {
		t.Errorf("the advice shows a command's arguments: %q", advice)
	}
}

func TestCronNeedsTwoHeavyDays(t *testing.T) {
	for name, in := range map[string]Input{
		"one day":     cronInput(map[time.Time]int{at2(2): 20}, 400*model.MiB),
		"small rises": cronInput(map[time.Time]int{at2(1): 20, at2(2): 20}, 150*model.MiB),
		"no history":  {Machine: collect.Machine{MemTotal: 2 * model.GiB}, Now: cronNow, Cron: cron.Parse("/etc/crontab", []byte(cronTab), true)},
	} {
		if got := cronMoves(in); len(got) != 0 {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// A job that starts with a timer's job moves, and the timer's job keeps its
// time; the timer fix handles timers.
func TestCronJobMovesForATimer(t *testing.T) {
	in := cronInput(map[time.Time]int{at2(1): 30, at2(2): 30}, 500*model.MiB)
	in.Cron = cron.Parse("/var/spool/cron/crontabs/ana", []byte("0 2 * * * /home/ana/bin/sync.sh\n"), false)
	for d := range 7 {
		start := at2(d).Add(40 * time.Second)
		in.Runs = append(in.Runs, model.Run{Name: "backup.service", Start: start, End: start.Add(25 * time.Minute), Peak: 300 * model.MiB})
	}
	got := cronMoves(in)
	if len(got) != 1 || got[0].Program != "sync.sh" || got[0].With != "backup.service" || got[0].Minutes != 35 || got[0].Suggested != "35 2 * * *" {
		t.Fatalf("moves %+v", got)
	}
}

// A move that would start the job together with another heavy one goes a
// step further.
func TestCronMoveSkipsAnotherHeavyStart(t *testing.T) {
	tab := cronTab + "25 2 * * * root /usr/local/bin/export\n25 2 * * * root /usr/local/bin/import\n"
	in := cronInput(map[time.Time]int{at2(1): 20, at2(3): 20, at2(1).Add(25 * time.Minute): 10, at2(3).Add(25 * time.Minute): 10}, 400*model.MiB)
	in.Cron = cron.Parse("/etc/crontab", []byte(tab), true)
	got := cronMoves(in)
	byProgram := map[string]int{}
	for _, c := range got {
		byProgram[c.Program] = c.Minutes
	}
	if byProgram["logrotate"] != 30 || byProgram["import"] != 15 || len(got) != 2 {
		t.Errorf("moves %+v", got)
	}
}

func TestCronAdviceWithoutARewrite(t *testing.T) {
	c := CronMove{File: "/etc/crontab", Line: 9, Program: "pg_dump", Schedule: "0,30 2 * * 1-5", Minutes: 25,
		With: "backup.sh", At: at2(1), Days: 2, Rise: 300 * model.MiB}
	if got := CronAdvice(c); !strings.Contains(got, `start it 25 minutes later; Snugbox can't rewrite a schedule like "0,30 2 * * 1-5" for you.`) {
		t.Errorf("advice %q", got)
	}
}

// A schedule fix with only cron jobs in it is advice; the helper gets
// nothing.
func TestCronOnlyProposalIsAdvice(t *testing.T) {
	in := cronInput(map[time.Time]int{at2(1): 20, at2(3): 20}, 400*model.MiB)
	ps := Propose(in)
	i := slices.IndexFunc(ps, func(p Proposal) bool { return p.Kind == KindSchedule })
	if i < 0 || len(ps[i].Shifts) != 0 || len(ps[i].Cron) != 1 {
		t.Fatalf("proposals %+v", ps)
	}
	f, advice, err := Request(ps[i], nil, RequestOptions{})
	if f != nil || err != nil || len(advice) != 1 || !strings.Contains(advice[0], "logrotate") {
		t.Errorf("request %v, %q, %v", f, advice, err)
	}
	if _, _, err := Request(ps[i], nil, RequestOptions{Only: []string{"nothing.service"}}); err == nil {
		t.Error("--only naming nothing in the fix was accepted")
	}
}
