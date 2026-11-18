package report

import (
	"encoding/json"
	"io"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/plan"
)

// SchemaV1 names the JSON format. A field is only removed in a new major
// version; new fields can appear in any release, so readers must ignore
// fields they don't know.
const SchemaV1 = "https://snugbox.dev/schema/audit/v1"

// The JSON types are separate from the internal ones so that a refactor
// inside the agent can never change the published format by accident.
// Memory is in bytes and money in cents of the named currency.

type jsonReport struct {
	Schema        string          `json:"schema"`
	Version       string          `json:"version"`
	Host          string          `json:"host"`
	Started       time.Time       `json:"started"`
	Ended         time.Time       `json:"ended"`
	WindowSeconds float64         `json:"window_seconds"`
	Samples       int             `json:"samples"`
	Root          bool            `json:"root"`
	Headline      string          `json:"headline"`
	Machine       jsonMachine     `json:"machine"`
	Provider      jsonProvider    `json:"provider"`
	Estimate      jsonEstimate    `json:"estimate"`
	Advice        jsonAdvice      `json:"advice"`
	Risks         []jsonRisk      `json:"risks"`
	Fixes         []jsonFix       `json:"fixes"`
	Workloads     []jsonWorkload  `json:"workloads"`
	Idle          []jsonIdle      `json:"idle"`
	KernelOther   jsonStats       `json:"kernel_other"`
	DataQuality   jsonDataQuality `json:"data_quality"`
}

type jsonMachine struct {
	Kernel         string `json:"kernel"`
	OS             string `json:"os"`
	Virtualization string `json:"virtualization"`
	CPUs           int    `json:"cpus"`
	MemTotalBytes  uint64 `json:"mem_total_bytes"`
	SwapBytes      uint64 `json:"swap_bytes"`
	Zswap          bool   `json:"zswap_enabled"`
	ZswapAvailable bool   `json:"zswap_available"`
	THP            string `json:"transparent_hugepages"`
	Overcommit     int    `json:"overcommit_memory"`
	OOMDaemon      string `json:"oom_daemon"`
	// OOMPolicy is systemd-oomd's policy for a slice when someone set one
	// already, or null.
	OOMPolicy     *jsonOOMPolicy `json:"oom_policy"`
	CgroupV2      bool           `json:"cgroup_v2"`
	UptimeSeconds float64        `json:"uptime_seconds"`
}

type jsonOOMPolicy struct {
	Slice string `json:"slice"`
	Value string `json:"value"`
	File  string `json:"file"`
}

type jsonProvider struct {
	ID           string `json:"id"`
	Region       string `json:"region"`
	InstanceType string `json:"instance_type,omitempty"`
	FromMetadata bool   `json:"from_metadata"`
}

type jsonEstimate struct {
	NeedBytes         uint64  `json:"need_bytes"`
	NeedFromHard      bool    `json:"need_from_hard"`
	HeadroomBytes     uint64  `json:"headroom_bytes"`
	HeadroomRule      string  `json:"headroom_rule"`
	PeakAdditionBytes uint64  `json:"peak_addition_bytes"`
	CacheBytes        uint64  `json:"cache_bytes"`
	SnugBytes         uint64  `json:"snug_bytes"`
	Confidence        string  `json:"confidence"`
	Coverage          float64 `json:"coverage"`
	CacheOverestimate bool    `json:"cache_overestimate"`
	Upward            jsonUp  `json:"upward"`
}

type jsonUp struct {
	Any               bool    `json:"any"`
	PressureShare     float64 `json:"pressure_full_share"`
	PressureSinceBoot bool    `json:"pressure_since_boot"`
	RecentKills       int     `json:"recent_kills"`
	UndatedKills      int     `json:"undated_kills"`
	SwapInIntervals   int     `json:"swap_in_intervals"`
	Intervals         int     `json:"intervals"`
}

type jsonPlan struct {
	Provider     string `json:"provider"`
	ID           string `json:"id"`
	Label        string `json:"label"`
	VCPUs        int    `json:"vcpus"`
	RAMBytes     uint64 `json:"ram_bytes"`
	UsableBytes  uint64 `json:"usable_bytes"`
	DiskBytes    uint64 `json:"disk_bytes"`
	MonthlyCents int64  `json:"monthly_cents"`
	Converted    bool   `json:"converted_from_eur"`
	Available    bool   `json:"available"`
	Arm          bool   `json:"arm"`
	Move         string `json:"move,omitempty"`
	SavingCents  *int64 `json:"saving_cents,omitempty"`
}

type jsonAdvice struct {
	Currency       string     `json:"currency"`
	CatalogueDate  string     `json:"catalogue_checked"`
	PaysForBytes   uint64     `json:"pays_for_bytes"`
	PaysForExact   bool       `json:"pays_for_exact"`
	WasteBytes     uint64     `json:"waste_bytes"`
	SizeClassBytes uint64     `json:"size_class_bytes"`
	Withheld       bool       `json:"withheld"`
	Current        *jsonPlan  `json:"current_plan"`
	Plans          []jsonPlan `json:"plans"`
	Unavailable    []jsonPlan `json:"unavailable"`
}

type jsonRisk struct {
	Kind     string   `json:"kind"`
	Text     string   `json:"text"`
	Count    int      `json:"count,omitempty"`
	Of       int      `json:"of,omitempty"`
	Undated  int      `json:"undated,omitempty"`
	Names    []string `json:"names,omitempty"`
	Share    float64  `json:"share,omitempty"`
	Setting  string   `json:"setting,omitempty"`
	Database string   `json:"database,omitempty"`
	// Restarts gives each crash-looping container's restart count, in the
	// order of names; Bytes is the memory orphaned browsers hold.
	Restarts []int  `json:"restarts,omitempty"`
	Bytes    uint64 `json:"bytes,omitempty"`
}

// jsonIdle is a container that used almost no CPU over a week or more.
type jsonIdle struct {
	Workload        string  `json:"workload"`
	Name            string  `json:"name"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	ObservedSeconds float64 `json:"observed_seconds"`
	WorkingSetBytes uint64  `json:"working_set_p99_bytes"`
}

type jsonLimit struct {
	Workload      string `json:"workload"`
	Name          string `json:"name"`
	MaxBytes      uint64 `json:"max_bytes"`
	SoftBytes     uint64 `json:"soft_bytes"`
	LowConfidence bool   `json:"low_confidence"`
}

type jsonHeap struct {
	Workload string `json:"workload"`
	Name     string `json:"name"`
	Runtime  string `json:"runtime"`
	// Env is the environment variable the cap goes in, and Setting its
	// value; PHP-FPM has no variable, and its Setting is the pool line.
	// Service names the unit whose drop-in sets the cap, and is empty where
	// the cap is advice for a person.
	Env        string `json:"env,omitempty"`
	Setting    string `json:"setting"`
	Service    string `json:"service,omitempty"`
	LimitBytes uint64 `json:"limit_bytes"`
	// Workers and WorkerBytes are PHP-FPM's worker count and its largest
	// worker.
	Workers     int    `json:"workers,omitempty"`
	WorkerBytes uint64 `json:"worker_bytes,omitempty"`
}

type jsonFix struct {
	Number       int         `json:"number"`
	Kind         string      `json:"kind"`
	Title        string      `json:"title"`
	Risk         string      `json:"risk"`
	NeedsRestart bool        `json:"needs_restart"`
	SwapBytes    uint64      `json:"swap_bytes,omitempty"`
	Compressor   string      `json:"compressor,omitempty"`
	Limits       []jsonLimit `json:"limits,omitempty"`
	Heaps        []jsonHeap  `json:"heaps,omitempty"`
	Shifts       []jsonShift `json:"shifts,omitempty"`
	Cron         []jsonCron  `json:"cron,omitempty"`
}

// jsonShift is a timer the schedule fix moves earlier.
type jsonShift struct {
	Timer     string `json:"timer"`
	Service   string `json:"service"`
	With      string `json:"collides_with"`
	Minutes   int    `json:"minutes_earlier"`
	Days      int    `json:"days"`
	PeakBytes uint64 `json:"peak_bytes"`
}

// jsonCron is a cron job the schedule fix suggests starting later, which a
// person changes in its crontab. Suggested is empty when Snugbox can't
// rewrite a schedule like it.
type jsonCron struct {
	File      string    `json:"file"`
	Line      int       `json:"line"`
	Program   string    `json:"program"`
	Schedule  string    `json:"schedule"`
	Suggested string    `json:"suggested_schedule,omitempty"`
	Minutes   int       `json:"minutes_later"`
	With      string    `json:"collides_with"`
	At        time.Time `json:"collided_at"`
	Days      int       `json:"days"`
	RiseBytes uint64    `json:"rise_bytes"`
}

type jsonStats struct {
	WorkingSetP95  uint64 `json:"working_set_p95_bytes"`
	WorkingSetP99  uint64 `json:"working_set_p99_bytes"`
	WorkingSetP999 uint64 `json:"working_set_p999_bytes"`
	WorkingSetMax  uint64 `json:"working_set_max_bytes"`
	HardMax        uint64 `json:"hard_max_bytes"`
}

type jsonWorkload struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
	Programs []string  `json:"programs"`
	Compose  *jsonComp `json:"compose,omitempty"`
	Database string    `json:"database,omitempty"`
	// DBSettings are the memory settings on the database's command line.
	DBSettings []string `json:"database_settings,omitempty"`
	Runtime    string   `json:"runtime,omitempty"`
	LimitBytes *uint64  `json:"limit_bytes"`
	PeakBytes  *uint64  `json:"memory_peak_bytes"`
	OOMKills   uint64   `json:"oom_kills_in_window"`
	Pressured  bool     `json:"pressured"`
	jsonStats
}

type jsonComp struct {
	Project string `json:"project"`
	Service string `json:"service"`
}

type jsonGap struct {
	What string `json:"what"`
	Why  string `json:"why"`
}

type jsonDataQuality struct {
	GapSeconds float64   `json:"gap_seconds"`
	Unread     []jsonGap `json:"unread"`
	Notes      []string  `json:"notes"`
}

func stats(s model.WorkloadSummary) jsonStats {
	return jsonStats{WorkingSetP95: s.WorkingSetQ95, WorkingSetP99: s.WorkingSetQ99, WorkingSetP999: s.WorkingSetQ999, WorkingSetMax: s.WorkingSetMax, HardMax: s.HardMax}
}

func jplan(c *plan.Catalogue, p plan.Plan) jsonPlan {
	return jsonPlan{
		Provider: p.ProviderID, ID: p.ID, Label: p.Label, VCPUs: p.VCPUs, RAMBytes: p.RAM, UsableBytes: plan.Usable(p.RAM),
		DiskBytes: p.Disk, MonthlyCents: int64(c.Total(p)), Converted: p.Converted, Available: p.Available, Arm: p.Arm,
	}
}

func joption(c *plan.Catalogue, o plan.Option) jsonPlan {
	j := jplan(c, o.Plan)
	j.Move = MoveText(o)
	if o.HasSaving {
		s := int64(o.Saving)
		j.SavingCents = &s
	}
	return j
}

// JSON writes the versioned machine-readable report.
func JSON(w io.Writer, r *audit.Result, version string) error {
	e, a, m := r.Estimate, r.Advice, r.Machine
	var swap uint64
	for _, s := range m.Swaps {
		swap += s.Size
	}
	var policy *jsonOOMPolicy
	if p := m.OOMPolicy; p.Slice != "" {
		policy = &jsonOOMPolicy{Slice: p.Slice, Value: p.Value, File: p.File}
	}
	out := jsonReport{
		Schema: SchemaV1, Version: version, Host: m.Hostname, Started: r.Started, Ended: r.Ended,
		WindowSeconds: r.Summary.Window().Seconds(), Samples: r.Summary.Samples, Root: r.Root, Headline: Headline(r),
		Machine: jsonMachine{
			Kernel: m.Kernel.Raw, OS: m.OS, Virtualization: m.Virt, CPUs: m.CPUs, MemTotalBytes: m.MemTotal, SwapBytes: swap,
			Zswap: m.Zswap.Enabled, ZswapAvailable: m.Zswap.Available, THP: m.THP, Overcommit: m.Overcommit,
			OOMDaemon: m.OOMDaemon, OOMPolicy: policy, CgroupV2: m.CgroupV2, UptimeSeconds: r.History.Uptime.Seconds(),
		},
		Provider: jsonProvider{ID: r.Provider.Provider, Region: r.Provider.Region, InstanceType: r.Provider.InstanceType, FromMetadata: r.Provider.FromMetadata},
		Estimate: jsonEstimate{
			NeedBytes: e.Need, NeedFromHard: e.NeedFromHard, HeadroomBytes: e.Headroom, HeadroomRule: string(e.HeadroomFrom),
			PeakAdditionBytes: e.PeakAddition, CacheBytes: e.Cache, SnugBytes: e.Snug, Confidence: e.Confidence.String(),
			Coverage: e.Coverage, CacheOverestimate: e.CacheOverestimate,
			Upward: jsonUp{
				Any: e.Upward.Any(), PressureShare: e.Upward.PressureShare, PressureSinceBoot: e.Upward.PressureSinceBoot,
				RecentKills: e.Upward.RecentKills, UndatedKills: e.Upward.UndatedKills,
				SwapInIntervals: e.Upward.SwapInIntervals, Intervals: e.Upward.Intervals,
			},
		},
		Risks:       []jsonRisk{},
		Fixes:       []jsonFix{},
		Workloads:   []jsonWorkload{},
		Idle:        []jsonIdle{},
		KernelOther: stats(r.Summary.KernelOther),
		DataQuality: jsonDataQuality{GapSeconds: r.Summary.Gaps.Seconds(), Unread: []jsonGap{}, Notes: DataQuality(r)},
	}
	out.Advice = jsonAdvice{Currency: "USD", PaysForBytes: a.PaysFor, PaysForExact: a.PaysForExact, WasteBytes: a.Waste, SizeClassBytes: a.SizeClass, Withheld: a.Withheld, Plans: []jsonPlan{}, Unavailable: []jsonPlan{}}
	if c := a.Catalogue; c != nil {
		out.Advice.CatalogueDate = c.Checked
		if a.Current != nil {
			cur := jplan(c, *a.Current)
			out.Advice.Current = &cur
		}
		for _, o := range a.Options {
			out.Advice.Plans = append(out.Advice.Plans, joption(c, o))
		}
		for _, o := range a.Unavailable {
			out.Advice.Unavailable = append(out.Advice.Unavailable, joption(c, o))
		}
	}
	for _, k := range r.Risks {
		out.Risks = append(out.Risks, jsonRisk{Kind: string(k.Kind), Text: RiskText(k, r), Count: k.Count, Of: k.Of, Undated: k.Undated, Names: k.Names, Share: k.Share, Setting: k.Setting, Database: k.Database,
			Restarts: k.Restarts, Bytes: k.Bytes})
	}
	for _, w := range r.Idle {
		out.Idle = append(out.Idle, jsonIdle{Workload: w.ID, Name: w.Name, CPUSeconds: w.CPU.Seconds(), ObservedSeconds: w.Observed.Seconds(), WorkingSetBytes: w.WorkingSet})
	}
	for i, f := range r.Fixes {
		jf := jsonFix{Number: i + 1, Kind: string(f.Kind), Title: FixTitle(f), Risk: string(f.Risk), NeedsRestart: f.NeedsRestart}
		if f.Kind == fix.KindSwap {
			jf.SwapBytes, jf.Compressor = f.SwapSize, f.Compressor
		}
		for _, l := range f.Limits {
			jf.Limits = append(jf.Limits, jsonLimit{Workload: l.WorkloadID, Name: l.Name, MaxBytes: l.Max, SoftBytes: l.Soft, LowConfidence: l.LowConfidence})
		}
		for _, h := range f.Heaps {
			jf.Heaps = append(jf.Heaps, jsonHeap{Workload: h.WorkloadID, Name: h.Name, Runtime: h.Runtime, Env: h.Env, Setting: h.Setting, Service: h.Unit,
				LimitBytes: h.Limit, Workers: h.Workers, WorkerBytes: h.WorkerBytes})
		}
		for _, s := range f.Shifts {
			jf.Shifts = append(jf.Shifts, jsonShift{Timer: s.Timer, Service: s.Service, With: s.With, Minutes: s.Minutes, Days: s.Days, PeakBytes: s.Peak})
		}
		for _, c := range f.Cron {
			jf.Cron = append(jf.Cron, jsonCron{File: c.File, Line: c.Line, Program: c.Program, Schedule: c.Schedule, Suggested: c.Suggested,
				Minutes: c.Minutes, With: c.With, At: c.At, Days: c.Days, RiseBytes: c.Rise})
		}
		out.Fixes = append(out.Fixes, jf)
	}
	for _, id := range sortedIDs(r.Workloads) {
		w, s := r.Workloads[id], r.Summary.Workloads[id]
		jw := jsonWorkload{ID: id, Kind: string(w.Kind), Name: w.Label(), Programs: w.Programs, Database: w.Database, DBSettings: w.DBSettings, Runtime: w.Runtime, OOMKills: s.OOMKills, Pressured: s.Pressured, jsonStats: stats(s)}
		if jw.Programs == nil {
			jw.Programs = []string{}
		}
		if w.Compose.Project != "" {
			jw.Compose = &jsonComp{Project: w.Compose.Project, Service: w.Compose.Service}
		}
		if w.Max.Set {
			v := w.Max.Bytes
			jw.LimitBytes = &v
		}
		if s.HasPeak {
			v := s.Peak
			jw.PeakBytes = &v
		}
		out.Workloads = append(out.Workloads, jw)
	}
	for _, g := range r.Gaps {
		out.DataQuality.Unread = append(out.DataQuality.Unread, jsonGap{What: g.What, Why: g.Why})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
