package fix

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
)

// CronMove is a cron job the schedule fix suggests starting later, because
// it keeps starting in the same minute as another heavy job. Snugbox never
// edits a crontab: the move is advice for a person.
type CronMove struct {
	// File and Line say where the job is written, Program what it runs,
	// and Schedule its schedule as written.
	File     string
	Line     int
	Program  string
	Schedule string
	// Minutes is how much later it should start, and Suggested the schedule
	// that does that, or "" when Snugbox can't rewrite one like it.
	Minutes   int
	Suggested string
	// With is the job that keeps its time, At when the two started on the
	// day the box's memory rose most, Days on how many days of the window
	// they started together, and Rise how far the box's working set rose
	// within CronSpike of that start.
	With string
	At   time.Time
	Days int
	Rise uint64
}

const (
	// CronSpike is how long after jobs start together Snugbox looks for the
	// box's memory to peak.
	CronSpike = 10 * time.Minute
	// CronOften is the shortest period of a job that counts: one that runs
	// more often meets every other job, and is rarely the heavy one.
	CronOften = time.Hour
)

type starter struct {
	cron    int
	service string
}

// cronMoves finds cron jobs that started in the same minute as another job,
// cron's or a timer's, on at least CollideDays days of the window, each time
// with the box's working set rising by at least HeavyShare of RAM within
// CronSpike. The jobs of each such group are spread out: a timer's job keeps
// its time, and so does the first cron job in the files when there is none.
// The other cron jobs start later, each by as long as memory stayed high,
// rounded as shiftFor rounds, after the one before. A move that would start
// a job together with another heavy job, take it past two hours, or reach
// its own next run is left out. Jobs that run more often than CronOften are
// left out altogether.
//
// Unlike a timer, a cron job can move later: cron starts a job at the time
// its crontab says, with no memory of its last run.
func cronMoves(in Input) []CronMove {
	if len(in.Cron) == 0 || len(in.Minutes) == 0 || in.Machine.MemTotal == 0 {
		return nil
	}
	zone := in.Zone
	if zone == nil {
		zone = time.UTC
	}
	from := in.Now.Add(-ScheduleWindow)
	total := make(map[int64]uint64, len(in.Minutes))
	for _, m := range in.Minutes {
		total[m.Time.Unix()/60] = m.Total
	}

	fires := make([][]int64, len(in.Cron))
	for t := from.In(zone).Truncate(time.Minute); t.Before(in.Now); t = t.Add(time.Minute) {
		if t.Before(from) {
			continue
		}
		for i, j := range in.Cron {
			if j.StartsAt(t) {
				fires[i] = append(fires[i], t.Unix()/60)
			}
		}
	}
	starts := make(map[int64][]starter)
	for i, fs := range fires {
		if periodOf(fs) < CronOften {
			continue
		}
		for _, m := range fs {
			starts[m] = append(starts[m], starter{cron: i})
		}
	}
	runs := make(map[string][]int64)
	for _, r := range in.Runs {
		if !r.Start.Before(from) && r.End.Sub(r.Start) <= MaxJob && strings.HasSuffix(r.Name, ".service") {
			runs[r.Name] = append(runs[r.Name], r.Start.Unix()/60)
		}
	}
	for name, ms := range runs {
		if periodOf(ms) < CronOften {
			continue
		}
		for _, m := range ms {
			// The watcher sees a new cgroup up to a minute after it starts.
			for _, at := range []int64{m, m - 1} {
				if len(starts[at]) > 0 && starts[at][0].cron >= 0 {
					starts[at] = append(starts[at], starter{cron: -1, service: name})
					break
				}
			}
		}
	}

	groups := cronGroups(in, starts, total, zone)
	heavy := make(map[starter]bool)
	for _, g := range groups {
		for _, i := range g.cron {
			heavy[starter{cron: i}] = true
		}
		for _, s := range g.services {
			heavy[starter{cron: -1, service: s}] = true
		}
	}
	moved := make(map[int]CronMove)
	for _, g := range groups {
		movers, stays := g.cron, ""
		if len(g.services) > 0 {
			stays = g.services[0]
		} else {
			stays, movers = in.Cron[movers[0]].Program, movers[1:]
		}
		step, minutes := shiftFor(g.high), 0
		for _, i := range movers {
			minutes += step
			for minutes <= helperapi.MaxShiftMinutes && landsOnHeavy(fires[i], minutes, i, fires, runs, heavy) {
				minutes += int(ShiftStep / time.Minute)
			}
			if minutes > helperapi.MaxShiftMinutes || time.Duration(minutes)*time.Minute >= periodOf(fires[i]) {
				continue
			}
			j := in.Cron[i]
			c := CronMove{File: j.File, Line: j.Line, Program: j.Program, Schedule: j.Spec, Minutes: minutes,
				With: stays, At: g.at, Days: len(g.days), Rise: g.rise}
			c.Suggested, _ = j.Later(minutes)
			if old, ok := moved[i]; !ok || c.Rise > old.Rise {
				moved[i] = c
			}
		}
	}
	out := slices.Collect(maps.Values(moved))
	slices.SortFunc(out, func(a, b CronMove) int { return cmp.Or(strings.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line)) })
	return out
}

// cronGroup is a set of jobs that started together, the days they did with
// the box's memory rising, and the worst of those starts.
type cronGroup struct {
	cron     []int
	services []string
	days     map[string]bool
	rise     uint64
	at       time.Time
	// high is how long the box's memory stayed above half of the rise.
	high time.Duration
}

// cronGroups gathers the minutes in which a cron job started together with
// another job, keeps those after which the box's memory rose by HeavyShare
// of RAM, and returns the groups of jobs that did so on CollideDays days or
// more, in the order of their first job.
func cronGroups(in Input, starts map[int64][]starter, total map[int64]uint64, zone *time.Location) []*cronGroup {
	minutes := slices.Sorted(maps.Keys(starts))
	byKey := make(map[string]*cronGroup)
	var keys []string
	for _, m := range minutes {
		var g cronGroup
		for _, s := range starts[m] {
			if s.cron >= 0 && !slices.Contains(g.cron, s.cron) {
				g.cron = append(g.cron, s.cron)
			} else if s.cron < 0 && !slices.Contains(g.services, s.service) {
				g.services = append(g.services, s.service)
			}
		}
		if len(g.cron) == 0 || len(g.cron)+len(g.services) < 2 {
			continue
		}
		base, ok := total[m-1]
		if !ok {
			continue
		}
		var peak uint64
		for k := m; k <= m+int64(CronSpike/time.Minute); k++ {
			peak = max(peak, total[k])
		}
		if peak <= base || float64(peak-base) < HeavyShare*float64(in.Machine.MemTotal) {
			continue
		}
		slices.Sort(g.cron)
		slices.Sort(g.services)
		key := fmt.Sprint(g.cron, g.services)
		have := byKey[key]
		if have == nil {
			g.days = make(map[string]bool)
			have = &g
			byKey[key] = have
			keys = append(keys, key)
		}
		at := time.Unix(m*60, 0).In(zone)
		have.days[at.Format(time.DateOnly)] = true
		rise := peak - base
		n := 0
		for k := m; n < helperapi.MaxShiftMinutes && total[k] >= base+rise/2; k++ {
			n++
		}
		// The worst start is the highest rise, and of equal ones the
		// longest, so a move covers it.
		if high := time.Duration(n) * time.Minute; rise > have.rise || rise == have.rise && high > have.high {
			have.rise, have.at, have.high = rise, at, high
		}
	}
	var out []*cronGroup
	for _, k := range keys {
		if g := byKey[k]; len(g.days) >= CollideDays {
			out = append(out, g)
		}
	}
	slices.SortStableFunc(out, func(a, b *cronGroup) int { return cmp.Compare(a.cron[0], b.cron[0]) })
	return out
}

// landsOnHeavy reports whether job i, started minutes later, would start
// within CollideWithin of another heavy job.
func landsOnHeavy(own []int64, minutes, i int, fires [][]int64, runs map[string][]int64, heavy map[starter]bool) bool {
	near := func(ms []int64) bool {
		for _, f := range own {
			at := f + int64(minutes)
			for _, m := range ms {
				if d := m - at; d >= -1 && d <= 1 {
					return true
				}
			}
		}
		return false
	}
	for k, fs := range fires {
		if k != i && heavy[starter{cron: k}] && near(fs) {
			return true
		}
	}
	for name, ms := range runs {
		if heavy[starter{cron: -1, service: name}] && near(ms) {
			return true
		}
	}
	return false
}

// periodOf is period for starts given as minutes since the epoch, in any
// order.
func periodOf(minutes []int64) time.Duration {
	sorted := slices.Sorted(slices.Values(minutes))
	starts := make([]time.Time, len(sorted))
	for i, m := range sorted {
		starts[i] = time.Unix(m*60, 0)
	}
	return period(starts)
}

// CronAdvice says what a person changes in a crontab for a CronMove, and
// why.
func CronAdvice(c CronMove) string {
	where := fmt.Sprintf("%s, line %d of %s", c.Program, c.Line, c.File)
	why := fmt.Sprintf("It started at %s together with %s on %d of the last %d days, and the box's memory rose %d MiB within minutes.",
		c.At.Format("15:04"), c.With, c.Days, int(ScheduleWindow/(24*time.Hour)), c.Rise/model.MiB)
	if c.Suggested == "" {
		return fmt.Sprintf("%s: start it %d minutes later; Snugbox can't rewrite a schedule like %q for you. %s", where, c.Minutes, c.Schedule, why)
	}
	return fmt.Sprintf("%s: change its schedule from %q to %q, %d minutes later. %s", where, c.Schedule, c.Suggested, c.Minutes, why)
}
