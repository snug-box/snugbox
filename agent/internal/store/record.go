package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"

	"snugbox.dev/snugbox/agent/internal/model"
)

// CheckpointEvery is how often the hour's sketches are merged into the
// database, so a crash loses at most this much of them.
const CheckpointEvery = 10 * time.Minute

type minuteAcc struct {
	t                    int64
	samples              int
	hardMax, totalMax    uint64
	hotMax, curSumMax    uint64
	peakCurMax, riseMax  uint64
	psiFull              time.Duration
	hasPSI               bool
	swapinIntervals      int
	intervals            int
	swapinBytes, oomKill uint64
}

type wlHourAcc struct {
	samples                int
	ws, hot                *ddsketch.DDSketch
	psi                    *ddsketch.DDSketch
	wsMax, hardMax, curMax uint64
	shmemMax, sockMax      uint64
	pressured              bool
	kills                  uint64
	cpu                    time.Duration
	hasCPU                 bool
}

type hourAcc struct {
	t                         int64
	total, hot, uncal, kother *ddsketch.DDSketch
	cpu                       *ddsketch.DDSketch
	wl                        map[string]*wlHourAcc
}

func newHourAcc(t int64) *hourAcc {
	return &hourAcc{t: t, total: newSketch(), hot: newSketch(), uncal: newSketch(), kother: newSketch(), cpu: newSketch(), wl: make(map[string]*wlHourAcc)}
}

// Recorder turns the stream of samples into rows: a box row and workload rows
// for every sample, a minute row as each minute ends, and hourly sketches
// merged into the database every CheckpointEvery. It belongs to one
// goroutine.
type Recorder struct {
	s        *Store
	cpus     int
	pageSize uint64

	wids      map[string]int64
	infos     map[string]string
	touched   map[string]int64
	pressured map[string]bool
	prev      *model.Sample
	rise      model.RiseTracker
	minute    *minuteAcc
	hour      *hourAcc
	lastFlush time.Time
	runs      map[string]*openRun
}

// openRun is a service's current run: from when its cgroup appeared to the
// last sample that saw it, with its highest working set. startSeen is false
// when the run was already going at the first sample after a gap, so its
// real start is unknown.
type openRun struct {
	start, last time.Time
	peak        uint64
	startSeen   bool
}

// RunGap is how long a service may be missing from the samples before its
// run counts as over; a shorter absence is a slow sample, not a new run.
const RunGap = time.Minute

// NewRecorder returns a Recorder for a box with the given CPU count and page
// size.
func (s *Store) NewRecorder(cpus int, pageSize uint64) *Recorder {
	return &Recorder{s: s, cpus: max(cpus, 1), pageSize: pageSize, wids: make(map[string]int64), infos: make(map[string]string), touched: make(map[string]int64), pressured: make(map[string]bool),
		runs: make(map[string]*openRun)}
}

// workloadInfo is the part of a workload kept as JSON in the workload table.
type workloadInfo struct {
	Programs    []string      `json:"programs,omitempty"`
	ContainerID string        `json:"container_id,omitempty"`
	Compose     model.Compose `json:"compose"`
	Database    string        `json:"database,omitempty"`
	Runtime     string        `json:"runtime,omitempty"`
	HeapFlags   []string      `json:"heap_flags,omitempty"`
	Max         model.Limit   `json:"max"`
	High        model.Limit   `json:"high"`
	Reservation model.Limit   `json:"reservation"`
	Restarts    int           `json:"restarts"`
	FromSmaps   bool          `json:"from_smaps,omitempty"`
}

// Add records one sample, its derived point and the workloads it covers.
func (r *Recorder) Add(ctx context.Context, s model.Sample, p model.Point, ws map[string]model.Workload) error {
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := r.add(ctx, tx, s, p, ws); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit sample: %w", err)
	}
	if s.Time.Sub(r.lastFlush) >= CheckpointEvery {
		return r.Flush(ctx)
	}
	return nil
}

func (r *Recorder) add(ctx context.Context, tx *sql.Tx, s model.Sample, p model.Point, ws map[string]model.Workload) error {
	t := s.Time.Unix()
	if err := r.syncWorkloads(ctx, tx, t, s, ws); err != nil {
		return err
	}
	b := s.Box
	swapUsed := uint64(0)
	if b.SwapTotal > b.SwapFree {
		swapUsed = b.SwapTotal - b.SwapFree
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO box_10s (t, hard, total, hot, kother, cur_sum, mem_total, mem_avail, swap_used, psi_some, psi_full)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t, p.HardTotal, p.Total, p.HotFile, p.KernelOther, p.CurrentSum,
		b.MemTotal, b.MemAvailable, swapUsed, b.PressureSome.Microseconds(), b.PressureFull.Microseconds()); err != nil {
		return fmt.Errorf("store: box row: %w", err)
	}
	ins, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO wl_10s (wid, t, ws, hard, hot, cur) VALUES (?, ?, ?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer ins.Close()
	for id, wp := range p.Workloads {
		if _, err := ins.ExecContext(ctx, r.wids[id], t, wp.WorkingSet, wp.Hard, wp.HotFile, wp.Current); err != nil {
			return fmt.Errorf("store: workload row: %w", err)
		}
	}
	if err := r.accumulate(ctx, tx, s, p); err != nil {
		return err
	}
	if err := r.trackRuns(ctx, tx, s, p, ws); err != nil {
		return err
	}
	r.prev = &s
	return nil
}

// trackRuns follows each service's runs. A run is written when it ends and
// at every flush while it lasts, so the history holds when each job ran.
func (r *Recorder) trackRuns(ctx context.Context, tx *sql.Tx, s model.Sample, p model.Point, ws map[string]model.Workload) error {
	for id, wp := range p.Workloads {
		if ws[id].Kind != model.KindService {
			continue
		}
		run := r.runs[id]
		if run != nil && s.Time.Sub(run.last) > RunGap {
			if err := r.writeRun(ctx, tx, id, run); err != nil {
				return err
			}
			run = nil
		}
		if run == nil {
			// A service seen in this sample and not in the one just before
			// it started in between; after a gap in sampling, or at the
			// watcher's first sample, it may have started long before.
			seen := r.prev != nil && s.Time.Sub(r.prev.Time) <= RunGap
			if seen {
				_, before := r.prev.Workloads[id]
				seen = !before
			}
			run = &openRun{start: s.Time, startSeen: seen}
			r.runs[id] = run
		}
		run.last, run.peak = s.Time, max(run.peak, wp.WorkingSet)
	}
	for id, run := range r.runs {
		if _, seen := p.Workloads[id]; !seen && s.Time.Sub(run.last) > RunGap {
			if err := r.writeRun(ctx, tx, id, run); err != nil {
				return err
			}
			delete(r.runs, id)
		}
	}
	return nil
}

func (r *Recorder) writeRun(ctx context.Context, tx *sql.Tx, id string, run *openRun) error {
	wid, ok := r.wids[id]
	if !ok {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO run (wid, start, end, peak, start_seen) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (wid, start) DO UPDATE SET end = MAX(end, excluded.end), peak = MAX(peak, excluded.peak)`,
		wid, run.start.Unix(), run.last.Unix(), run.peak, boolInt(run.startSeen))
	if err != nil {
		return fmt.Errorf("store: run: %w", err)
	}
	return nil
}

// syncWorkloads gives every workload a row, and rewrites it when discovery saw
// it change or a minute has passed, which keeps its peak and last-seen time
// current without a write on every sample.
func (r *Recorder) syncWorkloads(ctx context.Context, tx *sql.Tx, t int64, s model.Sample, ws map[string]model.Workload) error {
	for id := range s.Workloads {
		w := ws[id]
		info, err := json.Marshal(workloadInfo{Programs: w.Programs, ContainerID: w.ContainerID, Compose: w.Compose, Database: w.Database,
			Runtime: w.Runtime, HeapFlags: w.HeapFlags, Max: w.Max, High: w.High, Reservation: w.Reservation, Restarts: w.Restarts, FromSmaps: w.FromSmaps})
		if err != nil {
			return fmt.Errorf("store: workload info: %w", err)
		}
		if _, known := r.wids[id]; known && r.infos[id] == string(info) && t-r.touched[id] < 60 {
			continue
		}
		u := s.Workloads[id]
		var peak any
		if u.HasPeak {
			peak = u.Peak
		}
		var wid int64
		err = tx.QueryRowContext(ctx, `INSERT INTO workload (key, kind, name, info, peak, first_seen, last_seen) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET kind = excluded.kind, name = excluded.name, info = excluded.info,
				peak = COALESCE(excluded.peak, workload.peak), last_seen = excluded.last_seen
			RETURNING id`, id, string(w.Kind), w.Label(), string(info), peak, t, t).Scan(&wid)
		if err != nil {
			return fmt.Errorf("store: workload %s: %w", id, err)
		}
		r.wids[id], r.infos[id], r.touched[id] = wid, string(info), t
	}
	return nil
}

func (r *Recorder) accumulate(ctx context.Context, tx *sql.Tx, s model.Sample, p model.Point) error {
	minute, hour := s.Time.Truncate(time.Minute).Unix(), s.Time.Truncate(time.Hour).Unix()
	if r.minute != nil && r.minute.t != minute {
		if err := r.writeMinute(ctx, tx); err != nil {
			return err
		}
		r.minute = nil
	}
	if r.minute == nil {
		r.minute = &minuteAcc{t: minute}
	}
	if r.hour != nil && r.hour.t != hour {
		if err := r.flushHour(ctx, tx); err != nil {
			return err
		}
		r.hour = nil
	}
	if r.hour == nil {
		r.hour = newHourAcc(hour)
	}

	m, h := r.minute, r.hour
	m.samples++
	m.hardMax = max(m.hardMax, p.HardTotal)
	m.totalMax = max(m.totalMax, p.Total)
	m.hotMax = max(m.hotMax, p.HotFile)
	m.curSumMax = max(m.curSumMax, p.CurrentSum)
	var peaked, uncal uint64
	for id, wp := range p.Workloads {
		if s.Workloads[id].HasPeak {
			peaked += wp.Current
		}
		if wp.Pressured {
			r.pressured[id] = true
		}
		if !r.pressured[id] {
			uncal += wp.HotFile
		}
	}
	m.peakCurMax = max(m.peakCurMax, peaked)
	m.riseMax = max(m.riseMax, r.rise.Add(s.Time, p.Total, p.StartupGrowth))
	m.hasPSI = s.Box.HasPressure

	addValue(h.total, float64(p.Total))
	addValue(h.hot, float64(p.HotFile))
	addValue(h.uncal, float64(uncal))
	addValue(h.kother, float64(p.KernelOther))

	if prev := r.prev; prev != nil {
		a, b := prev.Box, s.Box
		m.intervals++
		if b.SwapIns > a.SwapIns {
			m.swapinIntervals++
			m.swapinBytes += (b.SwapIns - a.SwapIns) * r.pageSize
		}
		if b.HasPressure && a.HasPressure && b.PressureFull >= a.PressureFull {
			m.psiFull += b.PressureFull - a.PressureFull
		}
		m.oomKill += delta(a.OOMKills, b.OOMKills)
		if b.CPUTotal > a.CPUTotal && b.CPUBusy >= a.CPUBusy {
			addValue(h.cpu, float64(b.CPUBusy-a.CPUBusy)/float64(b.CPUTotal-a.CPUTotal)*float64(r.cpus))
		}
	}
	for id, wp := range p.Workloads {
		u := s.Workloads[id]
		w := h.wl[id]
		if w == nil {
			w = &wlHourAcc{ws: newSketch(), hot: newSketch(), psi: newSketch()}
			h.wl[id] = w
		}
		w.samples++
		addValue(w.ws, float64(wp.WorkingSet))
		addValue(w.hot, float64(wp.HotFile))
		w.wsMax = max(w.wsMax, wp.WorkingSet)
		w.hardMax = max(w.hardMax, wp.Hard)
		w.curMax = max(w.curMax, wp.Current)
		w.shmemMax = max(w.shmemMax, u.Shmem)
		w.sockMax = max(w.sockMax, u.Sock)
		w.pressured = w.pressured || wp.Pressured
		if r.prev != nil {
			if pu, ok := r.prev.Workloads[id]; ok {
				w.kills += delta(pu.OOMKills, u.OOMKills)
				if share, ok := PressureShare(pu.PressureSome, u.PressureSome, s.Time.Sub(r.prev.Time)); ok {
					addValue(w.psi, share)
				}
				if pu.HasCPU && u.HasCPU {
					if u.CPU >= pu.CPU {
						w.cpu += u.CPU - pu.CPU
					} else {
						w.cpu += u.CPU
					}
				}
			}
		}
		// A workload's first sample has nothing to measure CPU against, but
		// its hour still has a CPU figure, of zero so far.
		w.hasCPU = w.hasCPU || u.HasCPU
	}
	return nil
}

// PressureShare is the percentage of an interval in which some task of a
// workload stalled on memory, from two readings of its cumulative
// memory.pressure total. A reset counter or an empty interval gives false.
func PressureShare(prev, cur, interval time.Duration) (float64, bool) {
	if interval <= 0 || cur < prev {
		return 0, false
	}
	return min(100*float64(cur-prev)/float64(interval), 100), true
}

func delta(prev, cur uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (r *Recorder) writeMinute(ctx context.Context, tx *sql.Tx) error {
	m := r.minute
	_, err := tx.ExecContext(ctx, `INSERT INTO box_1m (t, samples, hard_max, total_max, hot_max, cur_sum_max, peak_cur_max, rise_max,
			psi_full_us, has_psi, swapin_intervals, intervals, swapin_bytes, oom_kills)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (t) DO UPDATE SET samples = samples + excluded.samples,
			hard_max = MAX(hard_max, excluded.hard_max), total_max = MAX(total_max, excluded.total_max),
			hot_max = MAX(hot_max, excluded.hot_max), cur_sum_max = MAX(cur_sum_max, excluded.cur_sum_max),
			peak_cur_max = MAX(peak_cur_max, excluded.peak_cur_max), rise_max = MAX(rise_max, excluded.rise_max),
			psi_full_us = psi_full_us + excluded.psi_full_us, has_psi = excluded.has_psi,
			swapin_intervals = swapin_intervals + excluded.swapin_intervals, intervals = intervals + excluded.intervals,
			swapin_bytes = swapin_bytes + excluded.swapin_bytes, oom_kills = oom_kills + excluded.oom_kills`,
		m.t, m.samples, m.hardMax, m.totalMax, m.hotMax, m.curSumMax, m.peakCurMax, m.riseMax,
		m.psiFull.Microseconds(), boolInt(m.hasPSI), m.swapinIntervals, m.intervals, m.swapinBytes, m.oomKill)
	if err != nil {
		return fmt.Errorf("store: minute row: %w", err)
	}
	return nil
}

// Flush merges the current hour's sketches and the current minute into the
// database, so a restart keeps them. The watcher calls it on shutdown.
func (r *Recorder) Flush(ctx context.Context) error {
	tx, err := r.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if r.minute != nil {
		if err := r.writeMinute(ctx, tx); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		r.minute = &minuteAcc{t: r.minute.t}
	}
	if r.hour != nil {
		if err := r.flushHour(ctx, tx); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		r.hour = newHourAcc(r.hour.t)
	}
	for id, run := range r.runs {
		if err := r.writeRun(ctx, tx, id, run); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: flush: %w", err)
	}
	if r.prev != nil {
		r.lastFlush = r.prev.Time
	}
	return nil
}

// mergeStored merges the sketch stored in b, when there is one, into s.
func mergeStored(s *ddsketch.DDSketch, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	return s.DecodeAndMergeWith(b)
}

func (r *Recorder) flushHour(ctx context.Context, tx *sql.Tx) error {
	h := r.hour
	var total, hot, uncal, kother, cpu []byte
	err := tx.QueryRowContext(ctx, "SELECT total, hot, uncal, kother, cpu FROM box_1h WHERE t = ?", h.t).Scan(&total, &hot, &uncal, &kother, &cpu)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store: hour row: %w", err)
	}
	for _, m := range []struct {
		s *ddsketch.DDSketch
		b []byte
	}{{h.total, total}, {h.hot, hot}, {h.uncal, uncal}, {h.kother, kother}, {h.cpu, cpu}} {
		if err := mergeStored(m.s, m.b); err != nil {
			return fmt.Errorf("store: merge hour: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO box_1h (t, total, hot, uncal, kother, cpu) VALUES (?, ?, ?, ?, ?, ?)",
		h.t, encodeSketch(h.total), encodeSketch(h.hot), encodeSketch(h.uncal), encodeSketch(h.kother), encodeSketch(h.cpu)); err != nil {
		return fmt.Errorf("store: hour row: %w", err)
	}
	for id, w := range h.wl {
		wid, ok := r.wids[id]
		if !ok {
			continue
		}
		var ws, whot, psi []byte
		var samples int
		var wsMax, hardMax, curMax, shmemMax, sockMax, kills uint64
		var pressured int
		var cpuUS *int64
		err := tx.QueryRowContext(ctx, `SELECT samples, ws, hot, psi, ws_max, hard_max, cur_max, shmem_max, sock_max, pressured, oom_kills, cpu_us
			FROM wl_1h WHERE wid = ? AND t = ?`, wid, h.t).Scan(&samples, &ws, &whot, &psi, &wsMax, &hardMax, &curMax, &shmemMax, &sockMax, &pressured, &kills, &cpuUS)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: workload hour: %w", err)
		}
		for _, m := range []struct {
			s *ddsketch.DDSketch
			b []byte
		}{{w.ws, ws}, {w.hot, whot}, {w.psi, psi}} {
			if err := mergeStored(m.s, m.b); err != nil {
				return fmt.Errorf("store: merge workload hour: %w", err)
			}
		}
		var cpu any
		if cpuUS != nil || w.hasCPU {
			stored := int64(0)
			if cpuUS != nil {
				stored = *cpuUS
			}
			cpu = stored + w.cpu.Microseconds()
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO wl_1h (wid, t, samples, ws, hot, psi, ws_max, hard_max, cur_max, shmem_max, sock_max, pressured, oom_kills, cpu_us)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, wid, h.t, samples+w.samples, encodeSketch(w.ws), encodeSketch(w.hot), encodeSketch(w.psi),
			max(wsMax, w.wsMax), max(hardMax, w.hardMax), max(curMax, w.curMax), max(shmemMax, w.shmemMax), max(sockMax, w.sockMax),
			boolInt(pressured == 1 || w.pressured), kills+w.kills, cpu); err != nil {
			return fmt.Errorf("store: workload hour: %w", err)
		}
	}
	return nil
}
