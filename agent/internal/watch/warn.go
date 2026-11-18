package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"snugbox.dev/snugbox/agent/internal/fix"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/report"
)

// Early warning settings from blueprint section 7.6.
const (
	TrendWindow   = 2 * time.Hour
	RisingWindow  = 10 * time.Minute
	ForecastAhead = 30 * time.Minute
	DailyLookback = 7
	WarnCooldown  = time.Hour
	OutcomeAfter  = time.Hour
)

// sample is one point of a series: its time in Unix seconds, as the store
// keeps it, and its value. The warner holds two hours of them for the box
// and for every workload, 36,000 for 50 workloads.
type sample struct {
	at int64
	v  float64
}

// Forecast is where a series is heading.
type Forecast struct {
	// Level is the expected value ForecastAhead from now: the trend of the
	// last two hours, or the level the same half hour reached on one of the
	// last seven days, whichever is higher.
	Level float64
	// PerMinute is the trend's growth, in bytes a minute.
	PerMinute float64
	// Rising is true when the last ten minutes still grow, which takes a
	// series that covers ten minutes.
	Rising bool
}

// slope fits a least-squares line to the samples and returns its slope in
// units a second.
func slope(pts []sample) float64 {
	if len(pts) < 3 {
		return 0
	}
	t0 := pts[0].at
	var n, sx, sy, sxx, sxy float64
	for _, p := range pts {
		x := float64(p.at - t0)
		n++
		sx += x
		sy += p.v
		sxx += x * x
		sxy += x * p.v
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}

// forecast projects a series ForecastAhead past its last sample. daily is the
// highest level the same window reached on earlier days, or zero.
func forecast(pts []sample, daily float64) Forecast {
	if len(pts) == 0 {
		return Forecast{Level: daily}
	}
	last := pts[len(pts)-1]
	trend := slope(pts)
	var recent []sample
	for _, p := range pts {
		if time.Duration(last.at-p.at)*time.Second <= RisingWindow {
			recent = append(recent, p)
		}
	}
	// A service climbs to its usual size in its first minute, snugboxd
	// itself on every start, and a trend from that climb alone would reach
	// any limit; so a series shorter than ten minutes isn't rising yet.
	long := time.Duration(last.at-pts[0].at)*time.Second >= RisingWindow
	f := Forecast{PerMinute: trend * 60, Rising: long && slope(recent) > 0}
	f.Level = last.v
	if trend > 0 {
		f.Level += trend * ForecastAhead.Seconds()
	}
	f.Level = math.Max(f.Level, daily)
	return f
}

type pending struct {
	at     time.Time
	target string
	name   string
}

type warner struct {
	d       *Daemon
	last    map[string]time.Time
	pending []pending
	client  *http.Client
	// box and work hold the last TrendWindow of the box's total working set
	// and of each workload's, as the store holds them. The first check reads
	// them from the store; after it, record keeps them up from each sample,
	// so a check every minute reads two hours of points from memory rather
	// than from the database.
	box      []sample
	memTotal float64
	work     map[string][]sample
	loaded   bool
}

func newWarner(d *Daemon) *warner {
	return &warner{d: d, last: make(map[string]time.Time), client: &http.Client{Timeout: 5 * time.Second}}
}

// windowPoints is how many samples a series holds at one every SampleEvery,
// with room for the one record adds before trim drops the oldest, so a
// full series never has to grow.
const windowPoints = int(TrendWindow/SampleEvery) + 4

// newSeries makes a series of n samples with room for the window.
func newSeries(n int) []sample { return make([]sample, n, max(n, windowPoints)) }

// load reads the trend window since a time from the store.
func (w *warner) load(ctx context.Context, since time.Time) error {
	recent, err := w.d.st.Recent(ctx, since)
	if err != nil {
		return err
	}
	per, err := w.d.st.WorkloadPoints(ctx, since)
	if err != nil {
		return err
	}
	w.box = newSeries(len(recent))
	for i, p := range recent {
		w.box[i] = sample{p.Time.Unix(), float64(p.Total)}
	}
	if len(recent) > 0 {
		w.memTotal = float64(recent[len(recent)-1].MemTotal)
	}
	w.work = make(map[string][]sample, len(per))
	for id, pts := range per {
		series := newSeries(len(pts))
		for i, p := range pts {
			series[i] = sample{p.Time.Unix(), float64(p.WorkingSet)}
		}
		w.work[id] = series
	}
	w.loaded = true
	return nil
}

// record adds a sample the store has just taken, once the first check has
// read the window. Times are whole seconds, as the store keeps them.
func (w *warner) record(s model.Sample, p model.Point) {
	if !w.loaded {
		return
	}
	at := s.Time.Unix()
	w.box = append(w.box, sample{at, float64(p.Total)})
	w.memTotal = float64(s.Box.MemTotal)
	for id, wp := range p.Workloads {
		series, ok := w.work[id]
		if !ok {
			series = newSeries(0)
		}
		w.work[id] = append(series, sample{at, float64(wp.WorkingSet)})
	}
	w.trim(time.Unix(at, 0).Add(-TrendWindow))
}

// trim drops the points older than since, by whole seconds as the store's
// query does, and the workloads left with none.
func (w *warner) trim(since time.Time) {
	cut := since.Unix()
	keep := func(pts []sample) []sample {
		i := 0
		for i < len(pts) && pts[i].at < cut {
			i++
		}
		if i == 0 {
			return pts
		}
		return pts[:copy(pts, pts[i:])]
	}
	w.box = keep(w.box)
	for id, pts := range w.work {
		if pts = keep(pts); len(pts) == 0 {
			delete(w.work, id)
		} else {
			w.work[id] = pts
		}
	}
}

// check looks for the box or a limited workload heading over its limit, and
// settles warnings old enough to judge.
func (w *warner) check(ctx context.Context, now time.Time) {
	log := w.d.deps.Log
	w.settle(ctx, now)
	since := now.Add(-TrendWindow)
	if !w.loaded {
		if err := w.load(ctx, since); err != nil {
			log.Error("early warning: trend window", "err", err)
			return
		}
	}
	w.trim(since)
	if len(w.box) == 0 {
		return
	}
	limit, box := w.memTotal, w.box
	daily, err := w.daily(ctx, now)
	if err != nil {
		log.Error("early warning: daily peaks", "err", err)
	}
	if f := forecast(box, daily); f.Rising && f.Level >= limit && box[len(box)-1].v < limit {
		grower, rate := fastestGrower(w.work)
		w.raise(ctx, now, "", w.d.Machine().Hostname, f, box[len(box)-1].v, limit, grower, rate)
	}

	r := w.d.Result()
	if r == nil {
		return
	}
	for id, series := range w.work {
		wl, ok := r.Workloads[id]
		if !ok || !wl.Max.Set {
			continue
		}
		lim := float64(wl.Max.Bytes)
		if f := forecast(series, 0); f.Rising && f.Level >= lim && series[len(series)-1].v < lim {
			w.raise(ctx, now, id, wl.Label(), f, series[len(series)-1].v, lim, id, f.PerMinute)
		}
	}
}

// daily returns the highest total working set the next half hour reached on
// any of the last seven days.
func (w *warner) daily(ctx context.Context, now time.Time) (float64, error) {
	var best float64
	for day := 1; day <= DailyLookback; day++ {
		from := now.Add(-time.Duration(day) * 24 * time.Hour)
		rows, err := w.d.st.MinuteMaxima(ctx, from, from.Add(ForecastAhead))
		if err != nil {
			return best, err
		}
		for _, m := range rows {
			best = math.Max(best, float64(m.Total))
		}
	}
	return best, nil
}

func fastestGrower(per map[string][]sample) (string, float64) {
	var best string
	var rate float64
	for id, series := range per {
		if s := slope(series) * 60; s > rate {
			best, rate = id, s
		}
	}
	return best, rate
}

// Warning is what a webhook receives.
type Warning struct {
	Box           string    `json:"box"`
	Time          time.Time `json:"time"`
	Workload      string    `json:"workload,omitempty"`
	ForecastBytes uint64    `json:"forecast_bytes"`
	LimitBytes    uint64    `json:"limit_bytes"`
	Message       string    `json:"message"`
}

func (w *warner) raise(ctx context.Context, now time.Time, target, name string, f Forecast, current, limit float64, grower string, rate float64) {
	if last, ok := w.last[target]; ok && now.Sub(last) < WarnCooldown {
		return
	}
	w.last[target] = now
	msg := w.message(target, name, f, current, limit, grower, rate)
	w.d.event(ctx, now, "warning", name, msg)
	w.pending = append(w.pending, pending{at: now, target: target, name: name})
	if w.d.cfg.Webhook != "" {
		warning := Warning{Box: w.d.Machine().Hostname, Time: now, ForecastBytes: uint64(f.Level), LimitBytes: uint64(limit), Message: msg}
		if target != "" {
			warning.Workload = name
		}
		w.post(ctx, warning)
	}
}

// withinMinutes says how soon, in whole minutes.
func withinMinutes(n int) string {
	if n == 1 {
		return "within 1 minute"
	}
	return fmt.Sprintf("within %d minutes", n)
}

// message says what is heading where, how soon, which workload is growing
// and which fix would help most.
func (w *warner) message(target, name string, f Forecast, current, limit float64, grower string, rate float64) string {
	minutes := ForecastAhead.Minutes()
	if f.PerMinute > 0 {
		minutes = math.Min(minutes, math.Max(1, (limit-current)/f.PerMinute))
	}
	var b strings.Builder
	if target == "" {
		fmt.Fprintf(&b, "%s may run out of memory %s", name, withinMinutes(int(math.Ceil(minutes))))
	} else {
		fmt.Fprintf(&b, "%s may reach its %s limit %s", name, report.Size(uint64(limit)), withinMinutes(int(math.Ceil(minutes))))
	}
	r := w.d.Result()
	label := grower
	if r != nil {
		if wl, ok := r.Workloads[grower]; ok {
			label = wl.Label()
		}
	}
	if grower != "" && rate > 0 {
		fmt.Fprintf(&b, ": %s is growing %s a minute.", label, report.Size(uint64(rate)))
	} else {
		b.WriteString(".")
	}
	if r == nil {
		return b.String()
	}
	for i, p := range r.Fixes {
		for _, l := range p.Limits {
			if l.WorkloadID == grower {
				fmt.Fprintf(&b, " Fix %d would help most: %s.", i+1, report.FixTitle(p))
				return b.String()
			}
		}
	}
	for i, p := range r.Fixes {
		if p.Kind == fix.KindSwap {
			fmt.Fprintf(&b, " Fix %d would give it room: %s.", i+1, report.FixTitle(p))
			return b.String()
		}
	}
	return b.String()
}

// webhookURL checks the address warnings go to: HTTPS, with a host.
func webhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the webhook must be an https:// address")
	}
	return u, nil
}

// post sends one warning, once; a webhook that fails or refuses it is
// logged, and the warning stays in the event history.
func (w *warner) post(ctx context.Context, warning Warning) {
	log := w.d.deps.Log
	u, err := webhookURL(w.d.cfg.Webhook)
	if err != nil {
		log.Error("warning not sent", "err", err)
		return
	}
	body, err := json.Marshal(warning)
	if err != nil {
		log.Error("webhook body", "err", err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		log.Error("webhook request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		log.Error("webhook failed", "host", u.Host, "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Error("webhook refused the warning", "host", u.Host, "status", resp.StatusCode)
	}
}

// settle records what followed each warning an hour on, so the share of
// warnings followed by real trouble is a number Snugbox can report.
func (w *warner) settle(ctx context.Context, now time.Time) {
	keep := w.pending[:0]
	for _, p := range w.pending {
		if now.Sub(p.at) < OutcomeAfter {
			keep = append(keep, p)
			continue
		}
		outcome := "nothing followed"
		events, err := w.d.st.Events(ctx, p.at, 1000)
		if err != nil {
			keep = append(keep, p)
			continue
		}
		for _, e := range events {
			if e.Kind == "oom_kill" && !e.Time.After(p.at.Add(OutcomeAfter)) && (p.target == "" || e.Workload == p.name) {
				outcome = "a kill followed"
				break
			}
		}
		if outcome == "nothing followed" {
			mins, err := w.d.st.MinuteMaxima(ctx, p.at, p.at.Add(OutcomeAfter))
			if err == nil {
				for _, m := range mins {
					if m.PressureFull > 0 {
						outcome = "memory pressure followed"
						break
					}
				}
			}
		}
		w.d.event(ctx, now, "warning_outcome", p.name, outcome)
	}
	w.pending = keep
}
