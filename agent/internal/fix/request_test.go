package fix

import (
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/model"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

const fullID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Every request built from a proposal passes the helper's own bounds, so a
// proposal the helper would refuse is a bug found here, not on a server.
func TestRequestsPassTheHelpersBounds(t *testing.T) {
	in := baseInput()
	a := in.Workloads["/system.slice/docker-a.scope"]
	a.ContainerID = fullID
	in.Workloads["/system.slice/docker-a.scope"] = a
	in.Workloads["/system.slice/api.service"] = model.Workload{ID: "/system.slice/api.service", Kind: model.KindService, Name: "api.service", Runtime: "java"}
	in.Workloads["/system.slice/worker.service"] = model.Workload{ID: "/system.slice/worker.service", Kind: model.KindService, Name: "worker.service", Runtime: "ruby", Threads: 8}
	in.Runs = append(daily("backup.service", time.Hour, 12*time.Minute, 500*mib, 1, 2), daily("dump.service", time.Hour, time.Minute, 400*mib, 1, 2)...)
	in.Now, in.Timers = scheduleNow, timers("backup.service", "dump.service")
	ps := Propose(in)
	if len(ps) != 6 {
		t.Fatalf("%d proposals", len(ps))
	}
	for _, p := range ps {
		f, advice, err := Request(p, in.Workloads, RequestOptions{Persist: true})
		if err != nil {
			t.Fatalf("%s: %v", p.Kind, err)
		}
		if f == nil {
			t.Fatalf("%s: no request, advice %q", p.Kind, advice)
		}
		req := &helperv1.Request{Kind: &helperv1.Request_Apply{Apply: &helperv1.ApplyRequest{Fix: f}}}
		if err := helperapi.Validate(req, helperapi.Box{MemTotal: in.Machine.MemTotal}); err != nil {
			t.Errorf("%s: the helper would refuse it: %v", p.Kind, err)
		}
		if p.Kind == KindHeap && (len(advice) != 1 || !strings.Contains(advice[0], "NODE_OPTIONS=--max-old-space-size=1248")) {
			t.Errorf("heap advice %q; the node container gets advice, not a change", advice)
		}
	}
}

func TestRequestNarrowing(t *testing.T) {
	in := baseInput()
	s, _ := find(Propose(in), KindService)
	f, _, err := Request(s, in.Workloads, RequestOptions{Only: []string{"backup.service"}})
	if err != nil {
		t.Fatal(err)
	}
	if ls := f.GetServiceLimits().GetLimits(); len(ls) != 1 || ls[0].GetUnit() != "backup.service" || ls[0].GetHighBytes() != 512*mib || ls[0].GetMaxBytes() != 640*mib {
		t.Errorf("narrowed to backup.service: %v", ls)
	}
	if _, _, err := Request(s, in.Workloads, RequestOptions{Only: []string{"nginx.service"}}); err == nil {
		t.Error("a name outside the fix was accepted")
	}
	c, _ := find(Propose(in), KindContainer)
	if _, _, err := Request(c, in.Workloads, RequestOptions{}); err == nil || !strings.Contains(err.Error(), "no full ID") {
		t.Errorf("container without an ID: %v", err)
	}

	sched := Proposal{Kind: KindSchedule, Shifts: []Shift{
		{Timer: "dump.timer", Service: "dump.service", Minutes: 20}, {Timer: "index.timer", Service: "index.service", Minutes: 45},
	}}
	for _, only := range []string{"dump.timer", "dump.service"} {
		f, _, err := Request(sched, in.Workloads, RequestOptions{Only: []string{only}})
		if ss := f.GetSchedule().GetShifts(); err != nil || len(ss) != 1 || ss[0].GetTimer() != "dump.timer" || ss[0].GetService() != "dump.service" || ss[0].GetMinutes() != 20 {
			t.Errorf("narrowed to %s: %v, %v", only, ss, err)
		}
	}
	if _, _, err := Request(sched, in.Workloads, RequestOptions{Only: []string{"backup.timer"}}); err == nil {
		t.Error("a timer outside the fix was accepted")
	}
}

func TestRequestSwap(t *testing.T) {
	in := baseInput()
	in.Machine.Zswap = collect.Zswap{}
	in.Summary.CPUQ95 = 3.5
	p, _ := find(Propose(in), KindSwap)
	f, _, err := Request(p, in.Workloads, RequestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := f.GetSwap(); s.GetCompressor() != helperv1.Compressor_COMPRESSOR_UNSPECIFIED || s.GetShrinker() || s.GetSizeBytes() != 4*gib {
		t.Errorf("swap without zswap: %v", s)
	}
}
