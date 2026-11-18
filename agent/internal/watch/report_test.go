package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/plan"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/store"
)

// Most resizes happen while the box is off, so the watcher compares the box
// with what it saw before it stopped, and records the plans on either side.
func TestResizeWhileStopped(t *testing.T) {
	ctx := context.Background()
	b, _ := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	d.prov = provider.Info{Provider: "digitalocean", FromMetadata: true}
	d.checkResize(ctx, clk.now)
	if got, _ := d.st.Resizes(ctx, time.Time{}); len(got) != 0 {
		t.Fatalf("the first start recorded a resize: %+v", got)
	}

	// The box comes back from its provider with half the memory and CPUs.
	old := d.facts()
	d.machine.MemTotal, d.machine.CPUs = 4<<30, 2
	clk.now = clk.now.Add(time.Hour)
	d.checkResize(ctx, clk.now)
	got, err := d.st.Resizes(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := plan.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	before, okBefore := c.Current(plan.Box{ProviderID: "digitalocean", MemTotal: old.MemTotal, CPUs: old.CPUs})
	after, okAfter := c.Current(plan.Box{ProviderID: "digitalocean", MemTotal: 4 << 30, CPUs: 2})
	if !okBefore || !okAfter {
		t.Fatalf("the catalogue has no DigitalOcean plan for the test box: %v %v", okBefore, okAfter)
	}
	want := store.Resize{Time: clk.now, OldRAM: old.MemTotal, NewRAM: 4 << 30, OldCPUs: old.CPUs, NewCPUs: 2, Provider: "digitalocean",
		OldPlan: before.ID, NewPlan: after.ID, OldCents: int64(c.Total(before)), NewCents: int64(c.Total(after))}
	if len(got) != 1 || got[0] != want {
		t.Errorf("resizes\n got %+v\nwant %+v", got, want)
	}
	// Started again on the same box, it records nothing more.
	d.checkResize(ctx, clk.now.Add(time.Hour))
	if got, _ := d.st.Resizes(ctx, time.Time{}); len(got) != 1 {
		t.Errorf("a restart without a resize recorded one: %+v", got)
	}
}

func TestReportRoute(t *testing.T) {
	ctx := context.Background()
	b, _ := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	run(t, d, clk, 10*time.Minute, nil)
	if err := d.analyseNow(ctx); err != nil {
		t.Fatal(err)
	}
	from := clk.now
	clk.now = clk.now.Add(time.Hour)
	for _, e := range []store.Event{
		{Time: from.Add(time.Minute), Kind: "oom_kill", Workload: "app.service"},
		{Time: from.Add(2 * time.Minute), Kind: "limit_high", Workload: "app.service"},
		{Time: from.Add(3 * time.Minute), Kind: "limit_high", Workload: "app.service"},
		{Time: from.Add(4 * time.Minute), Kind: "warning", Workload: "app.service", Detail: "may run out"},
		{Time: from.Add(-time.Minute), Kind: "oom_kill", Workload: "db.service"},
	} {
		if err := d.st.AddEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.st.AddResize(ctx, store.Resize{Time: from.Add(5 * time.Minute), OldRAM: 8 << 30, NewRAM: 4 << 30, OldCPUs: 4, NewCPUs: 2}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/report?since=" + url.QueryEscape(from.Format(time.RFC3339)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep localapi.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if !rep.From.Equal(from) || !rep.To.Equal(clk.now) || rep.Then == nil || rep.Now == nil || rep.Then.Snug == 0 || rep.WatchStart.IsZero() {
		t.Errorf("report period and estimates: %+v", rep)
	}
	if len(rep.Events) != 2 || rep.Events[0].Kind != "warning" || rep.Events[1].Kind != "oom_kill" || rep.Throttled["app.service"] != 2 {
		t.Errorf("events %+v, throttled %v", rep.Events, rep.Throttled)
	}
	if len(rep.Resizes) != 1 || rep.Resizes[0].NewRAM != 4<<30 {
		t.Errorf("resizes %+v", rep.Resizes)
	}
	if rep.Changes.Helper || rep.Changes.Error == "" {
		t.Errorf("changes without a helper: %+v", rep.Changes)
	}

	for _, bad := range []string{"", "yesterday", clk.now.Add(time.Hour).Format(time.RFC3339)} {
		resp, err := http.Get(srv.URL + "/v1/report?since=" + url.QueryEscape(bad))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("since=%q: status %d", bad, resp.StatusCode)
		}
	}
}
