package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/DataDog/sketches-go/ddsketch"
)

// The watcher sums 30 days of minute rows and hourly sketches every 5
// minutes, and all but the first and last day of them are over and can't
// change. dayCache keeps those days' sums and merged sketches, so a summary
// reads only the rows of the two days at the window's ends, and a full read
// happens once a day, when the window moves on by a day.

// The summary's reads. Each reads its rows in an order a table's key or an
// index gives, so none needs SQLite's sorter or a temporary table, which
// take a megabyte or more on a full store and stay with the C allocator
// under SQLite once freed (TestWindowReadsNeedNoSort).
const (
	minuteSumsSQL = `SELECT COUNT(*), MIN(t), MAX(t), SUM(samples), MAX(hard_max), MAX(total_max), MAX(cur_sum_max),
			MAX(peak_cur_max), MAX(rise_max), SUM(psi_full_us), MIN(has_psi), SUM(swapin_intervals), SUM(intervals), SUM(swapin_bytes),
			SUM(oom_kills)
		FROM box_1m WHERE t >= ? AND t <= ?`
	minuteTimesSQL   = "SELECT t FROM box_1m WHERE t >= ? AND t <= ? ORDER BY t"
	workloadHoursSQL = `SELECT wid, samples, ws_max, hard_max, cur_max, shmem_max, sock_max, pressured, oom_kills, cpu_us, ws, hot
		FROM wl_1h INDEXED BY wl_1h_t WHERE t >= ? AND t <= ?`
	boxHoursSQL = "SELECT total, hot, uncal, kother, cpu FROM box_1h WHERE t >= ? AND t <= ?"
)

// minuteAgg sums a run of minute rows. gaps is the time inside the run that
// has no rows, in seconds: for each row, the time since the one before it
// less the minute itself, where that is over three minutes.
type minuteAgg struct {
	rows, first, last, samples   int64
	hard, total, cur, peak, rise uint64
	psi, swInt, intervals, gaps  int64
	swBytes, kills               uint64
	hasPSI                       bool
}

// then joins a run of minute rows to the run that follows it.
func (a minuteAgg) then(b minuteAgg) minuteAgg {
	switch {
	case a.rows == 0:
		return b
	case b.rows == 0:
		return a
	}
	gaps := a.gaps + b.gaps
	if d := b.first - a.last; d > 180 {
		gaps += d - 60
	}
	return minuteAgg{
		rows: a.rows + b.rows, first: a.first, last: b.last, samples: a.samples + b.samples,
		hard: max(a.hard, b.hard), total: max(a.total, b.total), cur: max(a.cur, b.cur), peak: max(a.peak, b.peak), rise: max(a.rise, b.rise),
		psi: a.psi + b.psi, swInt: a.swInt + b.swInt, intervals: a.intervals + b.intervals, gaps: gaps,
		swBytes: a.swBytes + b.swBytes, kills: a.kills + b.kills, hasPSI: a.hasPSI && b.hasPSI,
	}
}

// minuteRange sums the minute rows from from to to, both included.
func (s *Store) minuteRange(ctx context.Context, from, to int64) (minuteAgg, error) {
	var m minuteAgg
	if from > to {
		return m, nil
	}
	var rows, first, last, samples, hasPSI, psi, swInt, intervals sql.NullInt64
	var hard, total, cur, peak, rise, swBytes, kills sql.Null[uint64]
	err := s.db.QueryRowContext(ctx, minuteSumsSQL, from, to).Scan(
		&rows, &first, &last, &samples, &hard, &total, &cur, &peak, &rise, &psi, &hasPSI, &swInt, &intervals, &swBytes, &kills)
	if err != nil {
		return m, fmt.Errorf("store: minutes: %w", err)
	}
	if rows.Int64 == 0 {
		return m, nil
	}
	gaps, err := s.minuteGaps(ctx, from, to)
	if err != nil {
		return m, err
	}
	return minuteAgg{
		rows: rows.Int64, first: first.Int64, last: last.Int64, samples: samples.Int64,
		hard: hard.V, total: total.V, cur: cur.V, peak: peak.V, rise: rise.V,
		psi: psi.Int64, swInt: swInt.Int64, intervals: intervals.Int64, gaps: gaps,
		swBytes: swBytes.V, kills: kills.V, hasPSI: hasPSI.Int64 == 1,
	}, nil
}

// minuteGaps sums the gaps between the minute rows from from to to, from
// their times in order. SQL's LAG could sum them, but a window function
// keeps every row of the range in a temporary table.
func (s *Store) minuteGaps(ctx context.Context, from, to int64) (int64, error) {
	rows, err := s.db.QueryContext(ctx, minuteTimesSQL, from, to)
	if err != nil {
		return 0, fmt.Errorf("store: gaps: %w", err)
	}
	defer rows.Close()
	var gaps, prev, t int64
	for n := 0; rows.Next(); n++ {
		if err := rows.Scan(&t); err != nil {
			return 0, fmt.Errorf("store: gaps: %w", err)
		}
		if d := t - prev; n > 0 && d > 180 {
			gaps += d - 60
		}
		prev = t
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: gaps: %w", err)
	}
	return gaps, nil
}

// wlHours sums one workload's hourly rows and merges their sketches.
type wlHours struct {
	samples                                   int64
	wsMax, hardMax, curMax, shmemMax, sockMax uint64
	kills                                     uint64
	pressured                                 bool
	// cpu is in microseconds, from the hours that have it; noCPU counts
	// the hours that don't.
	cpu, noCPU int64
	ws, hot    *ddsketch.DDSketch
}

func newWLHours() *wlHours { return &wlHours{ws: newSketch(), hot: newSketch()} }

// add folds b into a, reading b's sketches without changing them.
func (a *wlHours) add(b *wlHours) error {
	a.samples += b.samples
	a.wsMax, a.hardMax, a.curMax = max(a.wsMax, b.wsMax), max(a.hardMax, b.hardMax), max(a.curMax, b.curMax)
	a.shmemMax, a.sockMax = max(a.shmemMax, b.shmemMax), max(a.sockMax, b.sockMax)
	a.kills += b.kills
	a.pressured = a.pressured || b.pressured
	a.cpu, a.noCPU = a.cpu+b.cpu, a.noCPU+b.noCPU
	if err := a.ws.MergeWith(b.ws); err != nil {
		return err
	}
	return a.hot.MergeWith(b.hot)
}

// hourRange sums each workload's hourly rows that start from from to to,
// both included, keyed by workload id. The rows come one by one in time
// order and are summed here, as GROUP BY wid would have SQLite sort them.
func (s *Store) hourRange(ctx context.Context, from, to int64) (map[int64]*wlHours, error) {
	out := make(map[int64]*wlHours)
	if from > to {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, workloadHoursSQL, from, to)
	if err != nil {
		return nil, fmt.Errorf("store: workload hours: %w", err)
	}
	defer rows.Close()
	var wid, samples, wsMax, hardMax, curMax, shmemMax, sockMax, pressured, kills int64
	var cpu sql.NullInt64
	// RawBytes point into the row, which the merge reads before the next one.
	var ws, hot sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&wid, &samples, &wsMax, &hardMax, &curMax, &shmemMax, &sockMax, &pressured, &kills, &cpu, &ws, &hot); err != nil {
			return nil, fmt.Errorf("store: workload hours: %w", err)
		}
		h := out[wid]
		if h == nil {
			h = newWLHours()
			out[wid] = h
		}
		h.samples += samples
		h.wsMax, h.hardMax, h.curMax = max(h.wsMax, unsigned(wsMax)), max(h.hardMax, unsigned(hardMax)), max(h.curMax, unsigned(curMax))
		h.shmemMax, h.sockMax = max(h.shmemMax, unsigned(shmemMax)), max(h.sockMax, unsigned(sockMax))
		h.kills += unsigned(kills)
		h.pressured = h.pressured || pressured == 1
		if cpu.Valid && cpu.Int64 >= 0 {
			h.cpu += cpu.Int64
		} else {
			h.noCPU++
		}
		if err := mergeStored(h.ws, ws); err != nil {
			return nil, fmt.Errorf("store: workload hours: %w", err)
		}
		if err := mergeStored(h.hot, hot); err != nil {
			return nil, fmt.Errorf("store: workload hours: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: workload hours: %w", err)
	}
	return out, nil
}

// unsigned turns a stored size or count back into the unsigned value it was
// written from; SQLite keeps every integer signed. Scanning straight into a
// uint64 would format each value as text and parse it again.
func unsigned(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// wholeDays returns the UTC days wholly inside a window that ended at least
// an hour before it does, by when the watcher has written their last hour,
// as [first, last) in Unix seconds; first equals last when there are none.
func wholeDays(from, to time.Time) (first, last int64) {
	start := from.UTC().Truncate(24 * time.Hour)
	if start.Before(from) {
		start = start.Add(24 * time.Hour)
	}
	end := to.Add(-time.Hour).UTC().Truncate(24 * time.Hour)
	if !end.After(start) {
		return 0, 0
	}
	return start.Unix(), end.Unix()
}

// dayCache holds the sums of one run of whole days. Its zero value is empty.
type dayCache struct {
	mu          sync.Mutex
	first, last int64
	data        *dayData
}

type dayData struct {
	minutes minuteAgg
	hours   map[int64]*wlHours
}

// get returns the sums of the days [first, last), reading them unless they
// are the days it holds already.
func (c *dayCache) get(ctx context.Context, s *Store, first, last int64) (*dayData, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data != nil && c.first == first && c.last == last {
		return c.data, nil
	}
	m, err := s.minuteRange(ctx, first, last-1)
	if err != nil {
		return nil, err
	}
	h, err := s.hourRange(ctx, first, last-1)
	if err != nil {
		return nil, err
	}
	c.first, c.last, c.data = first, last, &dayData{minutes: m, hours: h}
	return c.data, nil
}

func (c *dayCache) clear() {
	c.mu.Lock()
	c.data = nil
	c.mu.Unlock()
}
