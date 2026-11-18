package watch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"time"

	"snugbox.dev/snugbox/agent/internal/localapi"
	"snugbox.dev/snugbox/agent/internal/version"
)

// The local API is plain HTTP and JSON on a Unix socket that only root and
// the snugbox group can open, so curl --unix-socket can read it too. It is
// read-only: nothing it serves changes the box.

// Handler returns the API's HTTP handler.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", d.handleStatus)
	mux.HandleFunc("GET /v1/result", d.handleResult)
	mux.HandleFunc("GET /v1/live", d.handleLive)
	mux.HandleFunc("GET /v1/events", d.handleEvents)
	mux.HandleFunc("GET /v1/changes", d.handleChanges)
	mux.HandleFunc("GET /v1/report", d.handleReport)
	return mux
}

// reportEvents is the most events one report carries; a period with more
// keeps the newest.
const reportEvents = 1000

func (d *Daemon) handleReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := d.deps.Now()
	from, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	if err != nil || !from.Before(now) {
		d.writeError(w, http.StatusBadRequest, "since must be an RFC 3339 time before now")
		return
	}
	rep, err := d.report(ctx, from, now)
	if err != nil {
		d.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.writeJSON(w, http.StatusOK, rep)
}

func (d *Daemon) report(ctx context.Context, from, now time.Time) (localapi.Report, error) {
	rep := localapi.Report{From: from, To: now, Resizes: []localapi.Resize{}, Events: []localapi.Event{}}
	start, ok, err := d.st.WatchStart(ctx)
	if err != nil {
		return rep, err
	}
	if ok {
		rep.WatchStart = start
	}
	for _, at := range []struct {
		t   time.Time
		dst **localapi.Snapshot
	}{{from, &rep.Then}, {now, &rep.Now}} {
		sn, ok, err := d.st.SnapshotAt(ctx, at.t)
		if err != nil {
			return rep, err
		}
		if ok {
			s := localapi.Snapshot(sn)
			*at.dst = &s
		}
	}
	resizes, err := d.st.Resizes(ctx, from)
	if err != nil {
		return rep, err
	}
	for _, rz := range resizes {
		rep.Resizes = append(rep.Resizes, localapi.Resize(rz))
	}
	evs, err := d.st.Events(ctx, from, reportEvents)
	if err != nil {
		return rep, err
	}
	for _, e := range evs {
		if e.Kind == "limit_high" {
			if rep.Throttled == nil {
				rep.Throttled = make(map[string]int)
			}
			rep.Throttled[e.Workload]++
			continue
		}
		rep.Events = append(rep.Events, localapi.Event(e))
	}
	if d.verify != nil {
		rep.Changes = d.verify.changes(now)
	} else {
		rep.Changes = localapi.Changes{Error: "snugboxd runs without snugbox-helper", Changes: []localapi.Change{}}
	}
	return rep, nil
}

func (d *Daemon) handleChanges(w http.ResponseWriter, r *http.Request) {
	if d.verify == nil {
		d.writeError(w, http.StatusServiceUnavailable, "snugboxd runs without snugbox-helper, so it verifies no fixes")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		d.verify.poke()
	}
	d.writeJSON(w, http.StatusOK, d.verify.changes(d.deps.Now()))
}

func (d *Daemon) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		d.deps.Log.Debug("api: the client left before the answer was sent", "err", err)
	}
}

func (d *Daemon) writeError(w http.ResponseWriter, status int, msg string) {
	d.writeJSON(w, status, map[string]string{"error": msg})
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	size, err := d.st.Size(r.Context())
	if err != nil {
		d.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	start, _, err := d.st.WatchStart(r.Context())
	if err != nil {
		d.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	d.mu.RLock()
	st := localapi.Status{Version: version.Version, Started: d.started, Paused: d.Paused(), Interval: d.interval, LastSample: d.last.Time,
		Workloads: len(d.last.Workloads), StoreBytes: size, WatchStart: start, Analysed: d.result != nil}
	d.mu.RUnlock()
	d.writeJSON(w, http.StatusOK, st)
}

func (d *Daemon) handleResult(w http.ResponseWriter, _ *http.Request) {
	r := d.Result()
	if r == nil {
		d.writeError(w, http.StatusServiceUnavailable, "the first analysis runs a few minutes after the watcher starts; try again shortly")
		return
	}
	d.writeJSON(w, http.StatusOK, r)
}

func (d *Daemon) handleLive(w http.ResponseWriter, r *http.Request) {
	now := d.deps.Now()
	box, err := d.st.Recent(r.Context(), now.Add(-15*time.Minute))
	if err != nil {
		d.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ws := d.deps.Collector.Workloads()
	d.mu.RLock()
	s, p := d.last, d.lastPt
	d.mu.RUnlock()
	live := localapi.Live{Time: s.Time, Box: make([]localapi.LivePoint, len(box))}
	for i, b := range box {
		live.Box[i] = localapi.LivePoint(b)
	}
	for id, wp := range p.Workloads {
		wl := ws[id]
		lw := localapi.LiveWorkload{ID: id, Name: wl.Label(), Kind: wl.Kind, Programs: wl.Programs, WorkingSet: wp.WorkingSet, Hard: wp.Hard,
			Hot: wp.HotFile, Current: wp.Current, PressureSome: s.Workloads[id].PressureSome}
		if wl.Max.Set {
			lw.Limit = wl.Max.Bytes
		}
		live.Workloads = append(live.Workloads, lw)
	}
	slices.SortFunc(live.Workloads, func(a, b localapi.LiveWorkload) int {
		switch {
		case a.WorkingSet > b.WorkingSet:
			return -1
		case a.WorkingSet < b.WorkingSet:
			return 1
		}
		return 0
	})
	live.Workloads = live.Workloads[:min(len(live.Workloads), liveWorkloadMax)]
	d.writeJSON(w, http.StatusOK, live)
}

func (d *Daemon) handleEvents(w http.ResponseWriter, r *http.Request) {
	since := d.deps.Now().Add(-7 * 24 * time.Hour)
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			d.writeError(w, http.StatusBadRequest, "since must be an RFC 3339 time")
			return
		}
		since = t
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			d.writeError(w, http.StatusBadRequest, "limit must be from 1 to 10000")
			return
		}
		limit = n
	}
	evs, err := d.st.Events(r.Context(), since, limit)
	if err != nil {
		d.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]localapi.Event, len(evs))
	for i, e := range evs {
		out[i] = localapi.Event(e)
	}
	d.writeJSON(w, http.StatusOK, out)
}

// Serve answers the API on ln until ctx ends.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shut); err != nil {
			d.deps.Log.Error("api shutdown", "err", err)
		}
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
