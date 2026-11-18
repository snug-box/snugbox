package fix

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
)

// Spreading colliding schedules, blueprint section 3.7: heavy jobs that
// keep starting in the same minute push memory up together, and starting
// some of them earlier spreads them out. Starting values for the outside
// review.
const (
	// CollideWithin is how close two runs' starts must be to collide.
	CollideWithin = time.Minute
	// CollideDays is on how many different days in ScheduleWindow two jobs
	// must have collided.
	CollideDays    = 2
	ScheduleWindow = 7 * 24 * time.Hour
	// MaxJob is the longest run that still counts as a job a timer starts;
	// a service that runs longer is not one.
	MaxJob = 6 * time.Hour
	// HeavyShare is the share of RAM two colliding jobs' peaks must reach
	// together to count as pushing memory up.
	HeavyShare = 0.1
	// ShiftStep is the step moves are made in: a job that moves starts its
	// longest run, rounded up to a step, and one more step, before the job
	// after it. No timer moves more than helperapi.MaxShiftMinutes, two hours.
	ShiftStep = 5 * time.Minute
)

// Shift is a proposed move of one timer earlier in the day.
type Shift struct {
	// Timer is the timer to move, Service the job it starts, and Minutes
	// how much earlier it starts it.
	Timer, Service string
	Minutes        int
	// With is the job Service collided with most heavily, Days on how many
	// days in the window the two started together, and Peak their highest
	// working sets added up, on the worst of those days.
	With string
	Days int
	Peak uint64
}

type pairKey struct{ a, b string }

// collision is what two services' runs that started together have shown:
// the days they did and their peaks added up on the worst run.
type collision struct {
	days map[string]bool
	peak uint64
}

// jobs is what the runs in the window show about each service.
type jobs struct {
	longest map[string]time.Duration
	starts  map[string][]time.Time
	peak    map[string]uint64
}

// scheduleShifts finds services whose runs started within a minute of each
// other on at least CollideDays days of the last ScheduleWindow, with peaks
// that reached HeavyShare of RAM together, and proposes spreading them out.
// Jobs joined by such collisions form a group, laid out so each ends before
// the next starts. The jobs that can't move keep their time; when every job
// can, the one that runs longest keeps it. The others start earlier, those
// that run most often nearest that time and then the heaviest, each its
// longest run rounded up to a step, and one more step, before the job after
// it, and a step further while that would start it together with another
// heavy job. Only a job a timer starts can move, never a unit no fix may
// touch or a timer Snugbox moved already, never by more than two hours, and
// never by as long as the job waits between its own runs; a job that
// doesn't fit stays where it is.
//
// Jobs move earlier and never later because systemd counts a timer's next
// run from its last one: a later time would come between the two, and
// systemd would start the job as soon as the move took hold.
//
// The runs of a service from before Snugbox moved its timer are left out:
// they show the old time, and their collisions are over.
//
// The watcher finds new cgroups once a minute, so a run's start is when it
// was first seen, up to a minute late, and two jobs that start together
// are seen together. A job shorter than that may not be seen at all, which
// a heavy job rarely is.
func scheduleShifts(in Input) []Shift {
	var runs []model.Run
	for _, r := range in.Runs {
		moved := in.Timers[r.Name].Moved
		if !r.Start.Before(in.Now.Add(-ScheduleWindow)) && r.End.Sub(r.Start) <= MaxJob && strings.HasSuffix(r.Name, ".service") &&
			(moved.IsZero() || !r.Start.Before(moved)) {
			runs = append(runs, r)
		}
	}
	slices.SortFunc(runs, func(a, b model.Run) int { return a.Start.Compare(b.Start) })
	j := jobs{longest: make(map[string]time.Duration), starts: make(map[string][]time.Time), peak: make(map[string]uint64)}
	for _, r := range runs {
		j.longest[r.Name] = max(j.longest[r.Name], r.End.Sub(r.Start))
		j.starts[r.Name] = append(j.starts[r.Name], r.Start)
		j.peak[r.Name] = max(j.peak[r.Name], r.Peak)
	}
	pairs := make(map[pairKey]*collision)
	for i, a := range runs {
		for _, b := range runs[i+1:] {
			if b.Start.Sub(a.Start) > CollideWithin {
				break
			}
			if a.Name == b.Name {
				continue
			}
			k := pairKey{min(a.Name, b.Name), max(a.Name, b.Name)}
			c := pairs[k]
			if c == nil {
				c = &collision{days: make(map[string]bool)}
				pairs[k] = c
			}
			c.days[a.Start.UTC().Format(time.DateOnly)] = true
			c.peak = max(c.peak, a.Peak+b.Peak)
		}
	}
	parent := make(map[string]string)
	root := func(s string) string {
		for parent[s] != s {
			s = parent[s]
		}
		return s
	}
	worst := make(map[string]Shift)
	for k, c := range pairs {
		if len(c.days) < CollideDays || float64(c.peak) < HeavyShare*float64(in.Machine.MemTotal) {
			continue
		}
		for _, s := range []string{k.a, k.b} {
			if _, ok := parent[s]; !ok {
				parent[s] = s
			}
		}
		parent[root(k.a)] = root(k.b)
		for _, side := range [][2]string{{k.a, k.b}, {k.b, k.a}} {
			w := Shift{Service: side[0], With: side[1], Days: len(c.days), Peak: c.peak}
			if old, ok := worst[side[0]]; !ok || cmp.Or(cmp.Compare(w.Peak, old.Peak), cmp.Compare(w.Days, old.Days), strings.Compare(old.With, w.With)) > 0 {
				worst[side[0]] = w
			}
		}
	}
	groups := make(map[string][]string)
	for s := range worst {
		groups[root(s)] = append(groups[root(s)], s)
	}
	var out []Shift
	for _, members := range groups {
		out = append(out, layOut(in, j, worst, members)...)
	}
	slices.SortFunc(out, func(a, b Shift) int { return strings.Compare(a.Timer, b.Timer) })
	return out
}

// layOut places one group of jobs that start together so that each ends
// before the next starts, as scheduleShifts describes, and returns the
// moves.
func layOut(in Input, j jobs, worst map[string]Shift, members []string) []Shift {
	var fixed, movers []string
	for _, s := range members {
		if t, ok := in.Timers[s]; ok && t.Moved.IsZero() && !helperapi.ProtectedUnit(s) && !helperapi.ProtectedUnit(t.Name) {
			movers = append(movers, s)
		} else {
			fixed = append(fixed, s)
		}
	}
	if len(fixed) == 0 && len(movers) > 0 {
		// The others only need to end before it starts, however long it
		// runs, so the longest job staying moves the rest the least.
		stays := slices.MaxFunc(movers, func(a, b string) int {
			return cmp.Or(cmp.Compare(j.longest[a], j.longest[b]), strings.Compare(b, a))
		})
		movers = slices.DeleteFunc(movers, func(s string) bool { return s == stays })
	}
	slices.SortFunc(movers, func(a, b string) int {
		return cmp.Or(cmp.Compare(period(j.starts[a]).Round(time.Hour), period(j.starts[b]).Round(time.Hour)),
			cmp.Compare(j.peak[b], j.peak[a]), strings.Compare(a, b))
	})
	// before is how many minutes before the group's time the job placed
	// last starts.
	before := 0
	var out []Shift
	for _, s := range movers {
		m := before + shiftFor(j.longest[s])
		for m <= helperapi.MaxShiftMinutes && landsOnAnother(in, j, s, m, members) {
			m += int(ShiftStep / time.Minute)
		}
		if m > helperapi.MaxShiftMinutes || time.Duration(m)*time.Minute >= period(j.starts[s]) {
			continue
		}
		sh := worst[s]
		sh.Timer, sh.Minutes = in.Timers[s].Name, m
		out = append(out, sh)
		before = m
	}
	return out
}

// landsOnAnother reports whether job s, started minutes earlier, would have
// started within CollideWithin of a job outside its group on a run of
// theirs, with peaks that reach HeavyShare of RAM together: the move would
// make a collision of its own.
func landsOnAnother(in Input, j jobs, s string, minutes int, group []string) bool {
	earlier := time.Duration(minutes) * time.Minute
	for other, starts := range j.starts {
		if slices.Contains(group, other) || float64(j.peak[s]+j.peak[other]) < HeavyShare*float64(in.Machine.MemTotal) {
			continue
		}
		for _, at := range j.starts[s] {
			for _, u := range starts {
				if u.Sub(at.Add(-earlier)).Abs() <= CollideWithin {
					return true
				}
			}
		}
	}
	return false
}

// shiftFor is how many minutes before the next job a job that runs for d
// should start: d rounded up to ShiftStep, plus one step, at most two
// hours.
func shiftFor(d time.Duration) int {
	steps := int((d + ShiftStep - 1) / ShiftStep)
	return min((steps+1)*int(ShiftStep/time.Minute), helperapi.MaxShiftMinutes)
}

// period is how long a job usually waits from one start to the next: the
// median gap between its starts, given in order, so a run started by hand
// now and then doesn't change it. A job seen once has no period, and gets
// the whole window.
func period(starts []time.Time) time.Duration {
	if len(starts) < 2 {
		return ScheduleWindow
	}
	gaps := make([]time.Duration, 0, len(starts)-1)
	for i := 1; i < len(starts); i++ {
		gaps = append(gaps, starts[i].Sub(starts[i-1]))
	}
	slices.Sort(gaps)
	return gaps[len(gaps)/2]
}
