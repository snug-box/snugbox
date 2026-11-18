package watch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/testbox"
)

const mib = model.MiB

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newTestDaemon(t testing.TB, b *testbox.Box, clk *fakeClock) *Daemon {
	t.Helper()
	c, err := collect.New(collect.Options{FS: b, PageSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	d, err := New(ctx, Config{DB: filepath.Join(dir, "snugbox.db"), Pause: filepath.Join(dir, "paused"), Offline: true},
		Deps{Collector: c, FS: b, Now: clk.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	d.machine = c.Machine(ctx, nil)
	d.rec = d.st.NewRecorder(d.machine.CPUs, c.PageSize())
	return d
}

func shop() (*testbox.Box, *testbox.Workload) {
	b := testbox.New()
	app := b.Add(&testbox.Workload{Path: "system.slice/app.service", Programs: []string{"node"}, Anon: 800 * mib, ActiveFile: 100 * mib, InactiveFile: 100 * mib})
	b.Add(&testbox.Workload{Path: "system.slice/db.service", Programs: []string{"postgres"}, Anon: 300 * mib, Shmem: 64 * mib, ActiveFile: 200 * mib})
	return b, app
}

// run samples the box every 10 seconds for d, letting step change it first.
func run(t *testing.T, d *Daemon, clk *fakeClock, dur time.Duration, step func(i int)) {
	t.Helper()
	ctx := context.Background()
	for i := 0; time.Duration(i)*SampleEvery < dur; i++ {
		if step != nil {
			step(i)
		}
		if err := d.sampleOnce(ctx, clk.now); err != nil {
			t.Fatal(err)
		}
		clk.now = clk.now.Add(SampleEvery)
	}
}

func TestAnalyse(t *testing.T) {
	b, app := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	d.prov = provider.Info{Provider: provider.DigitalOcean, Region: "fra1", FromMetadata: true}
	run(t, d, clk, 2*time.Hour, func(i int) {
		b.Update(func(*testbox.Box) {
			app.Anon = 800*mib + uint64(i%30)*4*mib
			b.CPUBusy += 100
			b.CPUIdle += 300
		})
	})
	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := d.Result()
	if r == nil || !r.Watched {
		t.Fatal("no watched result")
	}
	if r.Summary.Samples != 720 || r.Estimate.Window < 119*time.Minute {
		t.Errorf("samples %d over %v", r.Summary.Samples, r.Estimate.Window)
	}
	if r.Estimate.Confidence != model.ConfidenceLow {
		t.Errorf("two hours of watching gave confidence %v", r.Estimate.Confidence)
	}
	if r.Advice.Current == nil || r.Advice.Current.ID != "s-4vcpu-8gb" {
		t.Errorf("plan %+v", r.Advice.Current)
	}
	wl := r.Summary.Workloads["/system.slice/app.service"]
	if wl.WorkingSetMax != 800*mib+29*4*mib+100*mib {
		t.Errorf("app working set max %d MiB", wl.WorkingSetMax/mib)
	}
	snaps, err := d.st.Snapshots(context.Background(), time.Time{})
	if err != nil || len(snaps) != 1 {
		t.Errorf("snapshots %+v, %v", snaps, err)
	}
	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snaps, _ := d.st.Snapshots(context.Background(), time.Time{}); len(snaps) != 1 {
		t.Errorf("an unchanged estimate added a snapshot: %d", len(snaps))
	}
}

func TestKillEvents(t *testing.T) {
	b, app := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	run(t, d, clk, time.Minute, func(i int) {
		if i == 3 {
			b.Update(func(*testbox.Box) { app.OOMKills, b.OOMKills = 2, 2 })
		}
	})
	evs, err := d.st.Events(context.Background(), time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	kills := 0
	for _, e := range evs {
		if e.Kind == "oom_kill" && e.Workload == "app.service" {
			kills++
		}
	}
	if kills != 2 {
		t.Errorf("%d kill events, want 2: %+v", kills, evs)
	}
	select {
	case <-d.analyse:
	default:
		t.Error("a kill did not ask for an analysis")
	}
	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := d.Result(); r.History.Kills == nil || len(r.History.Kills) != 2 || !r.Advice.Withheld {
		t.Errorf("kills in history %v, withheld %v", r.History.Kills, r.Advice.Withheld)
	}
}

func TestEarlyWarning(t *testing.T) {
	b, app := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	type post struct {
		contentType string
		body        []byte
	}
	posts := make(chan post, 4)
	hook := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		posts <- post{r.Header.Get("Content-Type"), body}
	}))
	defer hook.Close()
	d.cfg.Webhook = hook.URL
	d.warn.client = hook.Client()
	// The app grows 40 MiB a minute for two hours, which takes the box past
	// its memory within half an hour.
	run(t, d, clk, 2*time.Hour, func(i int) {
		b.Update(func(*testbox.Box) { app.Anon = 2048*mib + uint64(i)*40*mib/6 })
	})
	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.warn.check(context.Background(), clk.now)
	evs, err := d.st.Events(context.Background(), time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var warning string
	for _, e := range evs {
		if e.Kind == "warning" {
			warning = e.Detail
		}
	}
	if !strings.Contains(warning, "may run out of memory within") || !strings.Contains(warning, "app.service is growing") {
		t.Fatalf("warning %q", warning)
	}
	select {
	case p := <-posts:
		var got struct {
			Box           string    `json:"box"`
			Time          time.Time `json:"time"`
			Workload      *string   `json:"workload"`
			ForecastBytes uint64    `json:"forecast_bytes"`
			LimitBytes    uint64    `json:"limit_bytes"`
			Message       string    `json:"message"`
		}
		if err := json.Unmarshal(p.body, &got); err != nil {
			t.Fatalf("the webhook got %q: %v", p.body, err)
		}
		// A warning about the whole box names no workload.
		if p.contentType != "application/json" || got.Box != "web-1" || !got.Time.Equal(clk.now) || got.Workload != nil ||
			got.LimitBytes != b.MemTotal || got.ForecastBytes < got.LimitBytes || got.Message != warning {
			t.Errorf("the webhook got %s as %q", p.body, p.contentType)
		}
	default:
		t.Error("the warning was not sent to the webhook")
	}
	d.warn.check(context.Background(), clk.now.Add(time.Minute))
	evs, _ = d.st.Events(context.Background(), time.Time{}, 100)
	n := 0
	for _, e := range evs {
		if e.Kind == "warning" {
			n++
		}
	}
	if n != 1 || len(posts) != 0 {
		t.Errorf("%d warnings and %d more posts within the cooldown, want 1 and none", n, len(posts))
	}
	d.warn.check(context.Background(), clk.now.Add(OutcomeAfter+time.Minute))
	evs, _ = d.st.Events(context.Background(), time.Time{}, 100)
	if evs[0].Kind != "warning_outcome" && evs[1].Kind != "warning_outcome" {
		t.Errorf("no outcome recorded after an hour: %+v", evs[:2])
	}
}

// A service that has just started climbs to its usual size within its
// first minute, as snugboxd does on every start. A trend from that climb
// forecast its 64 MiB limit within half an hour, and the watcher warned
// about itself after each start. A series is judged only once it covers
// the ten minutes "still rising" is measured over; then a service that
// keeps growing toward its limit is warned about.
func TestNoWarningFromAServicesFirstMinutes(t *testing.T) {
	b, _ := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	ctx := context.Background()
	run(t, d, clk, time.Hour, nil)
	w := b.Add(&testbox.Workload{Path: "system.slice/worker.service", Programs: []string{"worker"}, Anon: 2 * mib, Max: 64 * mib})
	if err := d.deps.Collector.Discover(ctx); err != nil {
		t.Fatal(err)
	}
	warnings := func() []string {
		evs, err := d.st.Events(ctx, time.Time{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range evs {
			if e.Kind == "warning" {
				out = append(out, e.Detail)
			}
		}
		return out
	}
	run(t, d, clk, time.Minute, func(i int) {
		if i == 1 {
			b.Update(func(*testbox.Box) { w.Anon = 16 * mib })
		}
	})
	if err := d.analyseNow(ctx); err != nil {
		t.Fatal(err)
	}
	if wl, ok := d.Result().Workloads["/system.slice/worker.service"]; !ok || !wl.Max.Set {
		t.Fatalf("the analysis has no limit for the new service: %+v", wl)
	}
	d.warn.check(ctx, clk.now)
	if got := warnings(); len(got) != 0 {
		t.Errorf("warnings a minute after a start: %q", got)
	}
	// Then it grows a quarter of a MiB every 10 seconds for 20 minutes.
	run(t, d, clk, 20*time.Minute, func(i int) {
		b.Update(func(*testbox.Box) { w.Anon = 16*mib + uint64(i)*mib/4 })
	})
	d.warn.check(ctx, clk.now)
	got := warnings()
	if len(got) != 1 || !strings.Contains(got[0], "worker.service may reach its 64 MiB limit within") {
		t.Errorf("warnings for a service growing toward its limit: %q", got)
	}
}

func TestWithinMinutes(t *testing.T) {
	for n, want := range map[int]string{1: "within 1 minute", 2: "within 2 minutes", 30: "within 30 minutes"} {
		if got := withinMinutes(n); got != want {
			t.Errorf("withinMinutes(%d) = %q, want %q", n, got, want)
		}
	}
}

// The early warnings keep their two hours of points in memory after the
// first check reads them from the store. Through three hours of samples, a
// workload that leaves and one that comes, with a check every minute as the
// watcher makes it, they hold exactly what the store would give for the
// same window.
func TestWarnerKeepsTheStoresWindow(t *testing.T) {
	b, app := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	ctx := context.Background()
	var cron *testbox.Workload
	for i := 0; time.Duration(i)*SampleEvery < 3*time.Hour; i++ {
		b.Update(func(*testbox.Box) { app.Anon = 800*mib + uint64(i%40)*mib })
		switch i {
		case 400:
			cron = b.Add(&testbox.Workload{Path: "system.slice/cron-job.service", Programs: []string{"python3"}, Anon: 90 * mib})
			if err := d.deps.Collector.Discover(ctx); err != nil {
				t.Fatal(err)
			}
		case 600:
			b.Update(func(b *testbox.Box) {
				b.Workloads = slices.DeleteFunc(b.Workloads, func(w *testbox.Workload) bool { return w == cron })
			})
			if err := d.deps.Collector.Discover(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.sampleOnce(ctx, clk.now); err != nil {
			t.Fatal(err)
		}
		if i%6 == 5 {
			d.warn.check(ctx, clk.now)
		}
		clk.now = clk.now.Add(SampleEvery)
	}
	since := clk.now.Add(-SampleEvery - TrendWindow)
	d.warn.trim(since)
	fresh := newWarner(d)
	if err := fresh.load(ctx, since); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.warn.box, fresh.box) || d.warn.memTotal != fresh.memTotal {
		t.Errorf("box: %d points in memory, %d in the store", len(d.warn.box), len(fresh.box))
	}
	if !reflect.DeepEqual(d.warn.work, fresh.work) {
		t.Errorf("workloads: %d series in memory, %d in the store", len(d.warn.work), len(fresh.work))
	}
	if len(fresh.work) < 2 || len(fresh.box) < 700 {
		t.Errorf("the window has %d workloads and %d box points; the test needs more", len(fresh.work), len(fresh.box))
	}
}

// Warnings carry the box's name and what is growing, so they never go out
// over plain HTTP.
func TestWebhookMustBeHTTPS(t *testing.T) {
	b, _ := shop()
	d := newTestDaemon(t, b, &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)})
	hits := make(chan struct{}, 1)
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits <- struct{}{} }))
	defer plain.Close()
	for _, hook := range []string{plain.URL, "https://", "https//example.com/hook"} {
		d.cfg.Webhook = hook
		d.warn.post(context.Background(), Warning{Box: "web-1", Message: "test"})
	}
	select {
	case <-hits:
		t.Error("a warning went to an http:// webhook")
	default:
	}
	for raw, ok := range map[string]bool{"https://example.com/hooks/snugbox": true, plain.URL: false, "https://": false, "": false} {
		if _, err := webhookURL(raw); (err == nil) != ok {
			t.Errorf("webhookURL(%q): %v", raw, err)
		}
	}
}

func TestForecast(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var pts []sample
	for i := range 120 {
		pts = append(pts, sample{t0.Add(time.Duration(i) * time.Minute).Unix(), float64(1000 + 10*i)})
	}
	f := forecast(pts, 0)
	if math.Abs(f.PerMinute-10) > 1e-6 || !f.Rising || math.Abs(f.Level-(1000+10*119+300)) > 1e-6 {
		t.Errorf("forecast %+v", f)
	}
	flat := forecast(pts[:1], 5000)
	if flat.Level != 5000 || flat.Rising {
		t.Errorf("one point with a daily peak: %+v", flat)
	}
	// Four minutes of steep growth aren't rising yet; ten minutes are.
	if f := forecast(pts[:5], 0); f.Rising {
		t.Errorf("a series of four minutes is rising: %+v", f)
	}
	if f := forecast(pts[:11], 0); !f.Rising {
		t.Errorf("a series of ten minutes isn't rising: %+v", f)
	}
	var falling []sample
	for i := range 60 {
		falling = append(falling, sample{t0.Add(time.Duration(i) * time.Minute).Unix(), float64(5000 - 10*i)})
	}
	if f := forecast(falling, 0); f.Rising || f.Level != 5000-590 {
		t.Errorf("falling series %+v", f)
	}
}

func TestAPI(t *testing.T) {
	b, _ := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	get := func(path string, out any) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode
	}
	if code := get("/v1/result", nil); code != http.StatusServiceUnavailable {
		t.Errorf("result before analysis: %d", code)
	}
	run(t, d, clk, 10*time.Minute, nil)
	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st localapi.Status
	if code := get("/v1/status", &st); code != http.StatusOK || !st.Analysed || st.Workloads != 2 || st.StoreBytes == 0 {
		t.Errorf("status %d %+v", code, st)
	}
	var live localapi.Live
	if code := get("/v1/live", &live); code != http.StatusOK || len(live.Workloads) != 2 || len(live.Box) == 0 || live.Workloads[0].Name != "app.service" {
		t.Errorf("live %d %+v", code, live)
	}
	if code := get("/v1/events?since=yesterday", nil); code != http.StatusBadRequest {
		t.Errorf("bad since: %d", code)
	}
	if code := get("/v1/events?limit=0", nil); code != http.StatusBadRequest {
		t.Errorf("bad limit: %d", code)
	}
	resp, err := http.Post(srv.URL+"/v1/status", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the read-only API: %d", resp.StatusCode)
	}
}

func TestClientOverSocket(t *testing.T) {
	b, _ := shop()
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	d := newTestDaemon(t, b, clk)
	run(t, d, clk, 10*time.Minute, nil)
	if err := d.analyseNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, ln) }()
	c := localapi.NewClient(sock)
	r, err := c.Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Advice.Catalogue == nil || r.Estimate.Snug == 0 || !r.Watched {
		t.Errorf("result over the socket: catalogue %v, snug %d", r.Advice.Catalogue != nil, r.Estimate.Snug)
	}
	cancel()
	if err := <-done; err != nil {
		t.Error(err)
	}
	if _, err := localapi.NewClient(filepath.Join(t.TempDir(), "missing.sock")).Status(context.Background()); err == nil {
		t.Error("a missing socket did not fail")
	}
}
