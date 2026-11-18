package store

import (
	"context"
	"database/sql"
	"time"

	"snugbox.dev/snugbox/agent/internal/model"
)

// Version 6 recorded whole-box rises, including the first sight of a new
// cgroup. Replay only minutes with every raw sample. Rounding timestamps
// to seconds can include a pair the original 60-second window excluded,
// so the replayed maximum can be larger than the recorded one.
func correctLegacyRises(ctx context.Context, tx *sql.Tx) error {
	starts, err := legacyStarts(ctx, tx)
	if err != nil || len(starts) == 0 {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT t, total FROM box_10s ORDER BY t")
	if err != nil {
		return err
	}
	defer rows.Close()
	type minute struct {
		at            int64
		samples       int
		raw, adjusted uint64
	}
	var current minute
	var fixes []minute
	var first time.Time
	var raw, adjusted model.RiseTracker
	for rows.Next() {
		var sec int64
		var total uint64
		if err := rows.Scan(&sec, &total); err != nil {
			return err
		}
		at := time.Unix(sec, 0)
		if first.IsZero() {
			first = at
		}
		bucket := at.Truncate(time.Minute).Unix()
		if current.at != bucket {
			if current.adjusted < current.raw && current.at >= first.Unix()+60 {
				fixes = append(fixes, current)
			}
			current = minute{at: bucket}
		}
		current.samples++
		current.raw = max(current.raw, raw.Add(at, total, 0))
		current.adjusted = max(current.adjusted, adjusted.Add(at, total, starts[sec]))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if current.adjusted < current.raw && current.at >= first.Unix()+60 {
		fixes = append(fixes, current)
	}
	for _, m := range fixes {
		if _, err := tx.ExecContext(ctx, `UPDATE box_1m SET rise_max = ?
			WHERE t = ? AND samples = ? AND rise_max > ? AND rise_max <= ?`, m.adjusted, m.at, m.samples, m.adjusted, m.raw); err != nil {
			return err
		}
	}
	return nil
}

// A known start contributes at least its first measured working set.
// Where its own row expired, subtract the kernel and the hourly maxima
// of every other cgroup that could have been present from the box total.
// This gives a lower bound on the opening footprint even when several
// workloads grew together. Only complete hours can provide that bound.
func legacyStarts(ctx context.Context, tx *sql.Tx) (map[int64]uint64, error) {
	complete, err := completeRiseHours(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `WITH starts(wid, t) AS (
		SELECT id, first_seen FROM workload WHERE first_seen >= (SELECT MIN(t) FROM box_10s)
		UNION SELECT wid, start FROM run WHERE start_seen = 1 AND start >= (SELECT MIN(t) FROM box_10s)
	) SELECT s.t, p.ws, CASE WHEN p.ws IS NULL THEN b.total - b.kother - (
		SELECT COALESCE(SUM(h.ws_max), 0) FROM wl_1h h JOIN workload w ON w.id = h.wid
		WHERE h.t = s.t - s.t % 3600 AND h.wid != s.wid
			AND w.first_seen <= s.t AND w.last_seen > s.t - 60
	) ELSE 0 END
	FROM starts s JOIN box_10s b ON b.t = s.t
	LEFT JOIN wl_10s p ON p.wid = s.wid AND p.t = s.t`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	starts := make(map[int64]uint64)
	for rows.Next() {
		var at int64
		var ws sql.Null[uint64]
		var lower int64
		if err := rows.Scan(&at, &ws, &lower); err != nil {
			return nil, err
		}
		if ws.Valid {
			starts[at] += ws.V
		} else if complete[at-at%3600] && lower > 0 {
			starts[at] += uint64(lower)
		}
	}
	return starts, rows.Err()
}

// An unflushed or partially pruned hour may be missing workload maxima.
// The box and workload sketches are checkpointed in the same transaction.
func completeRiseHours(ctx context.Context, tx *sql.Tx) (map[int64]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.t, h.total,
		(SELECT COUNT(*) FROM box_10s b WHERE b.t >= h.t AND b.t < h.t + 3600),
		(SELECT COALESCE(SUM(samples),0) FROM box_1m m WHERE m.t >= h.t AND m.t < h.t + 3600)
		FROM box_1h h WHERE h.t + 3600 > (SELECT MIN(t) FROM box_10s)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	complete := make(map[int64]bool)
	for rows.Next() {
		var hour, raw, minutes int64
		var data []byte
		if err := rows.Scan(&hour, &data, &raw, &minutes); err != nil {
			return nil, err
		}
		sk := newSketch()
		if err := mergeStored(sk, data); err != nil {
			return nil, err
		}
		complete[hour] = raw > 0 && raw == minutes && float64(raw) == sk.GetCount()
	}
	return complete, rows.Err()
}
