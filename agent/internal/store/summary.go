package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"

	"snugbox.dev/snugbox/agent/internal/model"
)

// Summary builds the memory model's summary of the window from the stored
// minute rows and hourly sketches, and returns the workloads seen in it.
// Percentiles from sketches are upper bounds within SketchAccuracy. Hourly
// sketches cover whole hours, so the window is widened to the hours it
// touches. Call Recorder.Flush first so the current minute and hour count.
// Whole days that are over come from the store's day cache, so only the
// rows of the window's first and last days are read each time.
func (s *Store) Summary(ctx context.Context, from, to time.Time) (model.Summary, map[string]model.Workload, error) {
	var sum model.Summary
	f, t := from.Unix(), to.Unix()
	hourFrom := from.Truncate(time.Hour).Unix()
	first, last := wholeDays(from, to)
	var days *dayData
	if first < last {
		var err error
		if days, err = s.days.get(ctx, s, first, last); err != nil {
			return sum, nil, err
		}
	}

	var m minuteAgg
	var hours map[int64]*wlHours
	var err error
	if days == nil {
		if m, err = s.minuteRange(ctx, f, t); err != nil {
			return sum, nil, err
		}
	} else {
		head, err := s.minuteRange(ctx, f, first-1)
		if err != nil {
			return sum, nil, err
		}
		tail, err := s.minuteRange(ctx, last, t)
		if err != nil {
			return sum, nil, err
		}
		m = head.then(days.minutes).then(tail)
	}
	if m.rows > 0 {
		m.fill(&sum, to)
	}
	if sum.Samples == 0 {
		return sum, map[string]model.Workload{}, nil
	}
	if err := s.boxHours(ctx, &sum, hourFrom, t); err != nil {
		return sum, nil, err
	}
	if days == nil {
		if hours, err = s.hourRange(ctx, hourFrom, t); err != nil {
			return sum, nil, err
		}
	} else {
		if hours, err = s.hourRange(ctx, hourFrom, first-1); err != nil {
			return sum, nil, err
		}
		tail, err := s.hourRange(ctx, last, t)
		if err != nil {
			return sum, nil, err
		}
		// The cached days are folded into fresh sums, never the other way.
		for _, part := range []map[int64]*wlHours{tail, days.hours} {
			for wid, h := range part {
				a := hours[wid]
				if a == nil {
					a = newWLHours()
					hours[wid] = a
				}
				if err := a.add(h); err != nil {
					return sum, nil, fmt.Errorf("store: workload hours: %w", err)
				}
			}
		}
	}
	ws, err := s.workloadSummaries(ctx, &sum, hours, hourFrom, t)
	if err != nil {
		return sum, nil, err
	}
	return sum, ws, nil
}

// fill sets the summary's minute figures. A gap is the time between a
// minute row and the one before it, less the minute itself, where that is
// over three minutes; the pressure figures count only when every minute had
// them.
func (m minuteAgg) fill(sum *model.Summary, to time.Time) {
	sum.Start = time.Unix(m.first, 0).UTC()
	sum.End = time.Unix(m.last, 0).UTC().Add(time.Minute)
	if sum.End.After(to) {
		sum.End = to
	}
	sum.Gaps = time.Duration(m.gaps) * time.Second
	sum.Samples = int(m.samples)
	sum.HardTotalMax, sum.TotalMax, sum.CurrentSumMax = m.hard, m.total, m.cur
	sum.PeakCurrentMax, sum.MaxRise = m.peak, m.rise
	sum.PressureFull = time.Duration(m.psi) * time.Microsecond
	sum.HasPressure = m.hasPSI
	sum.SwapInIntervals, sum.Intervals = int(m.swInt), int(m.intervals)
	sum.SwapInBytes, sum.OOMKills = m.swBytes, m.kills
}

// merged holds the box's sketches merged across the hours of a window.
type merged struct{ total, hot, uncal, kother, cpu *ddsketch.DDSketch }

func (s *Store) boxHours(ctx context.Context, sum *model.Summary, from, to int64) error {
	rows, err := s.db.QueryContext(ctx, boxHoursSQL, from, to)
	if err != nil {
		return fmt.Errorf("store: hours: %w", err)
	}
	defer rows.Close()
	m := merged{newSketch(), newSketch(), newSketch(), newSketch(), newSketch()}
	// RawBytes point into the row, which the merge reads before the next one.
	var total, hot, uncal, kother, cpu sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&total, &hot, &uncal, &kother, &cpu); err != nil {
			return fmt.Errorf("store: hours: %w", err)
		}
		for _, p := range []struct {
			s *ddsketch.DDSketch
			b []byte
		}{{m.total, total}, {m.hot, hot}, {m.uncal, uncal}, {m.kother, kother}, {m.cpu, cpu}} {
			if err := mergeStored(p.s, p.b); err != nil {
				return fmt.Errorf("store: hours: %w", err)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: hours: %w", err)
	}
	sum.TotalQ999 = upperQuantile(m.total, model.NeedQuantile)
	sum.HotFileQ99 = upperQuantile(m.hot, model.HotFileQuantile)
	sum.UncalibratedHotQ99 = upperQuantile(m.uncal, model.HotFileQuantile)
	sum.CPUQ95 = upperQuantileFloat(m.cpu, 0.95)
	sum.KernelOther = model.WorkloadSummary{
		Samples:        int(m.kother.GetCount()),
		WorkingSetQ95:  upperQuantile(m.kother, 0.95),
		WorkingSetQ99:  upperQuantile(m.kother, 0.99),
		WorkingSetQ999: upperQuantile(m.kother, 0.999),
		WorkingSetMax:  upperQuantile(m.kother, 1),
		HardMax:        upperQuantile(m.kother, 1),
	}
	return nil
}

// workloadSummaries turns each workload's summed hours into its summary,
// with its name and settings from the workload table. The hours of a
// workload whose row is gone are left out.
func (s *Store) workloadSummaries(ctx context.Context, sum *model.Summary, hours map[int64]*wlHours, from, to int64) (map[string]model.Workload, error) {
	sum.Workloads = make(map[string]model.WorkloadSummary, len(hours))
	out := make(map[string]model.Workload, len(hours))
	if len(hours) == 0 {
		return out, nil
	}
	// last_seen is written only once a minute. Two successive containers
	// can both appear recent, but only one is in the final raw sample.
	rows, err := s.db.QueryContext(ctx, `SELECT w.id, w.key, w.kind, w.name, w.info, w.peak, w.first_seen, w.last_seen, p.wid IS NOT NULL
		FROM workload w LEFT JOIN wl_10s p ON p.wid = w.id AND p.t = (SELECT MAX(t) FROM box_10s WHERE t BETWEEN ? AND ?)`, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: workload hours: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, firstSeen, lastSeen int64
		var key, kind, name, info string
		var peak *int64
		var present bool
		if err := rows.Scan(&id, &key, &kind, &name, &info, &peak, &firstSeen, &lastSeen, &present); err != nil {
			return nil, fmt.Errorf("store: workload hours: %w", err)
		}
		h := hours[id]
		if h == nil {
			continue
		}
		w, err := decodeWorkload(key, kind, name, info)
		if err != nil {
			return nil, err
		}
		ws := model.WorkloadSummary{
			Samples: int(h.samples), WorkingSetMax: h.wsMax, HardMax: h.hardMax, CurrentMax: h.curMax, ShmemMax: h.shmemMax, SockMax: h.sockMax,
			Pressured: h.pressured, OOMKills: h.kills,
			// An hour without a CPU figure, such as one recorded before
			// version 3, leaves the window's CPU time unknown.
			CPU: time.Duration(h.cpu) * time.Microsecond, HasCPU: h.noCPU == 0,
			// The workload was there from when it was first seen, or the
			// window's start, to when it was last seen, or the window's end.
			Observed: time.Duration(max(min(to, lastSeen)-max(from, firstSeen), 0)) * time.Second,
		}
		if peak != nil && *peak >= 0 {
			ws.Peak, ws.HasPeak = uint64(*peak), true
			ws.PeakAtEnd = present && lastSeen <= to
		}
		ws.WorkingSetQ95 = min(upperQuantile(h.ws, 0.95), ws.WorkingSetMax)
		ws.WorkingSetQ99 = min(upperQuantile(h.ws, 0.99), ws.WorkingSetMax)
		ws.WorkingSetQ999 = min(upperQuantile(h.ws, 0.999), ws.WorkingSetMax)
		ws.HotFileQ99 = upperQuantile(h.hot, model.HotFileQuantile)
		sum.Workloads[key] = ws
		out[key] = w
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: workload hours: %w", err)
	}
	return out, nil
}

// PressureQ99 returns, for each workload key asked for, an upper bound on
// the 99th percentile of the share of each 10-second interval it spent
// stalled on memory between two times, in percent. A workload without
// stored pressure is missing from the answer.
func (s *Store) PressureQ99(ctx context.Context, keys []string, from, to time.Time) (map[string]float64, error) {
	out := make(map[string]float64, len(keys))
	for _, key := range keys {
		sk, err := s.pressureSketch(ctx, key, from, to)
		if err != nil {
			return nil, err
		}
		if !sk.IsEmpty() {
			out[key] = upperQuantileFloat(sk, 0.99)
		}
	}
	return out, nil
}

func (s *Store) pressureSketch(ctx context.Context, key string, from, to time.Time) (*ddsketch.DDSketch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT h.psi FROM wl_1h h JOIN workload w ON w.id = h.wid
		WHERE w.key = ? AND h.t >= ? AND h.t <= ? AND h.psi IS NOT NULL`, key, from.Truncate(time.Hour).Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: pressure: %w", err)
	}
	defer rows.Close()
	sk := newSketch()
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("store: pressure: %w", err)
		}
		if err := mergeStored(sk, b); err != nil {
			return nil, fmt.Errorf("store: pressure: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pressure: %w", err)
	}
	return sk, nil
}

func decodeWorkload(key, kind, name, info string) (model.Workload, error) {
	var wi workloadInfo
	if err := json.Unmarshal([]byte(info), &wi); err != nil {
		return model.Workload{}, fmt.Errorf("store: workload %s: %w", key, err)
	}
	return model.Workload{
		ID: key, Kind: model.Kind(kind), Name: name, Programs: wi.Programs, ContainerID: wi.ContainerID, Compose: wi.Compose,
		Database: wi.Database, Runtime: wi.Runtime, HeapFlags: wi.HeapFlags, Max: wi.Max, High: wi.High,
		Reservation: wi.Reservation, Restarts: wi.Restarts, FromSmaps: wi.FromSmaps,
	}, nil
}

// LivePoint is one 10-second box point.
type LivePoint struct {
	Time      time.Time `json:"time"`
	Hard      uint64    `json:"hard"`
	Total     uint64    `json:"total"`
	Hot       uint64    `json:"hot"`
	MemTotal  uint64    `json:"mem_total"`
	MemAvail  uint64    `json:"mem_available"`
	SwapUsed  uint64    `json:"swap_used"`
	PSISomeUS int64     `json:"psi_some_us"`
	PSIFullUS int64     `json:"psi_full_us"`
}

const recentSQL = "SELECT t, hard, total, hot, mem_total, mem_avail, swap_used, psi_some, psi_full FROM box_10s WHERE t >= ? ORDER BY t"

// Recent returns the box's 10-second points since a time, oldest first.
func (s *Store) Recent(ctx context.Context, since time.Time) ([]LivePoint, error) {
	rows, err := s.db.QueryContext(ctx, recentSQL, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: recent: %w", err)
	}
	defer rows.Close()
	var out []LivePoint
	for rows.Next() {
		var p LivePoint
		var t int64
		if err := rows.Scan(&t, &p.Hard, &p.Total, &p.Hot, &p.MemTotal, &p.MemAvail, &p.SwapUsed, &p.PSISomeUS, &p.PSIFullUS); err != nil {
			return nil, fmt.Errorf("store: recent: %w", err)
		}
		p.Time = time.Unix(t, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// WatchStart returns when watching began, from the oldest minute row.
func (s *Store) WatchStart(ctx context.Context) (time.Time, bool, error) {
	var t *int64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(t) FROM box_1m").Scan(&t); err != nil {
		return time.Time{}, false, fmt.Errorf("store: watch start: %w", err)
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return time.Unix(*t, 0).UTC(), true, nil
}

// WorkloadPoint is one 10-second workload point.
type WorkloadPoint struct {
	Time       time.Time `json:"time"`
	WorkingSet uint64    `json:"working_set"`
	Hard       uint64    `json:"hard"`
	Hot        uint64    `json:"hot"`
	Current    uint64    `json:"current"`
}

// workloadPointsSQL reads the rows in time order from the time index, so
// SQLite needn't sort them; its sorter would take a megabyte for two hours
// of 50 workloads.
const workloadPointsSQL = `SELECT w.key, p.t, p.ws, p.hard, p.hot, p.cur FROM wl_10s p INDEXED BY wl_10s_t JOIN workload w ON w.id = p.wid
	WHERE p.t >= ? ORDER BY p.t`

// WorkloadPoints returns each workload's 10-second points since a time,
// oldest first, keyed by workload ID.
func (s *Store) WorkloadPoints(ctx context.Context, since time.Time) (map[string][]WorkloadPoint, error) {
	rows, err := s.db.QueryContext(ctx, workloadPointsSQL, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: workload points: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]WorkloadPoint)
	for rows.Next() {
		var key string
		var t int64
		var p WorkloadPoint
		if err := rows.Scan(&key, &t, &p.WorkingSet, &p.Hard, &p.Hot, &p.Current); err != nil {
			return nil, fmt.Errorf("store: workload points: %w", err)
		}
		p.Time = time.Unix(t, 0).UTC()
		out[key] = append(out[key], p)
	}
	return out, rows.Err()
}

// MinuteMax is the highest total working set seen in one minute.
type MinuteMax struct {
	Time  time.Time
	Total uint64
	// PressureFull is the box's full stall time in the minute.
	PressureFull time.Duration
}

const minuteMaximaSQL = "SELECT t, total_max, psi_full_us FROM box_1m WHERE t >= ? AND t <= ? ORDER BY t"

// MinuteMaxima returns the minute rows between two times, oldest first.
func (s *Store) MinuteMaxima(ctx context.Context, from, to time.Time) ([]MinuteMax, error) {
	rows, err := s.db.QueryContext(ctx, minuteMaximaSQL, from.Unix(), to.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: minute maxima: %w", err)
	}
	defer rows.Close()
	var out []MinuteMax
	for rows.Next() {
		var t, psi int64
		var m MinuteMax
		if err := rows.Scan(&t, &m.Total, &psi); err != nil {
			return nil, fmt.Errorf("store: minute maxima: %w", err)
		}
		m.Time, m.PressureFull = time.Unix(t, 0).UTC(), time.Duration(psi)*time.Microsecond
		out = append(out, m)
	}
	return out, rows.Err()
}

// Runs returns the runs of services that started at or after since, oldest
// first. A run already going when the watcher started, whose real start is
// unknown, is left out.
func (s *Store) Runs(ctx context.Context, since time.Time) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.key, w.name, r.start, r.end, r.peak FROM run r JOIN workload w ON w.id = r.wid
		WHERE r.start >= ? AND r.start_seen = 1 ORDER BY r.start, w.key`, since.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: runs: %w", err)
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		var run model.Run
		var start, end int64
		if err := rows.Scan(&run.Workload, &run.Name, &start, &end, &run.Peak); err != nil {
			return nil, fmt.Errorf("store: runs: %w", err)
		}
		run.Start, run.End = time.Unix(start, 0).UTC(), time.Unix(end, 0).UTC()
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: runs: %w", err)
	}
	return out, nil
}
