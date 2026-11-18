package audit

import (
	"slices"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

// RiskKind names a risk the audit reports.
type RiskKind string

// The risks of blueprint section 8.2, item 2, and the settings section 3.6
// says to report.
const (
	RiskKills           RiskKind = "oom_kills"
	RiskPressure        RiskKind = "pressure"
	RiskSwapping        RiskKind = "swapping"
	RiskNoSwap          RiskKind = "no_swap"
	RiskNoOOMProtection RiskKind = "no_oom_protection"
	RiskUnlimited       RiskKind = "unlimited_containers"
	RiskTHPDatabase     RiskKind = "thp_database"
	RiskOvercommit      RiskKind = "overcommit"
	// RiskCrashLoop and RiskOrphanBrowsers are the stuck workloads of the
	// catalogue's report-only "idle and stuck workloads" entry.
	RiskCrashLoop      RiskKind = "crash_loop"
	RiskOrphanBrowsers RiskKind = "orphan_browsers"
)

// Risk is one risk found on the box.
type Risk struct {
	Kind RiskKind
	// Count and Of count the affected things, such as 3 of 5 containers or
	// 2 kills. Undated counts kills whose time is unknown.
	Count   int
	Of      int
	Undated int
	Names   []string
	// Share is the share of time in full memory pressure, 0 to 1, over the
	// window or, with SinceBoot, since boot.
	Share     float64
	SinceBoot bool
	// Setting and Database describe a setting risk: the current value and the
	// database that expects another.
	Setting  string
	Database string
	// Restarts gives each crash-looping container's restart count, in the
	// order of Names, and Bytes the memory orphaned browsers hold.
	Restarts []int
	Bytes    uint64
}

// redisLike are the databases whose documentation asks for transparent huge
// pages off and memory overcommit on.
var redisLike = []string{"Redis", "Valkey"}

func findRisks(r *Result) []Risk {
	var out []Risk
	u := r.Estimate.Upward
	if u.Kills() {
		out = append(out, Risk{Kind: RiskKills, Count: u.RecentKills, Undated: u.UndatedKills, Names: killedWorkloads(r)})
	}
	if u.Pressure() {
		out = append(out, Risk{Kind: RiskPressure, Share: u.PressureShare, SinceBoot: u.PressureSinceBoot})
	}
	if u.Swapping() {
		out = append(out, Risk{Kind: RiskSwapping, Count: u.SwapInIntervals, Of: u.Intervals})
	}
	if len(r.Machine.Swaps) == 0 {
		out = append(out, Risk{Kind: RiskNoSwap})
	}
	if r.Machine.OOMDaemon == "" {
		out = append(out, Risk{Kind: RiskNoOOMProtection})
	}
	if names, of := unlimitedContainers(r.Workloads); len(names) > 0 {
		out = append(out, Risk{Kind: RiskUnlimited, Count: len(names), Of: of, Names: names})
	}
	dbs := databases(r.Workloads)
	for _, db := range redisLike {
		if !slices.Contains(dbs, db) {
			continue
		}
		if r.Machine.THP == "always" {
			out = append(out, Risk{Kind: RiskTHPDatabase, Setting: r.Machine.THP, Database: db})
		}
		if r.Machine.Overcommit != 1 && r.Machine.Overcommit >= 0 {
			out = append(out, Risk{Kind: RiskOvercommit, Setting: overcommitName(r.Machine.Overcommit), Database: db})
		}
		break
	}
	if r.Machine.Overcommit == 2 && !slices.ContainsFunc(out, func(x Risk) bool { return x.Kind == RiskOvercommit }) {
		out = append(out, Risk{Kind: RiskOvercommit, Setting: overcommitName(2)})
	}
	if len(r.CrashLoops) > 0 {
		k := Risk{Kind: RiskCrashLoop, Count: len(r.CrashLoops)}
		for _, cl := range r.CrashLoops {
			k.Names, k.Restarts = append(k.Names, cl.Name), append(k.Restarts, cl.Restarts)
		}
		out = append(out, k)
	}
	if k := orphanBrowsers(r.Workloads); k.Count > 0 {
		out = append(out, k)
	}
	return out
}

// orphanBrowsers gathers the headless browsers left without their parent,
// across workloads.
func orphanBrowsers(ws map[string]model.Workload) Risk {
	k := Risk{Kind: RiskOrphanBrowsers}
	for _, id := range sortedKeys(ws) {
		w := ws[id]
		if w.OrphanBrowsers == 0 {
			continue
		}
		k.Count += w.OrphanBrowsers
		k.Of += w.OrphanProcesses
		k.Bytes += w.OrphanBytes
		k.Names = append(k.Names, w.Label())
		switch k.Setting {
		case "":
			k.Setting = w.OrphanProgram
		case w.OrphanProgram:
		default:
			k.Setting = "several"
		}
	}
	return k
}

// Idle is a container that used almost no CPU over at least IdleAfter.
type Idle struct {
	ID, Name string
	CPU      time.Duration
	Observed time.Duration
	// WorkingSet is its 99th percentile working set, the memory a stop
	// would give back.
	WorkingSet uint64
}

// IdleAfter and IdleCPU define an idle container: watched for at least
// IdleAfter, and using under IdleCPU of one core on average. Starting
// values; the report shows the CPU time, so a person can judge.
const (
	IdleAfter = 7 * 24 * time.Hour
	IdleCPU   = 0.001
)

func findIdle(r *Result) []Idle {
	var out []Idle
	for _, id := range sortedKeys(r.Workloads) {
		w, s := r.Workloads[id], r.Summary.Workloads[id]
		if w.Kind != model.KindContainer || !s.HasCPU || s.Observed < IdleAfter {
			continue
		}
		if s.CPU.Seconds() < IdleCPU*s.Observed.Seconds() {
			out = append(out, Idle{ID: id, Name: w.Label(), CPU: s.CPU, Observed: s.Observed, WorkingSet: s.WorkingSetQ99})
		}
	}
	slices.SortStableFunc(out, func(a, b Idle) int {
		switch {
		case a.WorkingSet > b.WorkingSet:
			return -1
		case a.WorkingSet < b.WorkingSet:
			return 1
		}
		return 0
	})
	return out
}

func overcommitName(v int) string {
	switch v {
	case 0:
		return "0 (heuristic)"
	case 1:
		return "1 (always)"
	case 2:
		return "2 (strict)"
	}
	return "unknown"
}

// killedWorkloads names the workloads with kills inside the window, then the
// cgroups named by kills in the kernel log, without repeats.
func killedWorkloads(r *Result) []string {
	var names []string
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	for _, id := range sortedKeys(r.Summary.Workloads) {
		if r.Summary.Workloads[id].OOMKills > 0 {
			add(r.Workloads[id].Label())
		}
	}
	for _, k := range r.KernelLog.Kills {
		if !k.At.Before(r.History.Now.Add(-model.KillLookback)) {
			if w, ok := r.Workloads[k.Memcg]; ok {
				add(w.Label())
			} else {
				add(k.Task)
			}
		}
	}
	return names
}

func unlimitedContainers(ws map[string]model.Workload) (names []string, containers int) {
	for _, id := range sortedKeys(ws) {
		w := ws[id]
		if w.Kind != model.KindContainer {
			continue
		}
		containers++
		if !w.Max.Set {
			names = append(names, w.Label())
		}
	}
	return names, containers
}

func databases(ws map[string]model.Workload) []string {
	var out []string
	for _, w := range ws {
		if w.Database != "" && !slices.Contains(out, w.Database) {
			out = append(out, w.Database)
		}
	}
	slices.Sort(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
