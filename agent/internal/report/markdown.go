package report

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/fix"
)

// mdEscape keeps a name from breaking a Markdown table or turning into
// formatting.
func mdEscape(s string) string {
	r := strings.NewReplacer("|", `\|`, "*", `\*`, "_", `\_`, "`", "\\`", "<", "&lt;", ">", "&gt;", "[", `\[`, "]", `\]`)
	return r.Replace(s)
}

// Markdown writes the full audit report as Markdown.
func Markdown(w io.Writer, r *audit.Result, version string) error {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	e, a := r.Estimate, r.Advice

	p("# Snugbox audit of %s", mdEscape(hostLabel(r)))
	p("")
	p("%s", Headline(r))
	p("")
	p("Snugbox %s, %s. Read-only: it changed nothing and sent no data.", version, Stamp(r.Ended))
	p("")

	p("## Summary")
	p("")
	p("| | | |")
	p("| --- | --- | --- |")
	p("| %s | %s | %s |", PaysLabel(a), GiB(a.PaysFor), mdEscape(payDetail(r)))
	p("| Needs | %s | %s |", GiB(e.Snug), SnugParts(e))
	waste := GiB(a.Waste)
	if a.Waste == 0 {
		waste = "none"
	}
	p("| Waste | %s | |", waste)
	switch {
	case a.Withheld:
		p("| Fits | held back | fix the risks first; no smaller plan is advised |")
	case a.HasSizeClass:
		p("| Fits | %s | %s |", GiB(a.SizeClass), mdEscape(fitDetail(r)))
	default:
		p("| Fits | none | no common size holds this box |")
	}
	p("| Confidence | %s | %s |", e.Confidence, ConfidenceDetail(e))
	p("")

	p("## Risks")
	p("")
	lines := RiskLines(r)
	for _, s := range lines {
		p("- %s.", mdEscape(s))
	}
	if ok := AllClear(r); ok != "" {
		p("- %s.", ok)
	}
	if len(lines) == 0 && AllClear(r) == "" {
		p("None found.")
	}
	p("")

	p("## Workloads")
	p("")
	p("Working set is the memory a workload would miss if it were taken away: memory that can't be dropped plus the file cache it keeps rereading.")
	p("")
	p("| Workload | Kind | Program | 95th pct | 99th pct | Peak seen | memory.peak | Limit |")
	p("| --- | --- | --- | ---: | ---: | ---: | ---: | --- |")
	ids := sortedIDs(r.Workloads)
	slices.SortStableFunc(ids, func(x, y string) int {
		a, b := r.Summary.Workloads[x].WorkingSetQ99, r.Summary.Workloads[y].WorkingSetQ99
		switch {
		case a > b:
			return -1
		case a < b:
			return 1
		}
		return 0
	})
	for _, id := range ids {
		w, s := r.Workloads[id], r.Summary.Workloads[id]
		peak := "n/a"
		if s.HasPeak {
			peak = MiB(s.Peak)
		}
		limit := "none"
		if w.Max.Set {
			limit = MiB(w.Max.Bytes)
		}
		p("| %s | %s | %s | %s | %s | %s | %s | %s |", mdEscape(w.Label()), w.Kind, mdEscape(strings.Join(w.Programs, ", ")),
			MiB(s.WorkingSetQ95), MiB(s.WorkingSetQ99), MiB(s.WorkingSetMax), peak, limit)
	}
	k := r.Summary.KernelOther
	p("| Kernel and other | kernel | | %s | %s | %s | | |", MiB(k.WorkingSetQ95), MiB(k.WorkingSetQ99), MiB(k.WorkingSetMax))
	p("")
	if idle := IdleLine(r); idle != "" {
		p("%s", mdEscape(idle))
		p("")
	}

	p("## Fixes")
	p("")
	if len(r.Fixes) == 0 {
		p("%s", NoFixes(r))
		for _, n := range FixAdvice(r) {
			p("")
			p("%s", mdEscape(n))
		}
	} else {
		p("| # | Fix | Risk |")
		p("| ---: | --- | --- |")
		for i, f := range r.Fixes {
			p("| %d | %s | %s |", i+1, mdEscape(FixTitle(f)), f.Risk)
		}
		p("")
		p("Preview a fix with `snugbox fit --dry-run <number>` and apply it with `sudo snugbox fit <number>`. Every fix has an undo: `sudo snugbox undo --last`.")
		if b := FixesBlocked(r); b != "" {
			p("")
			p("%s", b)
		}
		var details []string
		for _, f := range r.Fixes {
			for _, l := range f.Limits {
				conf := ""
				if l.LowConfidence {
					conf = ", an early estimate"
				}
				if f.Kind == fix.KindService {
					details = append(details, fmt.Sprintf("- %s: MemoryMax %s, MemoryHigh %s%s.", mdEscape(l.Name), MiB(l.Max), MiB(l.Soft), conf))
				} else {
					details = append(details, fmt.Sprintf("- %s: limit %s, reservation %s%s.", mdEscape(l.Name), MiB(l.Max), MiB(l.Soft), conf))
				}
			}
			for _, h := range f.Heaps {
				details = append(details, fmt.Sprintf("- %s, %s: %s", mdEscape(h.Name), RuntimeName(h.Runtime), HeapHow(h, "`"+heapSetting(h)+"`")))
			}
			for _, s := range f.Shifts {
				details = append(details, "- "+mdEscape(ShiftLine(s)))
			}
			for _, c := range f.Cron {
				details = append(details, "- "+mdEscape(fix.CronAdvice(c)))
			}
		}
		if len(details) > 0 {
			p("")
			for _, d := range details {
				p("%s", d)
			}
		}
		for _, n := range DatabaseNotes(r) {
			p("")
			p("Note: %s", mdEscape(n))
		}
		for _, n := range FixAdvice(r) {
			p("")
			p("%s", mdEscape(n))
		}
	}
	p("")

	p("## Plans that fit")
	p("")
	switch {
	case a.Catalogue == nil:
		p("The price catalogue could not be read.")
	case a.Withheld:
		p("None offered. This box shows signs it needs more memory or a fix, so Snugbox won't suggest a smaller plan.")
	case len(a.Options) == 0:
		p("No plan in the catalogue fits this box.")
	default:
		p("| Provider | Plan | A month | Saving | How |")
		p("| --- | --- | ---: | ---: | --- |")
		for _, o := range a.Options {
			name := o.Plan.ProviderID
			if prov, ok := a.Catalogue.Provider(o.Plan.ProviderID); ok {
				name = prov.Name
			}
			saving := "unknown"
			if o.HasSaving {
				saving = o.Saving.String()
			}
			p("| %s | %s | %s | %s | %s |", name, mdEscape(o.Plan.Label), o.Total, saving, MoveText(o))
		}
		p("")
		if note := UnavailableNote(a); note != "" {
			p("%s", note)
			p("")
		}
		for _, n := range PriceNote(a.Catalogue, anyConverted(a.Options, a.Unavailable)) {
			p("%s", n)
		}
		if a.Current != nil {
			if prov, ok := a.Catalogue.Provider(a.Current.ProviderID); ok {
				p("")
				p("How %s moves a server to a smaller plan: %s", prov.Name, prov.ResizeDoc)
			}
		}
	}
	p("")

	p("## Data quality")
	p("")
	for _, s := range DataQuality(r) {
		p("- %s", mdEscape(s))
	}
	p("")
	p("The memory model behind these numbers is published at https://snugbox.dev/docs/memory-model.")
	_, err := io.WriteString(w, b.String())
	return err
}
