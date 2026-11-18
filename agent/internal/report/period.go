package report

import (
	"cmp"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/plan"
)

// Period writes snugbox report: what the box needs now and at the start of
// the period, the saving the watcher saw happen, the fixes and the events.
// r is the watcher's latest analysis, for what the box pays for; it may be
// nil. A saving counts only when the box moved to a cheaper plan, which the
// watcher saw as a change of its size (blueprint section 7.5).
func Period(w io.Writer, rep localapi.Report, r *audit.Result, p Painter) error {
	var out []string
	add := func(title string, lines ...string) {
		out = append(out, p.paint(heading, title))
		for _, l := range lines {
			out = append(out, "  "+l)
		}
		out = append(out, "")
	}
	add("SINCE", sinceLines(rep)...)
	add("MEMORY", memoryLines(rep, r)...)
	add("SAVED", savedLines(rep)...)
	add("FIXES", fixLinesFor(rep, p)...)
	add("EVENTS", eventLines(rep)...)
	_, err := io.WriteString(w, strings.Join(out, "\n"))
	return err
}

func sinceLines(rep localapi.Report) []string {
	lines := []string{fmt.Sprintf("%s, the last %s.", Stamp(rep.From.Local()), Span(rep.To.Sub(rep.From)))}
	if rep.WatchStart.After(rep.From) {
		lines = append(lines, "The watcher has history only from "+Stamp(rep.WatchStart.Local())+".")
	}
	return lines
}

func memoryLines(rep localapi.Report, r *audit.Result) []string {
	if rep.Now == nil {
		return []string{"No analysis yet; the first runs a few minutes after the watcher starts."}
	}
	now := fmt.Sprintf("Needs %s now, %s confidence.", GiB(rep.Now.Snug), rep.Now.Confidence)
	switch {
	case rep.Then == nil || rep.Then.Snug == rep.Now.Snug:
		now += " The same as at the start."
	default:
		now += fmt.Sprintf(" It needed %s at the start.", GiB(rep.Then.Snug))
	}
	lines := []string{now}
	if r != nil {
		lines = append(lines, Headline(r))
	}
	return lines
}

// planLabel names a catalogue plan and its price for a resize, or says the
// plan is unknown.
func planLabel(c *plan.Catalogue, provider, id string, cents int64) string {
	if c != nil && id != "" {
		if prov, ok := c.Provider(provider); ok {
			for _, pl := range prov.Plans {
				if pl.ID == id {
					return fmt.Sprintf("%s (%s a month)", PlanName(c, pl), plan.Money(cents))
				}
			}
		}
	}
	return "a plan Snugbox doesn't know"
}

func savedLines(rep localapi.Report) []string {
	c, _ := plan.Embedded()
	var saved int64
	var lines []string
	priced := false
	for _, rz := range rep.Resizes {
		size := fmt.Sprintf("%s to %s of RAM", Size(rz.OldRAM), Size(rz.NewRAM))
		if rz.OldCPUs != rz.NewCPUs {
			size += fmt.Sprintf(", %d to %d CPUs", rz.OldCPUs, rz.NewCPUs)
		}
		if rz.OldCents == 0 || rz.NewCents == 0 {
			lines = append(lines, fmt.Sprintf("On %s the box went from %s. Its plans aren't known, so no saving is counted.", Stamp(rz.Time.Local()), size))
			continue
		}
		priced = true
		saved += rz.OldCents - rz.NewCents
		lines = append(lines, fmt.Sprintf("On %s the box went from %s: from %s to %s.", Stamp(rz.Time.Local()), size,
			planLabel(c, rz.Provider, rz.OldPlan, rz.OldCents), planLabel(c, rz.Provider, rz.NewPlan, rz.NewCents)))
	}
	switch {
	case priced && saved > 0:
		lines = append([]string{fmt.Sprintf("%s a month, %s a year.", plan.Money(saved), plan.Money(12*saved))}, lines...)
	case priced && saved < 0:
		lines = append([]string{fmt.Sprintf("Nothing saved: the box moved to a larger plan, which costs %s more a month.", plan.Money(-saved))}, lines...)
	case priced:
		lines = append([]string{"Nothing saved: the plan costs what it did."}, lines...)
	default:
		lines = append([]string{"Nothing yet. A saving counts once the box moves to a cheaper plan and the watcher sees its new size."}, lines...)
	}
	return lines
}

// active reports whether a fix is still in place and being watched.
func active(state string) bool {
	return state == "verifying" || state == "watching" || state == "reopened"
}

func fixLinesFor(rep localapi.Report, p Painter) []string {
	ch := rep.Changes
	if !ch.Helper && ch.Error != "" {
		return []string{"The fixes can't be listed: " + ch.Error + "."}
	}
	var lines []string
	for _, c := range ch.Changes {
		if c.Started.Before(rep.From) && c.Since.Before(rep.From) && !active(c.State) {
			continue
		}
		line := fmt.Sprintf("%s  %s, %s since %s", c.ID, c.Kind, c.State, Stamp(c.Since.Local()))
		switch {
		case c.Trouble != "":
			line += ": " + c.Trouble
		case c.State == "verifying" && !c.VerifyEnds.IsZero():
			line += fmt.Sprintf(": verified until %s", c.VerifyEnds.Local().Format("15:04"))
		case c.State == "watching" && c.Counted, c.State == "done":
			line += ": kept, a day or more without trouble"
		case c.Reason != "":
			line += ": " + c.Reason
		}
		lines = append(lines, p.paint(stateStyle(c.State), line))
	}
	if len(lines) == 0 {
		return []string{"No fix was applied, undone or kept in this time."}
	}
	return lines
}

func stateStyle(state string) style {
	switch state {
	case "undone", "failed":
		return warn
	case "reopened":
		return bad
	}
	return plain
}

func eventLines(rep localapi.Report) []string {
	kills := map[string]int{}
	var warnings int
	outcomes := map[string]int{}
	var other []string
	for _, e := range rep.Events {
		switch e.Kind {
		case "oom_kill":
			kills[cmp.Or(e.Workload, "the box")]++
		case "warning":
			warnings++
		case "warning_outcome":
			outcomes[e.Detail]++
		case "plan_change_seen":
			other = append(other, fmt.Sprintf("On %s the metadata service named a new provider or region: %s.", Stamp(e.Time.Local()), e.Detail))
		}
	}
	var lines []string
	if n := sum(kills); n > 0 {
		lines = append(lines, fmt.Sprintf("%s: %s.", Count(n, "out-of-memory kill", "out-of-memory kills"), counts(kills)))
	}
	if n := sum(rep.Throttled); n > 0 {
		lines = append(lines, fmt.Sprintf("Held back at memory.high in %s: %s.", Count(n, "sample", "samples"), counts(rep.Throttled)))
	}
	if warnings > 0 {
		line := Count(warnings, "early warning", "early warnings") + "."
		if len(outcomes) > 0 {
			keys := slices.SortedFunc(maps.Keys(outcomes), func(a, b string) int { return cmp.Or(cmp.Compare(outcomes[b], outcomes[a]), strings.Compare(a, b)) })
			parts := make([]string, len(keys))
			for i, k := range keys {
				parts[i] = fmt.Sprintf("%s %d", k, outcomes[k])
			}
			parts[0] += " of them"
			line = fmt.Sprintf("%s; %s.", Count(warnings, "early warning", "early warnings"), strings.Join(parts, ", "))
		}
		lines = append(lines, line)
	}
	lines = append(lines, other...)
	if len(lines) == 0 {
		return []string{"No out-of-memory kill, early warning or memory.high hit in this time."}
	}
	return lines
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// counts writes "app.service 2 and db.service 1", largest first.
func counts(m map[string]int) string {
	keys := slices.SortedFunc(maps.Keys(m), func(a, b string) int { return cmp.Or(cmp.Compare(m[b], m[a]), strings.Compare(a, b)) })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return List(parts)
}

// PeriodSince is the start of the period snugbox report covers: the last
// 24 hours, or the last 7 days with --week.
func PeriodSince(now time.Time, week bool) time.Time {
	if week {
		return now.Add(-7 * 24 * time.Hour)
	}
	return now.Add(-24 * time.Hour)
}
