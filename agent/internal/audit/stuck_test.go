package audit

import (
	"slices"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/kernel"
	"snugbox.dev/snugbox/agent/internal/model"
)

func stuckResult() *Result {
	week := 8 * 24 * time.Hour
	return &Result{
		Machine: collect.Machine{Swaps: []kernel.Swap{{}}, OOMDaemon: "systemd-oomd", Overcommit: 0},
		Workloads: map[string]model.Workload{
			"/c/old":     {ID: "/c/old", Kind: model.KindContainer, Name: "shop-old-1"},
			"/c/busy":    {ID: "/c/busy", Kind: model.KindContainer, Name: "shop-app-1", Max: model.Limit{Bytes: 1 << 30, Set: true}},
			"/c/new":     {ID: "/c/new", Kind: model.KindContainer, Name: "shop-new-1", Max: model.Limit{Bytes: 1 << 30, Set: true}},
			"/c/nocpu":   {ID: "/c/nocpu", Kind: model.KindContainer, Name: "shop-quiet-1", Max: model.Limit{Bytes: 1 << 30, Set: true}},
			"/s/cron":    {ID: "/s/cron", Kind: model.KindService, Name: "cron.service"},
			"/c/scraper": {ID: "/c/scraper", Kind: model.KindContainer, Name: "shop-scraper-1", Max: model.Limit{Bytes: 1 << 30, Set: true}, OrphanBrowsers: 2, OrphanProcesses: 9, OrphanBytes: 700 << 20, OrphanProgram: "chrome"},
		},
		Summary: model.Summary{Workloads: map[string]model.WorkloadSummary{
			"/c/old":     {HasCPU: true, CPU: 30 * time.Second, Observed: week, WorkingSetQ99: 240 << 20},
			"/c/busy":    {HasCPU: true, CPU: 2 * time.Hour, Observed: week},
			"/c/new":     {HasCPU: true, CPU: time.Second, Observed: 6 * 24 * time.Hour},
			"/c/nocpu":   {Observed: week},
			"/s/cron":    {HasCPU: true, CPU: time.Second, Observed: week},
			"/c/scraper": {HasCPU: true, CPU: 3 * time.Hour, Observed: week},
		}},
		CrashLoops: []collect.CrashLoop{{Name: "shop-worker-1", Restarts: 14}, {Name: "shop-cron-1", Restarts: 5, Restarting: true}},
	}
}

// Crash loops and orphaned browsers are risks; an idle container is listed
// apart, as it harms nothing but the bill.
func TestStuckWorkloads(t *testing.T) {
	r := stuckResult()
	risks := findRisks(r)
	i := slices.IndexFunc(risks, func(k Risk) bool { return k.Kind == RiskCrashLoop })
	if i < 0 || risks[i].Count != 2 || !slices.Equal(risks[i].Names, []string{"shop-worker-1", "shop-cron-1"}) || !slices.Equal(risks[i].Restarts, []int{14, 5}) {
		t.Errorf("crash loop risk %+v", risks)
	}
	j := slices.IndexFunc(risks, func(k Risk) bool { return k.Kind == RiskOrphanBrowsers })
	if j < 0 || risks[j].Count != 2 || risks[j].Of != 9 || risks[j].Bytes != 700<<20 || risks[j].Setting != "chrome" || !slices.Equal(risks[j].Names, []string{"shop-scraper-1"}) {
		t.Errorf("orphan browser risk %+v", risks)
	}

	idle := findIdle(r)
	// A week and a day at 30 CPU seconds is well under 0.1% of a core. The
	// busy container is over it, the new one hasn't been watched a week, the
	// quiet one gave no CPU figure, and a service is not a container.
	if len(idle) != 1 || idle[0].Name != "shop-old-1" || idle[0].CPU != 30*time.Second || idle[0].WorkingSet != 240<<20 {
		t.Errorf("idle %+v, want shop-old-1 alone", idle)
	}
}
