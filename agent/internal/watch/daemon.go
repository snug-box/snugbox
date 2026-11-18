// Package watch is snugboxd, the watcher of blueprint section 5.3. It samples
// the box every 10 seconds, keeps the history in the store, works out the snug
// size every 5 minutes and after any out-of-memory kill, warns before a
// workload or the box runs out of memory, and answers the snugbox command on
// a local socket. It runs as the unprivileged snugbox user and changes
// nothing on the box.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	"snugbox.dev/snugbox/agent/internal/audit"
	"snugbox.dev/snugbox/agent/internal/collect"
	"snugbox.dev/snugbox/agent/internal/model"
	"snugbox.dev/snugbox/agent/internal/provider"
	"snugbox.dev/snugbox/agent/internal/store"
)

// Default paths and intervals.
const (
	SampleEvery     = 10 * time.Second
	DiscoverEvery   = time.Minute
	AnalyseEvery    = 5 * time.Minute
	WarnEvery       = time.Minute
	PruneEvery      = time.Hour
	ProviderEvery   = 24 * time.Hour
	MaxWindow       = 30 * 24 * time.Hour
	sampleBudget    = time.Second
	maxSampleEvery  = time.Minute
	liveWorkloadMax = 200
)

// Config sets where the watcher keeps its files.
type Config struct {
	DB      string
	Socket  string
	Pause   string
	Offline bool
	// Webhook, when set, receives early warnings as JSON over HTTPS.
	Webhook string
}

// Deps are the watcher's connections to the box; tests replace them.
type Deps struct {
	Collector *collect.Collector
	Host      collect.Host
	Prober    audit.Prober
	FS        fs.FS
	Now       func() time.Time
	Log       *slog.Logger
	// Helper is snugbox-helper, through which applied fixes are verified,
	// undone or moved on. Nil runs the watcher without verification.
	Helper Helper
}

// Daemon is a running watcher.
type Daemon struct {
	cfg  Config
	deps Deps
	st   *store.Store
	rec  *store.Recorder
	der  *model.Deriver
	warn *warner
	// verify is nil when the watcher runs without the helper.
	verify *verifier

	mu       sync.RWMutex
	machine  collect.Machine
	prov     provider.Info
	result   *audit.Result
	last     model.Sample
	lastPt   model.Point
	started  time.Time
	interval time.Duration
	prevKill map[string]uint64
	prevHigh map[string]uint64
	analyse  chan struct{}
}

// New opens the store and prepares a watcher.
func New(ctx context.Context, cfg Config, deps Deps) (*Daemon, error) {
	if deps.Collector.CgroupRoot() == "" {
		return nil, audit.ErrNoCgroupV2
	}
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		cfg: cfg, deps: deps, st: st,
		der: model.NewDeriver(deps.Collector.PageSize()), interval: SampleEvery,
		started: deps.Now(), prevKill: map[string]uint64{}, prevHigh: map[string]uint64{},
		analyse: make(chan struct{}, 1),
	}
	d.warn = newWarner(d)
	if cfg.Webhook != "" {
		if _, err := webhookURL(cfg.Webhook); err != nil {
			deps.Log.Error("SNUGBOX_WEBHOOK_URL is set but no warning will be sent to it", "err", err)
		}
	}
	if deps.Helper != nil {
		d.verify = newVerifier(d, deps.Helper)
	}
	return d, nil
}

// Close flushes the history and closes the store.
func (d *Daemon) Close(ctx context.Context) error {
	var err error
	if d.rec != nil {
		err = d.rec.Flush(ctx)
	}
	return errors.Join(err, d.st.Close())
}

// Paused reports whether snugbox pause is in force. Sampling goes on while
// paused; fixes and hub traffic stop.
func (d *Daemon) Paused() bool {
	_, err := os.Stat(d.cfg.Pause)
	return err == nil
}

// Run samples until ctx ends.
func (d *Daemon) Run(ctx context.Context) error {
	log := d.deps.Log
	c := d.deps.Collector
	if err := c.Discover(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	d.machine = c.Machine(ctx, d.deps.Host)
	d.mu.Unlock()
	d.rec = d.st.NewRecorder(d.machine.CPUs, c.PageSize())
	d.refreshProvider(ctx)
	d.checkResize(ctx, d.deps.Now())
	log.Info("watching", "cgroup_root", "/"+c.CgroupRoot(), "workloads", len(c.Workloads()), "interval", d.interval)

	timer := time.NewTimer(0)
	defer timer.Stop()
	var lastDiscover, lastAnalyse, lastWarn, lastPrune, lastProvider time.Time
	now := d.deps.Now()
	lastDiscover, lastProvider = now, now
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-d.analyse:
			if err := d.analyseNow(ctx); err != nil {
				log.Error("analysis failed", "err", err)
			}
			lastAnalyse = d.deps.Now()
			continue
		case <-timer.C:
		}
		start := d.deps.Now()
		if start.Sub(lastDiscover) >= DiscoverEvery {
			if err := c.Discover(ctx); err != nil {
				return err
			}
			lastDiscover = start
		}
		if err := d.sampleOnce(ctx, start); err != nil {
			log.Error("sample failed", "err", err)
		} else if d.verify != nil {
			d.mu.RLock()
			last := d.last
			d.mu.RUnlock()
			d.verify.check(ctx, start, last)
		}
		took := d.deps.Now().Sub(start)
		d.adjustInterval(took)
		if start.Sub(lastAnalyse) >= AnalyseEvery {
			if err := d.analyseNow(ctx); err != nil {
				log.Error("analysis failed", "err", err)
			}
			lastAnalyse = start
		}
		if start.Sub(lastWarn) >= WarnEvery {
			d.warn.check(ctx, start)
			lastWarn = start
		}
		if start.Sub(lastPrune) >= PruneEvery {
			if err := d.st.Prune(ctx, start); err != nil {
				log.Error("prune failed", "err", err)
			}
			lastPrune = start
		}
		if start.Sub(lastProvider) >= ProviderEvery {
			d.refreshProvider(ctx)
			lastProvider = start
		}
		timer.Reset(max(d.interval-took, time.Second))
	}
}

// adjustInterval backs off when a sample takes longer than its budget, which
// happens when the box is busy, and returns to 10 seconds when it is fast
// again.
func (d *Daemon) adjustInterval(took time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case took > sampleBudget && d.interval < maxSampleEvery:
		d.interval = min(2*d.interval, maxSampleEvery)
		d.deps.Log.Warn("sampling is slow; backing off", "took", took, "interval", d.interval)
	case took < sampleBudget/4 && d.interval > SampleEvery:
		d.interval = max(d.interval/2, SampleEvery)
	}
}

func (d *Daemon) refreshProvider(ctx context.Context) {
	if d.cfg.Offline || d.deps.Prober == nil {
		return
	}
	info := d.deps.Prober.Detect(ctx)
	d.mu.Lock()
	changed := d.prov.Known() && info.Known() && d.prov != info
	d.prov = info
	d.mu.Unlock()
	if changed {
		d.event(ctx, d.deps.Now(), "plan_change_seen", "", fmt.Sprintf("provider %s, region %s", info.Provider, info.Region))
	}
}

func (d *Daemon) sampleOnce(ctx context.Context, now time.Time) error {
	c := d.deps.Collector
	s := c.Sample(now)
	p := d.der.Add(s)
	ws := c.Workloads()
	if err := d.rec.Add(ctx, s, p, ws); err != nil {
		return err
	}
	d.warn.record(s, p)
	d.detectEvents(ctx, now, s, ws)
	d.mu.Lock()
	d.last, d.lastPt = s, p
	d.mu.Unlock()
	return nil
}

// detectEvents records kills and memory.high hits from the counters'
// changes, and asks for an analysis after a kill, as blueprint section 5.3
// says.
func (d *Daemon) detectEvents(ctx context.Context, now time.Time, s model.Sample, ws map[string]model.Workload) {
	killed := false
	for id, u := range s.Workloads {
		name := ws[id].Label()
		if prev, ok := d.prevKill[id]; ok && u.OOMKills > prev {
			for range min(u.OOMKills-prev, 100) {
				d.event(ctx, now, "oom_kill", name, "the kernel killed a process in this workload for lack of memory")
			}
			killed = true
		}
		if prev, ok := d.prevHigh[id]; ok && u.HighEvents > prev {
			d.event(ctx, now, "limit_high", name, "went over memory.high "+countText(u.HighEvents-prev, "time", "times"))
		}
		d.prevKill[id], d.prevHigh[id] = u.OOMKills, u.HighEvents
	}
	for id := range d.prevKill {
		if _, ok := s.Workloads[id]; !ok {
			delete(d.prevKill, id)
			delete(d.prevHigh, id)
		}
	}
	if d.ramChanged(ctx, now, s.Box.MemTotal) {
		killed = true
	}
	if killed {
		select {
		case d.analyse <- struct{}{}:
		default:
		}
	}
}

// ramChanged records a change of MemTotal, as when the host balloons the
// box's memory, and reports it so the advice is worked out again with the
// new size (blueprint section 3.6).
func (d *Daemon) ramChanged(ctx context.Context, now time.Time, memTotal uint64) bool {
	old := d.facts()
	changed := memTotal > 0 && old.MemTotal > 0 && memTotal != old.MemTotal
	if !changed {
		return false
	}
	d.mu.Lock()
	d.machine.MemTotal = memTotal
	d.mu.Unlock()
	cur := old
	cur.MemTotal = memTotal
	d.recordResize(ctx, now, old, cur)
	d.remember(ctx, cur)
	return true
}

func (d *Daemon) event(ctx context.Context, at time.Time, kind, workload, detail string) {
	if err := d.st.AddEvent(ctx, store.Event{Time: at, Kind: kind, Workload: workload, Detail: detail}); err != nil {
		d.deps.Log.Error("could not record an event", "kind", kind, "err", err)
		return
	}
	d.deps.Log.Info(kind, "workload", workload, "detail", detail)
}
