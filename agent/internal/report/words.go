package report

import (
	"fmt"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/plan"
)

// The sentences of the report live here, so the terminal and Markdown forms
// say the same thing in the same words. The voice is blueprint section 4.7's:
// short sentences, real numbers, no hype.

// Headline is the one-line answer: what the box pays for, what it needs and
// what fits.
func Headline(r *audit.Result) string {
	a := r.Advice
	needs := GiB(r.Estimate.Snug)
	pays := "pays for " + GiB(a.PaysFor)
	if !a.PaysForExact {
		pays = "has " + GiB(a.PaysFor)
	}
	switch {
	case a.Withheld:
		return fmt.Sprintf("This box %s and needs %s, but it shows signs of memory trouble, so no smaller plan is advised.", pays, needs)
	case !a.HasSizeClass:
		return fmt.Sprintf("This box %s and needs %s, more than any common plan holds.", pays, needs)
	case !r.Fits():
		return fmt.Sprintf("This box %s and needs %s. It needs a %s plan or a fix.", pays, needs, GiB(a.SizeClass))
	case a.SizeClass >= a.OwnClass(r.Machine.MemTotal):
		return fmt.Sprintf("This box %s and needs %s. It is snug.", pays, needs)
	default:
		return fmt.Sprintf("This box %s and needs %s. A %s plan fits.", pays, needs, GiB(a.SizeClass))
	}
}

// PaysLabel is the summary's first label: "Pays for" when the plan is known,
// "Has" when only the box's memory is.
func PaysLabel(a audit.Advice) string {
	if a.PaysForExact {
		return "Pays for"
	}
	return "Has"
}

// SnugParts writes the sum behind the snug size, in GiB.
func SnugParts(e model.Estimate) string {
	return fmt.Sprintf("snug size: %s need + %s headroom + %s cache", GiBNumber(e.Need), GiBNumber(e.Headroom), GiBNumber(e.Cache))
}

// ConfidenceDetail explains the confidence level.
func ConfidenceDetail(e model.Estimate) string {
	switch e.Confidence {
	case model.ConfidenceHigh:
		return Span(e.Window) + ", " + gapWords(e.Coverage)
	case model.ConfidenceMedium:
		return Span(e.Window) + ", " + gapWords(e.Coverage) + "; the saving firms up at 14 days"
	default:
		return fmt.Sprintf("Early estimate from %s of data. Keep watching for a week before changing plans.", Span(e.Window))
	}
}

func gapWords(coverage float64) string {
	if coverage >= 0.999 {
		return "no gaps"
	}
	return Percent(1-coverage) + " gaps"
}

// RiskText writes one risk as a sentence.
func RiskText(k audit.Risk, r *audit.Result) string {
	switch k.Kind {
	case audit.RiskKills:
		var parts []string
		if k.Count > 0 {
			parts = append(parts, Count(k.Count, "out-of-memory kill", "out-of-memory kills")+" in the last 7 days")
		}
		if k.Undated > 0 {
			parts = append(parts, Count(k.Undated, "kill", "kills")+" since boot at an unknown time, counted as recent")
		}
		s := strings.Join(parts, ", and ")
		if len(k.Names) > 0 {
			s += ": " + names(k.Names)
		}
		return upperFirst(s)
	case audit.RiskPressure:
		basis := "over the window"
		if k.SinceBoot {
			basis = "since boot"
		}
		return fmt.Sprintf("Every task waited for memory %s of the time %s, above the 1%% Snugbox allows", Percent(k.Share), basis)
	case audit.RiskSwapping:
		return fmt.Sprintf("Pages came back from swap in %d of %d sampling intervals", k.Count, k.Of)
	case audit.RiskNoSwap:
		return "No swap"
	case audit.RiskNoOOMProtection:
		return "No early out-of-memory protection, such as systemd-oomd or earlyoom"
	case audit.RiskUnlimited:
		return fmt.Sprintf("%d of %s have no memory limit: %s", k.Count, Count(k.Of, "container", "containers"), names(k.Names))
	case audit.RiskTHPDatabase:
		return fmt.Sprintf("Transparent huge pages are set to %s; %s asks for never or madvise", k.Setting, k.Database)
	case audit.RiskOvercommit:
		if k.Database != "" {
			return fmt.Sprintf("vm.overcommit_memory is %s; %s expects 1", k.Setting, k.Database)
		}
		return fmt.Sprintf("vm.overcommit_memory is %s, so allocations can fail while memory looks free", k.Setting)
	case audit.RiskCrashLoop:
		var each []string
		for i, n := range k.Names {
			if i < len(k.Restarts) {
				each = append(each, fmt.Sprintf("%s (%s)", n, Count(k.Restarts[i], "restart", "restarts")))
			}
		}
		if k.Count == 1 {
			return "A container keeps restarting: " + names(each)
		}
		return fmt.Sprintf("%d containers keep restarting: %s", k.Count, names(each))
	case audit.RiskOrphanBrowsers:
		browser := browserName(k.Setting)
		if k.Count == 1 {
			return fmt.Sprintf("A headless %s is still running after the program that started it stopped: %s holding %s, in %s",
				browser, Count(k.Of, "process", "processes"), Size(k.Bytes), names(k.Names))
		}
		what := fmt.Sprintf("%d headless %s browsers", k.Count, browser)
		if browser == "browser" {
			what = fmt.Sprintf("%d headless browsers", k.Count)
		}
		return fmt.Sprintf("%s are still running after the programs that started them stopped: %s holding %s, in %s",
			what, Count(k.Of, "process", "processes"), Size(k.Bytes), names(k.Names))
	}
	return string(k.Kind)
}

// browserName is a browser's own name for its program, or "browser" when
// several kinds were found.
func browserName(program string) string {
	switch program {
	case "chrome", "headless_shell", "chrome-headless":
		return "Chrome"
	case "chromium", "chromium-browse":
		return "Chromium"
	case "firefox":
		return "Firefox"
	}
	return "browser"
}

// IdleLine names the containers that used almost no CPU over a week or
// more, with what stopping them would give back.
func IdleLine(r *audit.Result) string {
	if len(r.Idle) == 0 {
		return ""
	}
	var each []string
	var total uint64
	for _, w := range r.Idle {
		each = append(each, fmt.Sprintf("%s (%s of CPU in %s)", w.Name, cpuText(w.CPU), Span(w.Observed)))
		total += w.WorkingSet
	}
	return fmt.Sprintf("Idle for a week or more: %s. Together they hold %s.", names(each), Size(total))
}

// cpuText writes a small amount of CPU time.
func cpuText(d time.Duration) string {
	if d < time.Minute {
		return Count(int(d.Seconds()), "second", "seconds")
	}
	return Span(d)
}

// RiskLines merges "no swap" and "no early protection" into one line, as the
// two usually travel together, and returns the risks as sentences.
func RiskLines(r *audit.Result) []string {
	var lines []string
	noSwap, noOOM := false, false
	for _, k := range r.Risks {
		switch k.Kind {
		case audit.RiskNoSwap:
			noSwap = true
		case audit.RiskNoOOMProtection:
			noOOM = true
		}
	}
	for _, k := range r.Risks {
		switch {
		case k.Kind == audit.RiskNoSwap && noOOM:
			lines = append(lines, "No swap, and no early out-of-memory protection")
		case k.Kind == audit.RiskNoOOMProtection && noSwap:
		default:
			lines = append(lines, RiskText(k, r))
		}
	}
	return lines
}

// AllClear is the reassuring line shown when there are no kills and no
// pressure, or "" when there is something to report.
func AllClear(r *audit.Result) string {
	u := r.Estimate.Upward
	if u.Kills() || u.Pressure() {
		return ""
	}
	pressure := "pressure stayed under 1%"
	if u.PressureSinceBoot {
		pressure += " since boot"
	}
	if r.History.OOMKills == 0 && r.History.Uptime > 0 {
		return fmt.Sprintf("No out-of-memory kills since the box booted %s ago, and %s", Span(r.History.Uptime), pressure)
	}
	return "No out-of-memory kills in the last 7 days, and " + pressure
}

// NoFixes is the line shown when no fix is offered.
func NoFixes(r *audit.Result) string {
	if len(r.Risks) == 0 {
		return "None needed. This box is snug."
	}
	return "None that Snugbox can apply here."
}

// FixesBlocked says why none of the listed fixes can run on this box yet,
// or "" when they can. Every fix waits for a calm box, which the helper
// reads from the kernel's pressure files; RHEL 9 and its rebuilds ship with
// them off.
func FixesBlocked(r *audit.Result) string {
	if r.Machine.PSI || len(r.Fixes) == 0 {
		return ""
	}
	return "None of these can run yet: fixes wait for a calm box, and this kernel has pressure stall information off. Boot with psi=1; snugbox doctor says how."
}

// FixAdvice covers risks no catalogue fix answers on this box, with what a
// person can do instead.
func FixAdvice(r *audit.Result) []string {
	var out []string
	for _, k := range r.Risks {
		if k.Kind != audit.RiskNoOOMProtection {
			continue
		}
		offered := false
		for _, f := range r.Fixes {
			offered = offered || f.Kind == fix.KindOOMProtection
		}
		switch p := r.Machine.OOMPolicy; {
		case offered:
		case p.Slice != "":
			out = append(out, fmt.Sprintf("systemd-oomd is installed, and %s gives %s the policy ManagedOOMMemoryPressure=%s, but systemd-oomd isn't running. Snugbox leaves a policy it didn't write alone; to use this one, run sudo systemctl enable --now systemd-oomd.", p.File, p.Slice, p.Value))
		default:
			out = append(out, "For early out-of-memory protection, install systemd-oomd or earlyoom from the distribution's packages. Snugbox doesn't install packages.")
		}
	}
	return out
}

// FixTitle names a proposed fix in one line.
func FixTitle(p fix.Proposal) string {
	switch p.Kind {
	case fix.KindSwap:
		if !p.Zswap {
			return fmt.Sprintf("Swap file, %s (this kernel has no zswap)", Size(p.SwapSize))
		}
		return fmt.Sprintf("Swap file plus zswap, %s with %s", Size(p.SwapSize), p.Compressor)
	case fix.KindContainer, fix.KindService:
		var ns []string
		for _, l := range p.Limits {
			ns = append(ns, l.Name)
		}
		if len(ns) > 3 {
			what := "containers"
			if p.Kind == fix.KindService {
				what = "services"
			}
			return fmt.Sprintf("Limits for %d %s", len(ns), what)
		}
		return "Limits for " + List(ns)
	case fix.KindOOMProtection:
		if p.OOMDaemon == fix.DaemonEarlyoom {
			return "Early out-of-memory protection, earlyoom"
		}
		return "Early out-of-memory protection, systemd-oomd"
	case fix.KindHeap:
		var ns []string
		for _, h := range p.Heaps {
			ns = append(ns, h.Name)
		}
		if len(ns) > 3 {
			return fmt.Sprintf("Heap caps for %d workloads, after a restart", len(ns))
		}
		return "Heap caps for " + List(ns) + ", after a restart"
	case fix.KindSchedule:
		var parts []string
		switch len(p.Shifts) {
		case 0:
		case 1:
			s := p.Shifts[0]
			parts = append(parts, fmt.Sprintf("move %s %s earlier", s.Timer, Count(s.Minutes, "minute", "minutes")))
		default:
			parts = append(parts, fmt.Sprintf("move %d timers earlier", len(p.Shifts)))
		}
		switch len(p.Cron) {
		case 0:
		case 1:
			c := p.Cron[0]
			parts = append(parts, fmt.Sprintf("start %s %s later", c.Program, Count(c.Minutes, "minute", "minutes")))
		default:
			parts = append(parts, fmt.Sprintf("start %d cron jobs later", len(p.Cron)))
		}
		title := strings.Join(parts, ", and ")
		if title == "" {
			return string(p.Kind)
		}
		return strings.ToUpper(title[:1]) + title[1:]
	}
	return string(p.Kind)
}

// ShiftLine says how far a timer moves and which collision that spreads.
func ShiftLine(s fix.Shift) string {
	return fmt.Sprintf("%s: %s earlier. %s started within a minute of %s on %d of the last %d days, and their peaks add up to %s.",
		s.Timer, Count(s.Minutes, "minute", "minutes"), s.Service, s.With, s.Days, int(fix.ScheduleWindow/(24*time.Hour)), Size(s.Peak))
}

// runtimeNames are the runtimes' own names, for people.
var runtimeNames = map[string]string{"java": "Java", "node": "Node.js", "go": "Go", "python": "Python", "ruby": "Ruby", "php-fpm": "PHP-FPM"}

// heapSetting is what a heap cap sets: the variable and its value, or
// PHP-FPM's pool line.
func heapSetting(h fix.Heap) string {
	if h.Env == "" {
		return h.Setting
	}
	return h.Env + "=" + h.Setting
}

// RuntimeName is a runtime's own name, such as Node.js for node.
func RuntimeName(rt string) string {
	if n, ok := runtimeNames[rt]; ok {
		return n
	}
	return rt
}

// HeapHow says how a heap cap reaches its workload: a drop-in and a restart
// for a service, the person's own hands for a container, which must be made
// again to change its environment. setting is the variable and its value,
// already formatted for the report.
func HeapHow(h fix.Heap, setting string) string {
	switch {
	case h.Runtime == "php-fpm":
		return fmt.Sprintf("set %s in the pool's file, such as www.conf, then reload PHP-FPM. That many workers of the largest size seen, %s, fit in %d%% of its %s limit; %d run now.",
			setting, MiB(h.WorkerBytes), int(fix.FPMShare*100), MiB(h.Limit), h.Workers)
	case h.Unit == "":
		return "set " + setting + " in its environment when you next make the container."
	}
	return setting + ", set in a drop-in; applying the fix restarts the service."
}

// DatabaseNotes returns, for each database, what it holds and how far its own
// memory settings may let it grow on the plan that fits, with each
// database's documented rule of thumb. Those settings decide whether a
// smaller plan stays safe as the database's caches fill up, since the plan
// was sized from what it holds now.
func DatabaseNotes(r *audit.Result) []string {
	where, room := databaseRoom(r)
	var out []string
	for _, id := range sortedIDs(r.Workloads) {
		w, s := r.Workloads[id], r.Summary.Workloads[id]
		held, upTo := MiB(s.WorkingSetQ999), MiB(s.WorkingSetQ999+room)
		var n string
		switch w.Database {
		case "PostgreSQL":
			n = fmt.Sprintf("%s is PostgreSQL and holds %s, %s of it shared memory, which is mostly shared_buffers. %s, shared_buffers plus max_connections times work_mem should stay under %s, since one query can use work_mem several times over. PostgreSQL's documentation suggests a quarter of RAM for shared_buffers on a server of its own.",
				w.Label(), held, MiB(s.ShmemMax), where, upTo)
		case "MySQL", "MariaDB":
			n = fmt.Sprintf("%s is %s and holds %s, most of it usually the InnoDB buffer pool. %s, innodb_buffer_pool_size should stay under %s. %s's documentation puts up to 80%% of memory in the buffer pool on a server of its own.",
				w.Label(), w.Database, held, where, upTo, w.Database)
		case "Redis", "Valkey":
			n = fmt.Sprintf("%s is %s and holds %s. %s, set maxmemory under %s: without it, the default, %s grows until the box runs out of memory.",
				w.Label(), w.Database, held, where, upTo, w.Database)
		case "MongoDB":
			n = fmt.Sprintf("%s is MongoDB and holds %s. Its WiredTiger cache defaults to half of RAM less 1 GiB and may not see a container's limit, so %s, set --wiredTigerCacheSizeGB to keep the whole of it under %s.",
				w.Label(), held, strings.ToLower(where[:1])+where[1:], upTo)
		case "ClickHouse":
			n = fmt.Sprintf("%s is ClickHouse and holds %s. %s, max_server_memory_usage should stay under %s.", w.Label(), held, where, upTo)
		default:
			continue
		}
		if len(w.DBSettings) > 0 {
			n += " Its command line sets " + strings.Join(w.DBSettings, ", ") + "."
		}
		out = append(out, n)
	}
	return out
}

// databaseRoom names the plan the database notes reason about, the cheapest
// that fits or the box itself, and how much more than the snug size that
// plan holds, which a database's caches may grow into.
func databaseRoom(r *audit.Result) (where string, room uint64) {
	usable := r.Machine.MemTotal
	where = "On this box"
	if len(r.Advice.Options) > 0 {
		o := r.Advice.Options[0]
		usable, where = o.Usable, "On "+o.Plan.Label
		if c := r.Advice.Catalogue; c != nil {
			where = "On " + PlanName(c, o.Plan)
		}
	}
	if usable > r.Estimate.Snug {
		room = usable - r.Estimate.Snug
	}
	return where, room
}

// MoveText names a move, in the provider's own word for a server.
func MoveText(o plan.Option) string {
	if o.Move == plan.MoveNewServer && o.Plan.ProviderID == "aws" {
		return "new instance"
	}
	return string(o.Move)
}

// PlanName writes "DigitalOcean Basic 8 GiB".
func PlanName(c *plan.Catalogue, p plan.Plan) string {
	if prov, ok := c.Provider(p.ProviderID); ok {
		return prov.Name + " " + p.Label
	}
	return p.Label
}

// PriceNote is the footnote under the plans: when prices were checked, the
// IPv4 fees, and the conversion rate when converted is true because a listed
// plan is priced only in euros.
func PriceNote(c *plan.Catalogue, converted bool) []string {
	if c == nil {
		return nil
	}
	checked := c.Checked
	if t, err := time.Parse("2006-01-02", c.Checked); err == nil {
		checked = t.Format("2 Jan 2006")
	}
	lines := []string{fmt.Sprintf("List prices checked %s, before VAT.", checked)}
	for _, prov := range c.Providers {
		if prov.IPv4 > 0 {
			lines[0] += fmt.Sprintf(" %s adds %s a month for IPv4.", prov.Name, prov.IPv4)
		}
	}
	if converted {
		for _, fx := range c.FX {
			if fx.Currency != "EUR" {
				continue
			}
			date := fx.Date
			if t, err := time.Parse("2006-01-02", fx.Date); err == nil {
				date = t.Format("2 Jan 2006")
			}
			lines = append(lines, fmt.Sprintf("Plans priced only in euros are converted at the ECB rate of %s, %s dollars to the euro.", date, fx.USD))
		}
	}
	return lines
}

// anyConverted reports whether any option is priced from euros.
func anyConverted(opts ...[]plan.Option) bool {
	for _, os := range opts {
		for _, o := range os {
			if o.Plan.Converted {
				return true
			}
		}
	}
	return false
}

// UnavailableNote names cheaper plans that fit but can't be ordered.
func UnavailableNote(a audit.Advice) string {
	if len(a.Unavailable) == 0 || a.Catalogue == nil {
		return ""
	}
	byProvider := map[string][]string{}
	var order []string
	for _, o := range a.Unavailable {
		name := ""
		if prov, ok := a.Catalogue.Provider(o.Plan.ProviderID); ok {
			name = prov.Name
		}
		if _, ok := byProvider[name]; !ok {
			order = append(order, name)
		}
		label := strings.TrimSuffix(o.Plan.Label, ", Arm")
		item := fmt.Sprintf("%s (%s", label, o.Total)
		if o.Plan.Arm {
			item += ", Arm"
		}
		byProvider[name] = append(byProvider[name], item+")")
	}
	var parts []string
	for _, n := range order {
		parts = append(parts, n+" "+List(byProvider[n]))
	}
	if len(a.Unavailable) == 1 {
		return List(parts) + " fits too, but is listed as not available to order."
	}
	return List(parts) + " fit too, but are listed as not available to order."
}

// namesShown is how many names a sentence lists before "and N more".
const namesShown = 4

func names(ns []string) string {
	if len(ns) <= namesShown {
		return strings.Join(ns, ", ")
	}
	return strings.Join(ns[:namesShown], ", ") + fmt.Sprintf(" and %d more", len(ns)-namesShown)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
