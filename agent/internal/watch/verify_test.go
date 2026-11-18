package watch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/helperapi"
	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/store"
	"snugbox.dev/snugbox/agent/internal/testbox"
	helperv1 "snugbox.dev/snugbox/gen/snugbox/helper/v1"
)

// fakeHelper keeps transactions and moves them the way snugbox-helper does
// when the watcher asks.
type fakeHelper struct {
	mu      sync.Mutex
	clk     *fakeClock
	txs     []*helperv1.Transaction
	reads   int
	cts     []*helperv1.Container
	calls   []string
	undoErr error
}

func (f *fakeHelper) List(context.Context) ([]*helperv1.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	out := slices.Clone(f.txs)
	slices.Reverse(out)
	return out, nil
}

func TestIdleVerifierReadsOnlyOnChange(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	h := &fakeHelper{clk: clk}
	d := newVerifiedDaemon(t, testbox.New(), clk, h)
	file := &fstest.MapFile{}
	d.deps.FS = fstest.MapFS{strings.TrimPrefix(helperapi.JournalPath, "/"): file}
	check := func(want int) {
		t.Helper()
		d.verify.check(context.Background(), clk.now, d.last)
		if h.reads != want {
			t.Fatalf("helper reads %d, want %d", h.reads, want)
		}
	}
	check(1)
	clk.now = clk.now.Add(ChangesEvery)
	check(1)
	file.Data = []byte("changed")
	clk.now = clk.now.Add(ChangesEvery)
	check(2)
	d.verify.poke()
	check(3)
	clk.now = clk.now.Add(helperResync)
	check(4)
}

func (f *fakeHelper) Containers(context.Context) (*helperv1.ContainersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &helperv1.ContainersResponse{Docker: true, Containers: f.cts}, nil
}

func (f *fakeHelper) find(id string) *helperv1.Transaction {
	for _, tx := range f.txs {
		if tx.GetId() == id {
			return tx
		}
	}
	return nil
}

func (f *fakeHelper) Advance(_ context.Context, id string, to helperv1.State, reason string) (*helperv1.Transaction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("advance %s to %s: %s", id, helperapi.StateName(to), reason))
	tx := f.find(id)
	tx.State, tx.StateSince, tx.Reason = to, timestamppb.New(f.clk.now), reason
	return tx, nil
}

func (f *fakeHelper) Undo(_ context.Context, id, reason string, _ func(*helperv1.Progress)) (*helperv1.UndoResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("undo %s: %s", id, reason))
	if f.undoErr != nil {
		return nil, f.undoErr
	}
	tx := f.find(id)
	tx.State, tx.StateSince, tx.Reason = helperv1.State_STATE_UNDONE, timestamppb.New(f.clk.now), reason
	return &helperv1.UndoResponse{Transaction: id}, nil
}

func (f *fakeHelper) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func newVerifiedDaemon(t *testing.T, b *testbox.Box, clk *fakeClock, h *fakeHelper) *Daemon {
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
		Deps{Collector: c, FS: b, Now: clk.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Helper: h})
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

// runVerified samples every 10 seconds and lets the verifier look after each
// sample, as Run does.
func runVerified(t *testing.T, d *Daemon, clk *fakeClock, dur time.Duration, step func(i int)) {
	t.Helper()
	ctx := context.Background()
	for i := 0; time.Duration(i)*SampleEvery < dur; i++ {
		if step != nil {
			step(i)
		}
		if err := d.sampleOnce(ctx, clk.now); err != nil {
			t.Fatal(err)
		}
		d.verify.check(ctx, clk.now, d.last)
		clk.now = clk.now.Add(SampleEvery)
	}
}

func limitTx(id string, at time.Time, cgroups ...string) *helperv1.Transaction {
	return &helperv1.Transaction{Id: id, State: helperv1.State_STATE_VERIFYING, Started: timestamppb.New(at), StateSince: timestamppb.New(at),
		Cgroups: cgroups, AutoUndo: true,
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_ServiceLimits{ServiceLimits: &helperv1.ServiceLimitsFix{Limits: []*helperv1.ServiceLimit{{Unit: "app.service", HighBytes: 1 << 30, MaxBytes: 1280 << 20}}}}}}
}

func verifyBox(t *testing.T) (*testbox.Box, *testbox.Workload, *fakeClock, *fakeHelper, *Daemon) {
	t.Helper()
	b, app := shop()
	app.Started = 100
	clk := &fakeClock{now: time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)}
	b.BootUnix = clk.now.Add(-24 * time.Hour).Unix()
	h := &fakeHelper{clk: clk}
	d := newVerifiedDaemon(t, b, clk, h)
	return b, app, clk, h, d
}

func TestQuietFixMovesOn(t *testing.T) {
	_, _, clk, h, d := verifyBox(t)
	tx := limitTx("20261031-090000-service-limits", clk.now, "system.slice/app.service")
	h.txs = append(h.txs, tx)
	runVerified(t, d, clk, 29*time.Minute, nil)
	if calls := h.callList(); len(calls) != 0 {
		t.Fatalf("acted before 30 minutes: %q", calls)
	}
	ch := d.verify.changes(clk.now)
	if len(ch.Changes) != 1 || ch.Changes[0].State != "verifying" || !ch.Changes[0].VerifyEnds.Equal(tx.GetStateSince().AsTime().Add(30*time.Minute)) {
		t.Errorf("changes while verifying: %+v", ch)
	}
	runVerified(t, d, clk, 2*time.Minute, nil)
	if calls := h.callList(); !slices.Equal(calls, []string{"advance 20261031-090000-service-limits to watching: "}) {
		t.Fatalf("calls %q", calls)
	}
	runVerified(t, d, clk, time.Minute, nil)
	clk.now = clk.now.Add(25 * time.Hour)
	runVerified(t, d, clk, time.Minute, nil)
	if c := d.verify.changes(clk.now).Changes[0]; c.State != "watching" || !c.Counted {
		t.Errorf("after a full day of watching: %+v", c)
	}
	clk.now = clk.now.Add(6 * 24 * time.Hour)
	runVerified(t, d, clk, time.Minute, nil)
	if calls := h.callList(); len(calls) != 2 || !strings.HasPrefix(calls[1], "advance 20261031-090000-service-limits to done") {
		t.Errorf("calls after 7 days: %q", calls)
	}
}

func TestTroubleUndoesALimit(t *testing.T) {
	cases := []struct {
		name  string
		step  func(app *testbox.Workload)
		every bool
		want  string
	}{
		{"a kill", func(app *testbox.Workload) { app.OOMKills++ }, false, "the kernel killed a process in app.service for lack of memory"},
		{"a restart", func(app *testbox.Workload) { app.Started = 90000 }, false, "app.service restarted"},
		// One second of stall per 10-second sample crosses the 5% bar on
		// the fourth sample, at 4 of the last 60 seconds.
		{"stalls with no history", func(app *testbox.Workload) { app.PressureSome += time.Second }, true, "app.service stalled on memory 6.7% of the last minute, and there is no pressure history to compare with"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, app, clk, h, d := verifyBox(t)
			h.txs = append(h.txs, limitTx("20261031-090000-service-limits", clk.now, "system.slice/app.service"))
			runVerified(t, d, clk, 5*time.Minute, func(i int) {
				if i == 12 || tc.every && i > 12 {
					b.Update(func(*testbox.Box) { tc.step(app) })
				}
			})
			calls := h.callList()
			if len(calls) != 1 || calls[0] != "undo 20261031-090000-service-limits: "+tc.want {
				t.Fatalf("calls %q", calls)
			}
			evs, err := d.st.Events(context.Background(), time.Time{}, 10)
			if err != nil || !slices.ContainsFunc(evs, func(e store.Event) bool { return e.Kind == "fix_undone" && strings.Contains(e.Detail, tc.want) }) {
				t.Errorf("events %+v, %v", evs, err)
			}
		})
	}
}

// A workload that stalls about 1% of each interval for two hours, then 30%
// once a fix is in, fails against its own 99th percentile.
func TestPressureAgainstHistory(t *testing.T) {
	b, app, clk, h, d := verifyBox(t)
	runVerified(t, d, clk, 2*time.Hour, func(int) {
		b.Update(func(*testbox.Box) { app.PressureSome += 100 * time.Millisecond })
	})
	if err := d.rec.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.txs = append(h.txs, limitTx("20261031-110000-service-limits", clk.now, "system.slice/app.service"))
	h.mu.Unlock()
	d.verify.dirty = true
	runVerified(t, d, clk, 3*time.Minute, func(i int) {
		b.Update(func(*testbox.Box) {
			app.PressureSome += 100 * time.Millisecond
			if i >= 6 {
				app.PressureSome += 2900 * time.Millisecond
			}
		})
	})
	calls := h.callList()
	if len(calls) != 1 || !strings.Contains(calls[0], "above the 99th percentile of 1.0% it showed before the fix") {
		t.Fatalf("calls %q", calls)
	}
}

func TestPauseHoldsTheUndo(t *testing.T) {
	b, app, clk, h, d := verifyBox(t)
	h.txs = append(h.txs, limitTx("20261031-090000-service-limits", clk.now, "system.slice/app.service"))
	if err := os.WriteFile(d.cfg.Pause, []byte("paused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runVerified(t, d, clk, 3*time.Minute, func(i int) {
		if i == 6 {
			b.Update(func(*testbox.Box) { app.OOMKills++ })
		}
	})
	if calls := h.callList(); len(calls) != 0 {
		t.Fatalf("acted while paused: %q", calls)
	}
	if c := d.verify.changes(clk.now).Changes[0]; !strings.Contains(c.Trouble, "killed") {
		t.Errorf("the held trouble is not shown: %+v", c)
	}
	if err := os.Remove(d.cfg.Pause); err != nil {
		t.Fatal(err)
	}
	runVerified(t, d, clk, 20*time.Second, nil)
	if calls := h.callList(); len(calls) != 1 || !strings.HasPrefix(calls[0], "undo ") {
		t.Errorf("after resume: %q", calls)
	}
}

func TestHeapCapsAreReopenedNotUndone(t *testing.T) {
	b, app, clk, h, d := verifyBox(t)
	tx := limitTx("20261031-090000-heap-caps", clk.now, "system.slice/app.service")
	tx.AutoUndo = false
	h.txs = append(h.txs, tx)
	runVerified(t, d, clk, 3*time.Minute, func(i int) {
		if i == 6 {
			b.Update(func(*testbox.Box) { app.OOMKills++ })
		}
	})
	calls := h.callList()
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "advance 20261031-090000-heap-caps to reopened: the kernel killed") {
		t.Errorf("calls %q", calls)
	}
}

func TestWatchingReopensOnThrottling(t *testing.T) {
	b, app, clk, h, d := verifyBox(t)
	tx := limitTx("20261031-090000-service-limits", clk.now.Add(-2*time.Hour), "system.slice/app.service")
	tx.State = helperv1.State_STATE_WATCHING
	h.txs = append(h.txs, tx)
	runVerified(t, d, clk, 3*time.Minute, func(i int) {
		if i == 6 {
			b.Update(func(*testbox.Box) { app.HighEvents += 40 })
		}
	})
	calls := h.callList()
	if len(calls) != 1 || calls[0] != "advance 20261031-090000-service-limits to reopened: app.service went over its MemoryHigh 40 times, so it is throttled" {
		t.Errorf("calls %q", calls)
	}
}

// After a reboot in the middle of verification, the 30 minutes start again
// at boot, and the restart the reboot caused is not held against the fix.
func TestRebootRestartsTheWindow(t *testing.T) {
	b, _, clk, h, d := verifyBox(t)
	applied := clk.now.Add(-20 * time.Minute)
	b.BootUnix = clk.now.Add(-time.Minute).Unix()
	d.machine.BootTime = time.Unix(b.BootUnix, 0).UTC()
	h.txs = append(h.txs, limitTx("20261031-084000-service-limits", applied, "system.slice/app.service"))
	runVerified(t, d, clk, 28*time.Minute, nil)
	if calls := h.callList(); len(calls) != 0 {
		t.Fatalf("moved on %v after boot, before 30 minutes: %q", clk.now.Sub(d.machine.BootTime), calls)
	}
	runVerified(t, d, clk, 3*time.Minute, nil)
	if calls := h.callList(); len(calls) != 1 || !strings.Contains(calls[0], "to watching") {
		t.Errorf("calls %q", calls)
	}
}

func TestContainerRestartCountsFromDocker(t *testing.T) {
	b, _, clk, h, d := verifyBox(t)
	id := strings.Repeat("ab", 32)
	b.Add(&testbox.Workload{Path: "system.slice/docker-" + id + ".scope", Programs: []string{"node"}, Anon: 300 * mib, Started: 200})
	if err := d.deps.Collector.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.cts = []*helperv1.Container{{Id: id, Name: "shop-web-1", Running: true, RestartCount: 1}}
	h.txs = append(h.txs, limitTx("20261031-090000-container-limits", clk.now, "system.slice/docker-"+id+".scope"))
	runVerified(t, d, clk, 4*time.Minute, func(i int) {
		if i == 12 {
			h.mu.Lock()
			h.cts = []*helperv1.Container{{Id: id, Name: "shop-web-1", Running: true, RestartCount: 2}}
			h.mu.Unlock()
		}
	})
	calls := h.callList()
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "restarted") {
		t.Errorf("calls %q", calls)
	}
}

// A kill or a restart in the minutes between the fix and the watcher's next
// read of the helper's list still counts against the fix.
func TestTroubleBeforeTheFirstLook(t *testing.T) {
	for _, tc := range []struct {
		name string
		step func(b *testbox.Box, app *testbox.Workload, applied time.Time)
		want string
	}{
		{"a kill", func(_ *testbox.Box, app *testbox.Workload, _ time.Time) { app.OOMKills++ }, "the kernel killed a process in app.service"},
		{"a restart", func(b *testbox.Box, app *testbox.Workload, applied time.Time) {
			app.Started = uint64(applied.Add(30*time.Second).Sub(time.Unix(b.BootUnix, 0)) / clockTick)
		}, "app.service restarted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, app, clk, h, d := verifyBox(t)
			runVerified(t, d, clk, 10*time.Second, nil)
			applied := clk.now
			h.mu.Lock()
			h.txs = append(h.txs, limitTx("20261031-090010-service-limits", applied, "system.slice/app.service"))
			h.mu.Unlock()
			runVerified(t, d, clk, 6*time.Minute, func(i int) {
				if i == 6 {
					b.Update(func(*testbox.Box) { tc.step(b, app, applied) })
				}
			})
			calls := h.callList()
			if len(calls) != 1 || !strings.HasPrefix(calls[0], "undo 20261031-090010-service-limits: "+tc.want) {
				t.Errorf("calls %q", calls)
			}
		})
	}
}

func TestChangesEndpoint(t *testing.T) {
	_, _, clk, h, d := verifyBox(t)
	tx := limitTx("20261031-090000-service-limits", clk.now, "system.slice/app.service")
	tx.Changes = []*helperv1.Change{{Target: "/etc/systemd/system/app.service.d/50-snugbox-memory.conf", NewValue: "[Service]\n", File: true}}
	h.txs = append(h.txs, tx, &helperv1.Transaction{Id: "20261030-090000-swap", State: helperv1.State_STATE_UNDONE, Reason: "undone on request",
		Fix: &helperv1.Fix{Kind: &helperv1.Fix_Swap{Swap: &helperv1.SwapFix{SizeBytes: 1 << 30}}}})
	runVerified(t, d, clk, time.Minute, nil)
	rec := httptest.NewRecorder()
	d.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/changes", nil))
	var got localapi.Changes
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	if !got.Helper || len(got.Changes) != 2 {
		t.Fatalf("changes %+v", got)
	}
	if c := got.Changes[1]; c.ID != "20261031-090000-service-limits" || c.Kind != "service-limits" || c.State != "verifying" || len(c.Changes) != 1 {
		t.Errorf("first change %+v", c)
	}
	if c := got.Changes[0]; c.Kind != "swap" || c.State != "undone" || c.Reason != "undone on request" {
		t.Errorf("second change %+v", c)
	}
}
