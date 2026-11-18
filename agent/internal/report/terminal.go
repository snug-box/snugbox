package report

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
)

type style int

const (
	plain style = iota
	dim
	strong
	heading
	good
	warn
	bad
)

// Painter adds ANSI colour when it is on. Blueprint section 8.1: colour only
// when writing to a terminal, and never when NO_COLOR is set; the caller
// decides.
type Painter struct{ Color bool }

var ansi = map[style]string{dim: "2", strong: "1", heading: "1", good: "32", warn: "33", bad: "31"}

func (p Painter) paint(s style, text string) string {
	if !p.Color || s == plain || text == "" {
		return text
	}
	return "\x1b[" + ansi[s] + "m" + text + "\x1b[0m"
}

// Dim paints text in the report's dim style, for other commands' output.
func (p Painter) Dim(text string) string { return p.paint(dim, text) }

// Strong paints text in the report's bold style.
func (p Painter) Strong(text string) string { return p.paint(strong, text) }

// Good paints text green, as the report marks a low risk.
func (p Painter) Good(text string) string { return p.paint(good, text) }

// Warn paints text yellow, as the report marks a medium risk.
func (p Painter) Warn(text string) string { return p.paint(warn, text) }

// Bad paints text red, as the report marks a high risk.
func (p Painter) Bad(text string) string { return p.paint(bad, text) }

type cell struct {
	text  string
	style style
	right bool
}

func pad(s string, w int, right bool) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	if right {
		return strings.Repeat(" ", w-n) + s
	}
	return s + strings.Repeat(" ", w-n)
}

// clip shortens a long name for a terminal column. The Markdown and JSON
// reports always carry the full name.
func clip(s string, w int) string {
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w-3]) + "..."
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// tableLines renders rows under a section title, with the column headings on
// the title's line, the way the preview lays out the terminal audit. Rows are
// indented two spaces and columns are two spaces apart; the title takes the
// place of any leading columns without a heading.
func tableLines(p Painter, title string, headings []cell, rows [][]cell) []string {
	cols := len(headings)
	widths := make([]int, cols)
	for i, h := range headings {
		widths[i] = runeLen(h.text)
	}
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], runeLen(c.text))
		}
	}
	starts := make([]int, cols)
	at := 2
	for i, w := range widths {
		starts[i] = at
		at += w + 2
	}
	firstHead := at
	for i := 1; i < cols; i++ {
		if headings[i].text != "" {
			firstHead = starts[i]
			break
		}
	}
	if need := runeLen(title) + 1; need > firstHead {
		widths[0] += need - firstHead
		for i := 1; i < cols; i++ {
			starts[i] += need - firstHead
		}
	}

	var head strings.Builder
	head.WriteString(p.paint(heading, title))
	cur := runeLen(title)
	for i := 1; i < cols; i++ {
		h := headings[i]
		if h.text == "" {
			continue
		}
		pos := starts[i]
		if h.right {
			pos += widths[i] - runeLen(h.text)
		}
		pos = max(pos, cur+1)
		head.WriteString(p.paint(dim, strings.Repeat(" ", pos-cur)+h.text))
		cur = pos + runeLen(h.text)
	}
	out := []string{head.String()}
	for _, r := range rows {
		parts := make([]string, len(r))
		for i, c := range r {
			text := c.text
			if i < len(r)-1 || c.right {
				text = pad(text, widths[i], c.right)
			}
			parts[i] = p.paint(c.style, text)
		}
		out = append(out, strings.TrimRight("  "+strings.Join(parts, "  "), " "))
	}
	return out
}

// Terminal writes the audit report for a terminal. saved, when not empty, is
// where the full Markdown report was written.
func Terminal(w io.Writer, r *audit.Result, version string, p Painter, saved string) error {
	var b strings.Builder
	line := func(parts ...string) { b.WriteString(strings.Join(parts, "") + "\n") }

	line(p.paint(dim, fmt.Sprintf("Snugbox %s, audit of %s at %s", version, hostLabel(r), Stamp(r.Ended))))
	if r.Watched {
		line(p.paint(dim, fmt.Sprintf("Read-only. Uses %s from snugbox watch. Changes nothing, sends no data.", Span(r.Summary.Window()))))
	} else {
		line(p.paint(dim, fmt.Sprintf("Read-only. Sampled for %s. Changes nothing, sends no data.", Span(r.Summary.Window()))))
	}
	line()
	for _, l := range summaryLines(r, p) {
		line(l)
	}
	line()
	line(p.paint(heading, "RISKS"))
	for _, s := range RiskLines(r) {
		line(p.paint(warn, "  !  "), s)
	}
	if ok := AllClear(r); ok != "" {
		line(p.paint(good, "  ok "), ok)
	}
	line()
	for _, l := range workloadLines(r, p) {
		line(l)
	}
	line()
	for _, l := range fixLines(r, p) {
		line(l)
	}
	line()
	for _, l := range planLines(r, p) {
		line(l)
	}
	line()
	line(p.paint(heading, "DATA QUALITY"))
	for _, s := range DataQuality(r) {
		line("  ", s)
	}
	line()
	full := "Full report: snugbox audit --markdown"
	if saved != "" {
		full = "Full report saved to " + saved
	}
	if n := len(r.Risks); n > 0 {
		line(p.paint(warn, Count(n, "risk", "risks")+" found"), p.paint(dim, " (exit code 3). "+full))
	} else {
		line(p.paint(good, "No risks found."), p.paint(dim, " "+full))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func hostLabel(r *audit.Result) string {
	if r.Machine.Hostname != "" {
		return r.Machine.Hostname
	}
	return "this box"
}

func summaryLines(r *audit.Result, p Painter) []string {
	a, e := r.Advice, r.Estimate
	type row struct {
		label, value string
		vstyle       style
		detail       string
	}
	pays := row{PaysLabel(a), GiB(a.PaysFor), strong, payDetail(r)}
	needs := row{"Needs", GiB(e.Snug), warn, SnugParts(e)}
	waste := row{"Waste", GiB(a.Waste), strong, ""}
	if a.Waste == 0 {
		waste.value = "none"
	}
	fits := row{"Fits", "", good, ""}
	switch {
	case a.Withheld:
		fits.value, fits.vstyle, fits.detail = "held back", warn, "fix the risks below first; no smaller plan is advised"
	case !a.HasSizeClass:
		fits.value, fits.vstyle, fits.detail = "none", bad, "no common size holds this box"
	default:
		fits.value, fits.detail = GiB(a.SizeClass), fitDetail(r)
	}
	conf := row{"Confidence", e.Confidence.String(), good, ConfidenceDetail(e)}
	if e.Confidence == model.ConfidenceLow {
		conf.vstyle = warn
	}
	rows := []row{pays, needs, waste, fits, conf}
	vw := 0
	for _, r := range rows {
		vw = max(vw, utf8.RuneCountInString(r.value))
	}
	vw = max(vw, 8)
	out := []string{p.paint(heading, "SUMMARY")}
	for _, r := range rows {
		l := "  " + pad(r.label, 12, false) + p.paint(r.vstyle, pad(r.value, vw, false))
		if r.detail != "" {
			l += "  " + p.paint(dim, r.detail)
		}
		out = append(out, strings.TrimRight(l, " "))
	}
	return out
}

func payDetail(r *audit.Result) string {
	a := r.Advice
	if a.Current != nil && a.Catalogue != nil {
		return fmt.Sprintf("%s, %s a month", PlanName(a.Catalogue, *a.Current), a.Catalogue.Total(*a.Current))
	}
	return "the memory the kernel sees; the plan is unknown"
}

// fitDetail names the best plan to move to: the cheapest on the box's own
// provider, which is usually the least work, or the cheapest overall.
func fitDetail(r *audit.Result) string {
	a := r.Advice
	if len(a.Options) == 0 {
		return ""
	}
	best := a.Options[0]
	for _, o := range a.Options {
		if o.Plan.ProviderID == r.Provider.Provider {
			best = o
			break
		}
	}
	name := best.Plan.Label
	if best.Plan.ProviderID != r.Provider.Provider {
		name = PlanName(a.Catalogue, best.Plan)
	}
	s := name + ", " + MoveText(best)
	if best.HasSaving {
		switch {
		case best.Saving > 0:
			s += ", saves " + best.Saving.String() + " a month"
		case best.Saving < 0:
			s += ", costs " + (-best.Saving).String() + " a month more"
		}
	}
	return s
}

// tinyWorkload is the working set below which the terminal folds a workload
// into one line. The Markdown and JSON reports list every workload.
const tinyWorkload = model.MiB

func workloadLines(r *audit.Result, p Painter) []string {
	ids := sortedIDs(r.Workloads)
	slices.SortStableFunc(ids, func(a, b string) int {
		wa, wb := r.Summary.Workloads[a].WorkingSetQ99, r.Summary.Workloads[b].WorkingSetQ99
		switch {
		case wa > wb:
			return -1
		case wa < wb:
			return 1
		}
		return strings.Compare(a, b)
	})
	var rows [][]cell
	folded := 0
	for _, id := range ids {
		w, s := r.Workloads[id], r.Summary.Workloads[id]
		if s.WorkingSetQ99 < tinyWorkload {
			folded++
			continue
		}
		limit := cell{text: "none", style: warn}
		if w.Max.Set {
			limit = cell{text: MiB(w.Max.Bytes)}
		}
		rows = append(rows, []cell{
			{text: clip(w.Label(), 32)},
			{text: clip(strings.Join(w.Programs, ", "), 18)},
			{text: MiB(s.WorkingSetQ99), right: true},
			{text: MiB(s.WorkingSetMax), right: true},
			limit,
		})
	}
	k := r.Summary.KernelOther
	rows = append(rows, []cell{
		{text: "kernel and other", style: dim}, {style: dim},
		{text: MiB(k.WorkingSetQ99), style: dim, right: true},
		{text: MiB(k.WorkingSetMax), style: dim, right: true},
		{style: dim},
	})
	heads := []cell{{}, {text: "program"}, {text: "99th pct", right: true}, {text: "peak", right: true}, {text: "limit"}}
	out := tableLines(p, "WORKLOADS", heads, rows)
	if folded == 1 {
		out = append(out, "  "+p.paint(dim, "Not shown: 1 workload under 1 MiB. The Markdown report lists it."))
	} else if folded > 1 {
		out = append(out, "  "+p.paint(dim, fmt.Sprintf("Not shown: %d workloads under 1 MiB each. The Markdown report lists them all.", folded)))
	}
	if idle := IdleLine(r); idle != "" {
		out = append(out, "  "+idle)
	}
	return out
}

func riskStyle(rk fix.Risk) style {
	switch rk {
	case fix.RiskLow:
		return good
	case fix.RiskMedium:
		return warn
	}
	return bad
}

func fixLines(r *audit.Result, p Painter) []string {
	if len(r.Fixes) == 0 {
		out := []string{p.paint(heading, "FIXES"), "  " + NoFixes(r)}
		for _, n := range FixAdvice(r) {
			out = append(out, "  "+p.paint(dim, n))
		}
		return out
	}
	var rows [][]cell
	for i, f := range r.Fixes {
		rows = append(rows, []cell{{text: fmt.Sprint(i + 1)}, {text: FixTitle(f)}, {text: string(f.Risk), style: riskStyle(f.Risk)}})
	}
	out := tableLines(p, "FIXES", []cell{{}, {}, {text: "risk"}}, rows)
	n := 1
	if len(r.Fixes) > 1 {
		n = 2
	}
	out = append(out, "  "+p.paint(dim, fmt.Sprintf("Preview one with snugbox fit --dry-run %d. Apply it with sudo snugbox fit %d.", n, n)))
	if b := FixesBlocked(r); b != "" {
		out = append(out, "  "+p.paint(warn, b))
	}
	if hasLowConfidenceLimits(r.Fixes) {
		out = append(out, "  "+p.paint(dim, "Limits from under 7 days of data are early estimates; snugbox fit asks before using them."))
	}
	for _, note := range DatabaseNotes(r) {
		out = append(out, "  "+p.paint(dim, "Note: "+note))
	}
	for _, n := range FixAdvice(r) {
		out = append(out, "  "+p.paint(dim, n))
	}
	return out
}

func hasLowConfidenceLimits(fs []fix.Proposal) bool {
	for _, f := range fs {
		for _, l := range f.Limits {
			if l.LowConfidence {
				return true
			}
		}
	}
	return false
}

func planLines(r *audit.Result, p Painter) []string {
	a := r.Advice
	switch {
	case a.Catalogue == nil:
		return []string{p.paint(heading, "PLANS THAT FIT"), "  The price catalogue could not be read."}
	case a.Withheld:
		return []string{p.paint(heading, "PLANS THAT FIT"), "  None offered. This box shows signs it needs more memory or a fix, so Snugbox won't suggest a smaller plan."}
	case len(a.Options) == 0:
		return []string{p.paint(heading, "PLANS THAT FIT"), "  No plan in the catalogue fits this box."}
	}
	showSaving := a.Current != nil
	var rows [][]cell
	for _, o := range a.Options {
		name := o.Plan.ProviderID
		if prov, ok := a.Catalogue.Provider(o.Plan.ProviderID); ok {
			name = prov.Name
		}
		row := []cell{{text: name}, {text: o.Plan.Label}, {text: o.Total.String(), right: true}}
		if showSaving {
			sv := cell{text: o.Saving.String(), right: true, style: good}
			if o.Saving <= 0 {
				sv.style = plain
			}
			row = append(row, sv)
		}
		row = append(row, cell{text: MoveText(o)})
		rows = append(rows, row)
	}
	heads := []cell{{}, {text: "plan"}, {text: "a month", right: true}}
	if showSaving {
		heads = append(heads, cell{text: "saving", right: true})
	}
	heads = append(heads, cell{text: "how"})
	out := tableLines(p, "PLANS THAT FIT", heads, rows)
	if note := UnavailableNote(a); note != "" {
		out = append(out, "  "+p.paint(dim, note))
	}
	for _, n := range PriceNote(a.Catalogue, anyConverted(a.Options, a.Unavailable)) {
		out = append(out, "  "+p.paint(dim, n))
	}
	return out
}

// DataQuality says how long Snugbox watched, what it could not read and why.
func DataQuality(r *audit.Result) []string {
	s := r.Summary
	var out []string
	gap := "with no gaps"
	if s.Gaps > 0 {
		gap = "with " + Span(s.Gaps) + " missing"
	}
	if r.Watched {
		out = append(out, fmt.Sprintf("Watched for %s by snugboxd, %s (%s).", Span(s.Window()), gap, Count(s.Samples, "sample", "samples")))
	} else {
		out = append(out, fmt.Sprintf("Sampled for %s, every %s, %s (%s).", Span(s.Window()), Span(audit.Interval(s.Window())), gap, Count(s.Samples, "sample", "samples")))
	}
	m := r.Machine
	var have []string
	have = append(have, "kernel "+m.Kernel.String())
	if m.CgroupV2 {
		have = append(have, "cgroup v2")
	}
	if m.PSI {
		have = append(have, "pressure files")
	}
	if m.Peak {
		have = append(have, "memory.peak")
	}
	out = append(out, upperFirst(List(have))+".")
	if len(r.Gaps) == 0 {
		out = append(out, "Could not read: nothing.")
	}
	for _, g := range r.Gaps {
		out = append(out, fmt.Sprintf("Could not read %s: %s.", g.What, g.Why))
	}
	if r.Estimate.CacheOverestimate {
		out = append(out, "File cache was counted in full for workloads that never had to give memory back, so the real need may be lower.")
	}
	if r.Estimate.PeakAddition > 0 {
		out = append(out, fmt.Sprintf("memory.peak shows the workloads used %s more before or between samples than Snugbox saw; headroom includes it.", Size(r.Estimate.PeakAddition)))
	}
	switch {
	case r.Offline:
		out = append(out, "Ran offline: the cloud metadata service was not asked, so the provider is unknown.")
	case r.Provider.FromMetadata:
		out = append(out, "Provider and region came from the cloud metadata service at 169.254.169.254. Skip it with --offline.")
	case r.Provider.Known():
		out = append(out, "Provider guessed from the firmware's DMI strings; the metadata service did not answer.")
	default:
		out = append(out, "Provider unknown: no metadata service answered and the DMI strings name none Snugbox knows.")
	}
	if !r.Root {
		out = append(out, "Ran without root, so some files were skipped. Run sudo snugbox audit for the full report.")
	}
	return out
}

func sortedIDs[V any](m map[string]V) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
