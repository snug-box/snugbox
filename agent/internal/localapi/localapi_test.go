package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
)

// serve answers h on a Unix socket in a fresh folder and returns its path.
// The folder name is short because a socket path is limited to 107 bytes.
func serve(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return path
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestClientReadsEachEndpoint(t *testing.T) {
	started := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Status{Version: "1.2.3", Started: started, Paused: true, Interval: 10 * time.Second, Workloads: 4, StoreBytes: 1 << 20})
	})
	mux.HandleFunc("GET /v1/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, Live{Time: started, Box: []LivePoint{{Time: started, Hard: 300, MemTotal: 1000}},
			Workloads: []LiveWorkload{{ID: "/system.slice/app.service", Name: "app.service", Kind: "service", WorkingSet: 200, PressureSome: time.Second}}})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		writeJSON(w, http.StatusOK, []Event{{Time: started, Kind: "oom_kill", Workload: "app.service", Detail: "killed"}})
	})
	mux.HandleFunc("GET /v1/changes", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		writeJSON(w, http.StatusOK, Changes{Helper: true, Read: started, Changes: []Change{{ID: "20261031-090000-swap", Kind: "swap", State: "verifying",
			Changes: []ChangeItem{{Target: "/var/lib/snugbox-helper/swapfile", New: "1 GiB", File: true}}}}})
	})
	mux.HandleFunc("GET /v1/report", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		writeJSON(w, http.StatusOK, Report{From: started, To: started.Add(time.Hour), Then: &Snapshot{Snug: 2 << 30, Confidence: "high"},
			Resizes: []Resize{{Time: started, OldRAM: 8 << 30, NewRAM: 4 << 30, OldCents: 4800, NewCents: 2400}}, Throttled: map[string]int{"app.service": 3}})
	})
	c := NewClient(serve(t, mux))
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "1.2.3" || !st.Started.Equal(started) || !st.Paused || st.Interval != 10*time.Second || st.Workloads != 4 || st.StoreBytes != 1<<20 {
		t.Errorf("status %+v", st)
	}
	live, err := c.Live(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Box) != 1 || live.Box[0].Hard != 300 || len(live.Workloads) != 1 || live.Workloads[0].PressureSome != time.Second {
		t.Errorf("live %+v", live)
	}
	ist := time.FixedZone("IST", 5*3600+1800)
	evs, err := c.Events(ctx, time.Date(2026, 10, 31, 14, 30, 0, 0, ist))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "oom_kill" {
		t.Errorf("events %+v", evs)
	}
	ch, err := c.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Helper || len(ch.Changes) != 1 || ch.Changes[0].State != "verifying" || !ch.Changes[0].Changes[0].File {
		t.Errorf("changes %+v", ch)
	}
	if err := c.Nudge(ctx); err != nil {
		t.Fatal(err)
	}
	rep, err := c.Report(ctx, time.Date(2026, 10, 31, 14, 30, 0, 0, ist))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Then == nil || rep.Then.Snug != 2<<30 || len(rep.Resizes) != 1 || rep.Resizes[0].NewCents != 2400 || rep.Throttled["app.service"] != 3 {
		t.Errorf("report %+v", rep)
	}
	mu.Lock()
	defer mu.Unlock()
	// Events and Report ask in UTC whatever zone the caller's time is in,
	// and only a nudge asks the watcher to read the helper again.
	want := []string{"since=2026-10-31T09:00:00Z", "", "refresh=1", "since=2026-10-31T09:00:00Z"}
	if strings.Join(queries, "|") != strings.Join(want, "|") {
		t.Errorf("queries %q, want %q", queries, want)
	}
}

func TestResultGetsTheCatalogueBack(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/result", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, audit.Result{Watched: true})
	})
	r, err := NewClient(serve(t, mux)).Result(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Watched || r.Advice.Catalogue == nil {
		t.Errorf("result %+v: the catalogue, which doesn't travel as JSON, was not put back", r.Advice)
	}
}

func TestClientErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/result", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the first analysis runs a few minutes after the watcher starts"})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream trouble", http.StatusBadGateway)
	})
	mux.HandleFunc("GET /v1/live", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	})
	c := NewClient(serve(t, mux))
	ctx := context.Background()

	if _, err := c.Result(ctx); err == nil || err.Error() != "the first analysis runs a few minutes after the watcher starts" {
		t.Errorf("Result = %v, want the watcher's own message", err)
	}
	if _, err := c.Status(ctx); err == nil || err.Error() != "snugboxd answered 502" {
		t.Errorf("Status = %v, want the status code when the body is not an API error", err)
	}
	if _, err := c.Live(ctx); err == nil {
		t.Error("Live accepted a body that is not JSON")
	}
	if _, err := c.Events(ctx, time.Now()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("Events = %v, want the 404 of an unknown path", err)
	}
}

func TestNoWatcher(t *testing.T) {
	dir, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := NewClient(filepath.Join(dir, "missing.sock"))
	if _, err := c.Status(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("no socket file: Status = %v, want ErrNotRunning", err)
	}

	// A watcher that stopped without removing its socket leaves a file that
	// refuses connections.
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("the stale socket file is gone: %v", err)
	}
	if _, err := NewClient(stale).Status(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stale socket file: Status = %v, want ErrNotRunning", err)
	}
}
