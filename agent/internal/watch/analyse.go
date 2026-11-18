package watch

import (
	"context"
	"maps"
	"strconv"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/cron"
	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
)

func countText(n uint64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatUint(n, 10) + " " + many
}

// analyseNow runs the memory model over the watched window, at most the last
// 30 days, stores a snapshot when the answer changed, and keeps the result
// for the command line.
func (d *Daemon) analyseNow(ctx context.Context) error {
	if err := d.rec.Flush(ctx); err != nil {
		return err
	}
	now := d.deps.Now()
	start, ok, err := d.st.WatchStart(ctx)
	if err != nil || !ok {
		return err
	}
	from := start
	if limit := now.Add(-MaxWindow); from.Before(limit) {
		from = limit
	}
	sum, stored, err := d.st.Summary(ctx, from, now)
	if err != nil {
		return err
	}
	if sum.Samples == 0 {
		return nil
	}
	c := d.deps.Collector
	ws := maps.Clone(stored)
	maps.Copy(ws, c.Workloads())
	for id := range ws {
		if _, ok := sum.Workloads[id]; !ok {
			delete(ws, id)
		}
	}

	d.mu.RLock()
	machine, prov, last := d.machine, d.prov, d.last
	d.mu.RUnlock()
	h, err := d.history(ctx, now, start, last)
	if err != nil {
		return err
	}
	runs, err := d.st.Runs(ctx, now.Add(-fix.ScheduleWindow))
	if err != nil {
		return err
	}
	jobs, minutes, err := d.cronHistory(ctx, now)
	if err != nil {
		return err
	}
	r := &audit.Result{
		Started: sum.Start, Ended: now, Root: true, Watched: true, Offline: d.cfg.Offline,
		Machine: machine, Provider: prov, Workloads: ws, Summary: sum, History: h, Gaps: c.Gaps(),
		CrashLoops: c.CrashLoops(now), Runs: runs, Cron: jobs, Minutes: minutes,
	}
	audit.Assemble(r, d.deps.FS)
	if added, err := d.st.AddSnapshot(ctx, now, r.Estimate); err != nil {
		return err
	} else if added {
		d.deps.Log.Info("snug size", "snug", r.Estimate.Snug, "need", r.Estimate.Need, "confidence", r.Estimate.Confidence.String(), "window", r.Estimate.Window.Round(time.Minute))
	}
	d.mu.Lock()
	d.result = r
	d.mu.Unlock()
	return nil
}

// cronHistory reads the box's cron jobs and, when there are any, the box's
// highest working set in each minute of the schedule fix's window.
func (d *Daemon) cronHistory(ctx context.Context, now time.Time) ([]cron.Job, []model.MinuteTotal, error) {
	if d.deps.FS == nil {
		return nil, nil, nil
	}
	jobs := collect.CronJobs(d.deps.FS)
	if len(jobs) == 0 {
		return nil, nil, nil
	}
	rows, err := d.st.MinuteMaxima(ctx, now.Add(-fix.ScheduleWindow-time.Minute), now)
	if err != nil {
		return nil, nil, err
	}
	minutes := make([]model.MinuteTotal, len(rows))
	for i, m := range rows {
		minutes[i] = model.MinuteTotal{Time: m.Time, Total: m.Total}
	}
	return jobs, minutes, nil
}

// history is what the model knows from before and during the window. The
// watcher can't read the kernel log, so the kills it recorded itself stand in
// for it: every kill since watching began has a time.
func (d *Daemon) history(ctx context.Context, now, watchStart time.Time, last model.Sample) (model.History, error) {
	d.mu.RLock()
	uptime := d.machine.Uptime + now.Sub(d.started)
	d.mu.RUnlock()
	h := model.History{
		Now: now, Uptime: uptime,
		PressureFull: last.Box.PressureFull, HasPressure: last.Box.HasPressure, OOMKills: last.Box.OOMKills,
		LogRead: true, LogSince: watchStart,
	}
	events, err := d.st.Events(ctx, now.Add(-model.KillLookback), 10000)
	if err != nil {
		return h, err
	}
	for _, e := range events {
		if e.Kind == "oom_kill" {
			h.Kills = append(h.Kills, e.Time)
		}
	}
	return h, nil
}

// Result returns the latest analysis, or nil before the first one.
func (d *Daemon) Result() *audit.Result {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.result
}

// Machine returns the box's fixed facts.
func (d *Daemon) Machine() collect.Machine {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.machine
}
